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

// maxCronStoreBytes caps the size of cron_jobs.json during Load. A cap of
// max_jobs=50 jobs with long prompts fits comfortably within 256 KB; 1 MB is
// ample headroom. A file larger than this is treated as corrupt/tampered and
// ignored (with the original preserved for forensics).
const maxCronStoreBytes = 1 * 1024 * 1024

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

func loadJobs(path string) map[string]*Job {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("load cron store failed", "path", path, "err", err)
		}
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxCronStoreBytes+1))
	if err != nil {
		slog.Warn("read cron store failed", "path", path, "err", err)
		return nil
	}
	if int64(len(data)) > maxCronStoreBytes {
		slog.Warn("cron store exceeds size cap; refusing to load",
			"path", path, "cap_bytes", maxCronStoreBytes, "observed_bytes", len(data))
		return nil
	}

	var entries []*Job
	if err := json.Unmarshal(data, &entries); err != nil {
		// Preserve the corrupt file so the next save does not silently
		// overwrite operator-visible evidence. Mirrors session/store.go.
		corruptPath := path + ".corrupt." + time.Now().Format("20060102-150405")
		if renameErr := os.Rename(path, corruptPath); renameErr != nil {
			slog.Warn("parse cron store failed; could not rename corrupt file",
				"err", err, "rename_err", renameErr, "path", path)
		} else {
			slog.Warn("parse cron store failed; corrupt file preserved",
				"err", err, "corrupt_path", corruptPath)
		}
		return nil
	}

	m := make(map[string]*Job, len(entries))
	for _, j := range entries {
		if j.ID != "" {
			m[j.ID] = j
		}
	}
	slog.Info("loaded cron store", "count", len(m), "path", path)
	return m
}
