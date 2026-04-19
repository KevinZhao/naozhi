package cron

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

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
	// Atomic write: write to temp file, fsync, then rename
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("open cron store %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write cron store %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sync cron store %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close cron store %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename cron store to %s: %w", path, err)
	}
	return nil
}

func loadJobs(path string) map[string]*Job {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("load cron store failed", "err", err)
		}
		return nil
	}

	var entries []*Job
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.Warn("parse cron store failed", "err", err)
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
