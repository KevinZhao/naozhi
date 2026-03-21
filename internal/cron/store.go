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
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create cron store directory: %w", err)
		}
	}

	entries := make([]*Job, 0, len(jobs))
	for _, j := range jobs {
		entries = append(entries, j)
	}

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write: write to temp file, then rename
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
