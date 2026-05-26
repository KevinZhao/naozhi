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
	if fi, lerr := os.Lstat(path); lerr != nil {
		// R246-SEC-12: previously we logged non-NotExist Lstat errors
		// (EPERM, EACCES, ELOOP, …) and fell through to os.Open. That
		// defeated the symlink check entirely — a local attacker who can
		// arrange for Lstat to fail (e.g. by removing search permission on
		// an ancestor directory but keeping it on the file via a different
		// path) could still get os.Open to traverse a symlink. Treat any
		// non-NotExist lstat failure as a hard error: the file exists in
		// some form and we cannot prove it is not a symlink. ErrNotExist
		// remains the "no file = empty jobs" path.
		if !errors.Is(lerr, fs.ErrNotExist) {
			slog.Warn("cron: lstat store path failed; refusing to load",
				"path", path, "err", lerr)
			return nil, fmt.Errorf("cron: lstat %s: %w", path, lerr)
		}
	} else if fi.Mode()&os.ModeSymlink != 0 {
		slog.Warn("cron store path is a symlink; refusing to follow", "path", path)
		return nil, fmt.Errorf("cron store path is a symlink, refusing to follow")
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
		// R20260526-CR-001: guard against nil entries in the JSON array.
		// json.Unmarshal of `[null, {...}]` into []*Job yields a slice whose
		// first element is a nil *Job. Without this check, j.ID below would
		// panic (NPE) the moment a tampered or hand-edited cron_jobs.json
		// is loaded. Drop the nil entry and keep loading the rest.
		if j == nil {
			continue
		}
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
		if !utf8.ValidString(j.Prompt) || containsCronUnsafe(j.Prompt) {
			slog.Warn("cron store: dropping job with invalid prompt bytes",
				"path", path, "cron_id", j.ID, "prompt_bytes", len(j.Prompt))
			continue
		}
		// R235-CR-5: same defensive rationale for Title / Backend. AddJob /
		// dashboard PATCH validate these before accepting; an attacker
		// hand-editing the JSON could smuggle bidi / control bytes into
		// dashboard responses and platform notifications via Job.Title or
		// Job.Backend, so drop offenders here as well.
		if !utf8.ValidString(j.Title) || containsCronUnsafe(j.Title) {
			slog.Warn("cron store: dropping job with invalid title bytes",
				"path", path, "cron_id", j.ID, "title_bytes", len(j.Title))
			continue
		}
		if !utf8.ValidString(j.Backend) || containsCronUnsafe(j.Backend) {
			slog.Warn("cron store: dropping job with invalid backend bytes",
				"path", path, "cron_id", j.ID, "backend_bytes", len(j.Backend))
			continue
		}
		// R236-QA-16: defensive Schedule / WorkDir validation. AddJob /
		// dashboard PATCH already validate Schedule via robfig/cron + the
		// minCronInterval floor, and reject WorkDir paths that escape the
		// configured root, but cron_jobs.json can be hand-edited offline.
		// A non-UTF-8 or control-byte WorkDir would smuggle ANSI / log-
		// injection sequences into every "could not chdir" slog line; an
		// over-long Schedule would propagate into dashboard responses and
		// metrics labels. Length caps mirror MaxScheduleBytes (256) and
		// the per-Job WorkDir budget below — runtime semantics are not
		// affected because Scheduler.registerJob re-validates the schedule
		// before adding it to robfig/cron.
		if len(j.Schedule) > MaxScheduleBytes || !utf8.ValidString(j.Schedule) || containsCronUnsafe(j.Schedule) {
			slog.Warn("cron store: dropping job with invalid schedule bytes",
				"path", path, "cron_id", j.ID, "schedule_bytes", len(j.Schedule))
			continue
		}
		// 4 KiB matches the de-facto Linux PATH_MAX × small slack; longer
		// values cannot legitimately reach a real filesystem.
		if len(j.WorkDir) > 4096 || !utf8.ValidString(j.WorkDir) || containsCronUnsafe(j.WorkDir) {
			slog.Warn("cron store: dropping job with invalid work_dir bytes",
				"path", path, "cron_id", j.ID, "work_dir_bytes", len(j.WorkDir))
			continue
		}
		// R239-CR-2 / R236-QA-16: same defensive rationale for NotifyChatID /
		// NotifyPlatform. Both fields ride the cronJobView struct and are
		// broadcast to dashboard at 1Hz via /api/cron; an attacker hand-editing
		// cron_jobs.json could smuggle bidi / control bytes into the dashboard
		// payload that way. AddJob / dashboard PATCH validate these on the
		// write path; this is the equivalent guard for the load path.
		if !utf8.ValidString(j.NotifyChatID) || containsCronUnsafe(j.NotifyChatID) {
			slog.Warn("cron store: dropping job with invalid notify_chat_id bytes",
				"path", path, "cron_id", j.ID, "chat_id_bytes", len(j.NotifyChatID))
			continue
		}
		if !utf8.ValidString(j.NotifyPlatform) || containsCronUnsafe(j.NotifyPlatform) {
			slog.Warn("cron store: dropping job with invalid notify_platform bytes",
				"path", path, "cron_id", j.ID, "platform_bytes", len(j.NotifyPlatform))
			continue
		}
		m[j.ID] = j
	}
	slog.Info("loaded cron store", "count", len(m), "path", path)
	return m, nil
}

// containsCronUnsafe reports whether s contains any byte sequence that the
// cron field-safety audit rejects: disallowed C0 control bytes, the DEL
// byte, Unicode bidi overrides, or line-/paragraph-separator codepoints.
// Together these are the codepoints that validateCronPrompt blocks on the
// IM / dashboard write paths and that a hand-edited cron_jobs.json could
// otherwise smuggle into IM notifications and dashboard responses.
//
// C0 policy: \t (0x09), \n (0x0A), \r (0x0D) are explicitly allowed;
// everything else in 0x00-0x1F plus 0x7F (DEL) trips the guard.
//
// Bidi / separator policy (R236-SEC-07): U+202A..U+202E (LRE/RLE/PDF/
// LRO/RLO) and U+2066..U+2069 (LRI/RLI/FSI/PDI) can visually reorder
// surrounding glyphs in any IM / browser renderer, so a tampered prompt
// could swap "rm -rf /tmp/safe" into "rm -rf /etc/passwd" at display
// time without changing the bytes on the wire. U+2028 (LS) and U+2029
// (PS) introduce hard line breaks the prompt sanitiser otherwise
// accepts. All eight codepoints encode as 3-byte UTF-8 sequences in the
// E2 80 / E2 81 prefix range, so we only decode when the first two
// bytes match — keeps the common ASCII-only path branchless.
//
// Inlined byte scan rather than the textutil regex helper because
// loadJobs runs once at startup over a small file and importing textutil
// would pull in regexp init cost on every scheduler boot. R234-SEC-12.
func containsCronUnsafe(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		if b < 0x20 || b == 0x7F {
			return true
		}
		// Detect bidi / LS / PS via direct UTF-8 byte inspection. The
		// guarded codepoints all live in U+2028..U+2029 and
		// U+202A..U+202E (E2 80 A8..AE) and U+2066..U+2069
		// (E2 81 A6..A9), so peek when we see the E2 80 / E2 81 prefix.
		if b == 0xE2 && i+2 < len(s) {
			b1 := s[i+1]
			b2 := s[i+2]
			if b1 == 0x80 && b2 >= 0xA8 && b2 <= 0xAE {
				return true
			}
			if b1 == 0x81 && b2 >= 0xA6 && b2 <= 0xA9 {
				return true
			}
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
