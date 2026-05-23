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
