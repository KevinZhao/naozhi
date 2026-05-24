package cron

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"time"
	"unicode/utf8"
)

// maxCronStoreBytes caps the size of cron_jobs.json during Load. The realistic
// worst case is maxJobsHardCap (500) × per-job payload (~8 KiB prompt + ~4 KiB
// truncated LastResult + metadata) ≈ 6.5 MiB; 16 MiB leaves headroom for future
// fields without risking unbounded memory use on a tampered file. When the
// store exceeds this cap loadJobs returns an error and leaves the file in
// place — callers (Scheduler.Start) must abort rather than continue with an
// empty in-memory state that would be persisted back and silently clobber the
// operator's real jobs.
const maxCronStoreBytes = 16 * 1024 * 1024

// loadJobs reads and parses the on-disk cron job store. The three possible
// outcomes are distinguished deliberately:
//
//   - (map, nil): normal read, including "file does not exist" (fresh
//     deployment). Returned map may be nil for the no-file case — callers
//     treat that identically to empty.
//   - (nil, nil): parse failed. The corrupt file has already been renamed to
//     <path>.corrupt.<ts>, so starting from empty state will not destroy
//     evidence and the next save is safe.
//   - (nil, error): the original file is still on disk (size cap exceeded,
//     I/O error, or the corrupt-rename itself failed). Callers MUST abort:
//     continuing with empty state would cause the next persist to clobber the
//     operator's real jobs with `[]`, silently losing data.
func loadJobs(path string) (map[string]*Job, error) {
	if path == "" {
		return nil, nil
	}
	// R236-SEC-01 (CWE-59): refuse to follow a symlink at the cron store
	// path. A local attacker who can write the data dir could otherwise
	// replace cron_jobs.json with a symlink to a sensitive file (whose
	// contents would be parsed and any parse failure would rename the
	// linked file out of place via the corrupt-rename branch). os.Lstat
	// inspects the link itself rather than the target.
	if fi, lerr := os.Lstat(path); lerr == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			slog.Warn("cron store path is a symlink; refusing to follow", "path", path)
			return nil, fmt.Errorf("cron store path is a symlink, refusing to follow")
		}
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		// Drop path from wrapped error; keep full path in log for operator.
		slog.Warn("open cron store failed", "path", path, "err", err)
		return nil, fmt.Errorf("open cron store: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxCronStoreBytes+1))
	if err != nil {
		slog.Warn("read cron store failed", "path", path, "err", err)
		return nil, fmt.Errorf("read cron store: %w", err)
	}
	if int64(len(data)) > maxCronStoreBytes {
		// We read cap+1 via the LimitReader, so we can only report "at least"
		// the true size. Leave the original in place so operators can
		// inspect and recover; returning an error forces Scheduler.Start
		// to abort rather than overwrite the file with an empty array on
		// the next save. Strip the absolute path from the user-facing
		// error — it may propagate to upstream/dashboard and leak host
		// filesystem layout. Full path goes to the warn log for operators.
		slog.Warn("cron store exceeds size cap",
			"path", path, "size", len(data), "cap", maxCronStoreBytes)
		return nil, fmt.Errorf("cron store exceeds size cap (at least %d bytes, cap=%d bytes); refusing to load — inspect the file or move it aside before restarting",
			len(data), maxCronStoreBytes)
	}

	var entries []*Job
	if err := json.Unmarshal(data, &entries); err != nil {
		// Preserve the corrupt file so the next save does not silently
		// overwrite operator-visible evidence. Mirrors session/store.go.
		// If the rename itself fails we return an error (the original file
		// is still on disk and an empty save would destroy it).
		// Append a cryptographic nonce to the rename target so two naozhi
		// instances mis-configured to share the same data dir don't pick
		// the same corruptPath at the same second and silently overwrite
		// each other's evidence file — rename collision would leave one
		// instance starting with empty in-memory state.
		corruptPath := path + ".corrupt." + time.Now().UTC().Format("20060102-150405") + "." + randomNonce()
		if renameErr := os.Rename(path, corruptPath); renameErr != nil {
			return nil, fmt.Errorf("parse cron store failed (%v); could not rename: %w",
				err, renameErr)
		}
		slog.Warn("parse cron store failed; corrupt file preserved",
			"err", err, "path", path, "corrupt_path", corruptPath)
		return nil, nil
	}

	// Defensive cap: even if the file fits within maxCronStoreBytes, an
	// attacker / corrupted shrinker could craft an array with hundreds of
	// thousands of zero-byte stub entries. The scheduler enforces
	// maxJobsHardCap at AddJob time but loadJobs runs before the scheduler
	// rejects them, so we'd allocate the map and walk every entry first.
	// Refuse to load anything beyond the hard cap rather than partially
	// loading and letting the scheduler silently truncate.
	if len(entries) > maxJobsHardCap {
		slog.Warn("cron store exceeds job count cap",
			"path", path, "count", len(entries), "cap", maxJobsHardCap)
		return nil, fmt.Errorf("cron store contains %d jobs (cap=%d); refusing to load — inspect the file or trim it before restarting",
			len(entries), maxJobsHardCap)
	}

	m := make(map[string]*Job, len(entries))
	for _, j := range entries {
		if j.ID == "" {
			continue
		}
		// R235-CR-12: enforce ID hex shape on load. AddJob always produces
		// IsValidID-conformant IDs (generateID), so a non-conformant ID came
		// either from a hand-edited cron_jobs.json or from an attacker writing
		// the file directly. runStore.Append rejects the same job at runtime
		// (slog.Warn + return), but the entry would otherwise sit in s.jobs
		// forever and round-trip back to disk on every persist.
		if !IsValidID(j.ID) {
			slog.Warn("cron store: dropping job with invalid ID",
				"path", path, "cron_id_bytes", len(j.ID))
			continue
		}
		// R234-SEC-12: defensive prompt validation. AddJob / dashboard PATCH
		// already enforce validateCronPrompt (UTF-8 + no C0 controls except
		// \t/\n/\r), but cron_jobs.json can be edited directly by an
		// operator. An invalid-UTF-8 prompt would corrupt every later
		// json.Marshal (round-trip writes U+FFFD silently); a control-byte
		// payload would smuggle ANSI / log-injection sequences into every
		// CronRun.Result and dashboard broadcast. Drop offenders rather
		// than aborting the whole load — losing a single tampered job is
		// strictly safer than refusing to start the scheduler.
		if !utf8.ValidString(j.Prompt) || containsCronC0(j.Prompt) {
			slog.Warn("cron store: dropping job with invalid prompt bytes",
				"path", path, "cron_id", j.ID, "prompt_bytes", len(j.Prompt))
			continue
		}
		// R235-CR-5: same defensive rationale for Title / Backend. AddJob /
		// dashboard PATCH validate these before accepting; an attacker
		// hand-editing the JSON could smuggle bidi / control bytes into
		// dashboard responses and platform notifications via Job.Title or
		// Job.Backend, so drop offenders here as well.
		if !utf8.ValidString(j.Title) || containsCronC0(j.Title) {
			slog.Warn("cron store: dropping job with invalid title bytes",
				"path", path, "cron_id", j.ID, "title_bytes", len(j.Title))
			continue
		}
		if !utf8.ValidString(j.Backend) || containsCronC0(j.Backend) {
			slog.Warn("cron store: dropping job with invalid backend bytes",
				"path", path, "cron_id", j.ID, "backend_bytes", len(j.Backend))
			continue
		}
		// R236-QA-16: 与 prompt/title 同等防御 — Schedule 与 WorkDir 也是
		// 启动期 slog 字段，AddJob/dashboard PATCH 已校验，但持久化文件
		// 可被运维直接编辑。超长 Schedule（robfig/cron 实际表达式 < 64 B）
		// 或非 UTF-8 / 控制字符的 WorkDir 进 slog 会污染日志（log injection）
		// 并在 broadcast 时跨终端跑空。len 阈值参照 MaxScheduleBytes 与
		// 4096（PATH_MAX 兼容值）。
		if len(j.Schedule) > MaxScheduleBytes || !utf8.ValidString(j.Schedule) || containsCronC0(j.Schedule) {
			slog.Warn("cron store: dropping job with invalid schedule bytes",
				"path", path, "cron_id", j.ID, "schedule_bytes", len(j.Schedule))
			continue
		}
		if len(j.WorkDir) > maxWorkDirBytes || !utf8.ValidString(j.WorkDir) || containsCronC0(j.WorkDir) {
			slog.Warn("cron store: dropping job with invalid work_dir bytes",
				"path", path, "cron_id", j.ID, "work_dir_bytes", len(j.WorkDir))
			continue
		}
		m[j.ID] = j
	}
	slog.Info("loaded cron store", "count", len(m), "path", path)
	return m, nil
}

// containsCronC0 reports whether s contains any C0 control byte that
// validateCronPrompt rejects on the IM / dashboard write paths. \t (0x09),
// \n (0x0A), \r (0x0D) are explicitly allowed; everything else in 0x00-0x1F
// plus 0x7F (DEL) trips the guard. Inlined byte scan rather than the
// textutil regex helper because loadJobs runs once at startup over a small
// file and importing textutil would pull in regexp init cost on every
// scheduler boot. R234-SEC-12.
func containsCronC0(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		if b < 0x20 || b == 0x7F {
			return true
		}
	}
	return false
}

// randomNonce returns a short hex-encoded random string for distinguishing
// otherwise-identical timestamped paths. Falls back to a time-derived
// suffix if crypto/rand is unavailable (never expected on Linux).
func randomNonce() string {
	var rb [4]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xFFFFFFFF)
	}
	return hex.EncodeToString(rb[:])
}
