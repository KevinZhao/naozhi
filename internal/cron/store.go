package cron

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
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

func saveJobs(path string, jobs map[string]*Job) error {
	if path == "" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create cron store directory: %w", err)
		}
	}

	entries := make([]*Job, 0, len(jobs))
	for _, j := range jobs {
		entries = append(entries, j)
	}

	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("marshal cron store: %w", err)
	}
	if err := osutil.WriteFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("save cron store: %w", err)
	}
	return nil
}

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
		return nil, fmt.Errorf("open cron store %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxCronStoreBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read cron store %s: %w", path, err)
	}
	if int64(len(data)) > maxCronStoreBytes {
		// We read cap+1 via the LimitReader, so we can only report "at least"
		// the true size. Leave the original in place so operators can
		// inspect and recover; returning an error forces Scheduler.Start
		// to abort rather than overwrite the file with an empty array on
		// the next save.
		return nil, fmt.Errorf("cron store %s exceeds size cap (at least %d bytes, cap=%d bytes); refusing to load — inspect the file or move it aside before restarting",
			path, len(data), maxCronStoreBytes)
	}

	var entries []*Job
	if err := json.Unmarshal(data, &entries); err != nil {
		// Preserve the corrupt file so the next save does not silently
		// overwrite operator-visible evidence. Mirrors session/store.go.
		// If the rename itself fails we return an error (the original file
		// is still on disk and an empty save would destroy it).
		corruptPath := path + ".corrupt." + time.Now().Format("20060102-150405")
		if renameErr := os.Rename(path, corruptPath); renameErr != nil {
			return nil, fmt.Errorf("parse cron store %s failed (%v); could not rename to %s: %w",
				path, err, corruptPath, renameErr)
		}
		slog.Warn("parse cron store failed; corrupt file preserved",
			"err", err, "corrupt_path", corruptPath)
		return nil, nil
	}

	m := make(map[string]*Job, len(entries))
	for _, j := range entries {
		if j.ID != "" {
			m[j.ID] = j
		}
	}
	slog.Info("loaded cron store", "count", len(m), "path", path)
	return m, nil
}
