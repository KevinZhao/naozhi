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

// maxCronStoreBytes caps the size of cron_jobs.json during Load: worst case is
// maxJobsHardCap × ~12 KiB per job ≈ 6.5 MiB, so 16 MiB leaves headroom while
// bounding memory on a tampered file. Over the cap, loadJobs returns an error
// and leaves the file in place so Scheduler.Start aborts instead of persisting
// an empty state over the operator's real jobs.
const maxCronStoreBytes = 16 * 1024 * 1024

// loadJobs reads and parses the on-disk cron job store. Outcomes:
//
//   - (map, nil): normal read; a missing file yields a nil map (= empty).
//   - (nil, nil): parse failed; the corrupt file was renamed to
//     <path>.corrupt.<ts> so starting empty destroys no evidence.
//   - (nil, error): the original file is still on disk (size cap, I/O error,
//     or the corrupt-rename failed). Callers MUST abort: continuing with empty
//     state would make the next persist clobber the real jobs with `[]`.
func loadJobs(path string) (map[string]*Job, error) {
	if path == "" {
		return nil, nil
	}
	// OpenFile(O_NOFOLLOW|O_CLOEXEC) refuses a symlinked cron_jobs.json atomically
	// (no Lstat→Open TOCTOU, #829); Fstat below validates the open inode is a
	// regular file so a fifo/socket never reaches json.Unmarshal + os.Rename.
	f, err := openCronStoreFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if isSymlinkLoopErr(err) {
			slog.Warn("cron store path is a symlink; refusing to follow", "path", path)
			return nil, fmt.Errorf("cron store path is a symlink, refusing to follow")
		}
		// Any non-NotExist open failure aborts the load: continuing could read
		// attacker-substituted bytes or succeed with an empty []*Job that the next
		// persist writes back over the real jobs (#469).
		slog.Warn("open cron store failed", "path", path, "err", err)
		return nil, fmt.Errorf("open cron store: %w", err)
	}
	defer f.Close()
	// Fstat on the open fd: O_NOFOLLOW filtered symlinks, but a fifo/socket/device
	// only fails here.
	fi, err := f.Stat()
	if err != nil {
		slog.Warn("cron: fstat store path failed; refusing to load",
			"path", path, "err", err)
		return nil, fmt.Errorf("cron: fstat %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		slog.Warn("cron store path is not a regular file; refusing to load",
			"path", path, "mode", fi.Mode().String())
		return nil, fmt.Errorf("cron store path is not a regular file, refusing to load")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCronStoreBytes+1))
	if err != nil {
		slog.Warn("read cron store failed", "path", path, "err", err)
		return nil, fmt.Errorf("read cron store: %w", err)
	}
	if int64(len(data)) > maxCronStoreBytes {
		// LimitReader read cap+1, so only "at least" is known. Leave the original in
		// place and return an error so Start aborts. The absolute path is kept out of
		// the user-facing error (may propagate to dashboard); it goes to the log.
		slog.Warn("cron store exceeds size cap",
			"path", path, "size", len(data), "cap", maxCronStoreBytes)
		return nil, fmt.Errorf("cron store exceeds size cap (at least %d bytes, cap=%d bytes); refusing to load — inspect the file or move it aside before restarting",
			len(data), maxCronStoreBytes)
	}

	var entries []*Job
	if err := json.Unmarshal(data, &entries); err != nil {
		// Preserve the corrupt file so the next save does not overwrite evidence; if
		// the rename fails return an error (an empty save would destroy the original).
		// The random nonce keeps two instances sharing one data dir from colliding on
		// the same corruptPath within a second.
		corruptPath := path + ".corrupt." + time.Now().UTC().Format("20060102-150405") + "." + randomNonce()
		if renameErr := os.Rename(path, corruptPath); renameErr != nil {
			return nil, fmt.Errorf("parse cron store failed (%v); could not rename: %w",
				err, renameErr)
		}
		slog.Warn("parse cron store failed; corrupt file preserved",
			"err", err, "path", path, "corrupt_path", corruptPath)
		return nil, nil
	}

	// Defensive cap: an array of hundreds of thousands of stub entries fits under
	// maxCronStoreBytes; refuse rather than allocate the map and let the
	// scheduler silently truncate at maxJobsHardCap.
	if len(entries) > maxJobsHardCap {
		slog.Warn("cron store exceeds job count cap",
			"path", path, "count", len(entries), "cap", maxJobsHardCap)
		return nil, fmt.Errorf("cron store contains %d jobs (cap=%d); refusing to load — inspect the file or trim it before restarting",
			len(entries), maxJobsHardCap)
	}

	m := make(map[string]*Job, len(entries))
	for _, j := range entries {
		// json.Unmarshal of `[null, {...}]` yields nil *Job entries; drop them
		// instead of panicking.
		if j == nil {
			continue
		}
		if j.ID == "" {
			continue
		}
		// A non-conformant ID can only come from a hand-edited or attacker-written
		// file; runStore.Append would reject it at runtime, but it would otherwise sit
		// in s.jobs forever and round-trip to disk on every persist.
		if !IsValidID(j.ID) {
			slog.Warn("cron store: dropping job with invalid ID",
				"path", path, "cron_id_bytes", len(j.ID))
			continue
		}
		// Defensive prompt validation (the write paths already enforce
		// validateCronPrompt, but cron_jobs.json can be edited offline): invalid UTF-8
		// corrupts every later json.Marshal and control bytes smuggle ANSI / log
		// injection into every CronRun.Result. Drop the job rather than abort the load.
		if !utf8.ValidString(j.Prompt) || containsCronUnsafe(j.Prompt) {
			slog.Warn("cron store: dropping job with invalid prompt bytes",
				"path", path, "cron_id", j.ID, "prompt_bytes", len(j.Prompt))
			continue
		}
		// Mirror the write path's MaxPromptBytes cap so a hand-edited runaway prompt
		// is not replayed every run.
		if len(j.Prompt) > MaxPromptBytes {
			slog.Warn("cron store: dropping job with overlong prompt",
				"path", path, "cron_id", j.ID,
				"prompt_bytes", len(j.Prompt), "cap", MaxPromptBytes)
			continue
		}
		// Same defensive rationale for Title / Backend: bidi / control bytes would
		// reach dashboard responses and notifications.
		if !utf8.ValidString(j.Title) || containsCronUnsafe(j.Title) {
			slog.Warn("cron store: dropping job with invalid title bytes",
				"path", path, "cron_id", j.ID, "title_bytes", len(j.Title))
			continue
		}
		// Rune-count cap mirroring the write path's MaxCronTitleLen guard; the
		// byte-level check above alone would let a CJK title reach 3× the rune cap
		// and inflate every 1 Hz list broadcast (#1075).
		if utf8.RuneCountInString(j.Title) > MaxCronTitleLen {
			slog.Warn("cron store: dropping job with overlong title",
				"path", path, "cron_id", j.ID,
				"title_runes", utf8.RuneCountInString(j.Title),
				"cap", MaxCronTitleLen)
			continue
		}
		if !utf8.ValidString(j.Backend) || containsCronUnsafe(j.Backend) {
			slog.Warn("cron store: dropping job with invalid backend bytes",
				"path", path, "cron_id", j.ID, "backend_bytes", len(j.Backend))
			continue
		}
		// Placement: tampered/unknown values are normalised to local rather than
		// dropping the job — a bad placement is a routing choice, not an injection
		// vector. The sandbox×work_dir combination degrades the same way.
		if err := validatePlacement(j.Placement); err != nil {
			slog.Warn("cron store: normalising job with invalid placement to local",
				"path", path, "cron_id", j.ID, "err", err)
			j.Placement = ""
		}
		if placementIsSandbox(j.Placement) && j.WorkDir != "" {
			slog.Warn("cron store: sandbox job carries work_dir (Phase 1 unsupported); normalising placement to local",
				"path", path, "cron_id", j.ID)
			j.Placement = ""
		}
		// Defensive Schedule / WorkDir validation for offline edits: control bytes
		// would smuggle log injection into "could not chdir" lines and an over-long
		// Schedule would reach dashboard responses and metrics labels. Runtime
		// semantics are unaffected because registerJob re-validates the schedule.
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
		// Same defensive rationale for NotifyChatID / NotifyPlatform: both ride the
		// 1Hz /api/cron broadcast.
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
		// A duplicate ID can only come from a hand-edited or badly merged file; keep
		// the first occurrence and warn so operators see the corruption at boot.
		if _, dup := m[j.ID]; dup {
			slog.Warn("cron store: dropping duplicate job ID",
				"path", path, "cron_id", j.ID)
			continue
		}
		m[j.ID] = j
	}
	slog.Info("loaded cron store", "count", len(m), "path", path)
	return m, nil
}

// containsCronUnsafe reports whether s contains any byte the cron field-safety
// policy rejects: C0 controls other than \t \n \r, DEL, Unicode bidi overrides
// (U+202A..U+202E, U+2066..U+2069) or LS/PS (U+2028/U+2029). Bidi codepoints
// can visually reorder glyphs in IM / browser renderers so a tampered prompt
// swaps "rm -rf /tmp/safe" for "rm -rf /etc/passwd" at display time; LS/PS
// introduce hard line breaks. Matches what validateCronPrompt blocks on the
// write paths. Inlined byte scan (no textutil regex) because loadJobs runs once
// at startup and the ASCII path stays branchless.
func containsCronUnsafe(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		if b < 0x20 || b == 0x7F {
			return true
		}
		// All guarded codepoints are 3-byte sequences with an E2 80 / E2 81 prefix
		// (U+2028..U+202E → E2 80 A8..AE, U+2066..U+2069 → E2 81 A6..A9).
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
