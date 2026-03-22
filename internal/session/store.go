package session

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

type sessionEntry struct {
	Key       string `json:"key"`
	SessionID string `json:"session_id"`
}

func saveStore(path string, sessions map[string]*ManagedSession) error {
	if path == "" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create store directory: %w", err)
		}
	}

	entries := make([]sessionEntry, 0, len(sessions))
	for _, s := range sessions {
		// Use getSessionID to avoid data race with concurrent Send
		if sid := s.getSessionID(); sid != "" {
			entries = append(entries, sessionEntry{
				Key:       s.Key,
				SessionID: sid,
			})
		}
	}

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadStore(path string) map[string]string {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("load session store failed", "err", err)
		}
		return nil
	}

	var entries []sessionEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.Warn("parse session store failed", "err", err)
		return nil
	}

	m := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Key != "" && e.SessionID != "" {
			m[e.Key] = e.SessionID
		}
	}
	slog.Info("loaded session store", "count", len(m), "path", path)
	return m
}
