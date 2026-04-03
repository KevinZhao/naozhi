package session

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

type storeEntry struct {
	Key       string  `json:"key"`
	SessionID string  `json:"session_id"`
	TotalCost float64 `json:"total_cost,omitempty"`
	Workspace string  `json:"workspace,omitempty"`
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

	entries := make([]storeEntry, 0, len(sessions))
	for _, s := range sessions {
		// Use getSessionID to avoid data race with concurrent Send.
		// Fallback to process's SessionID which is set earlier (on system/init),
		// before Send() completes and propagates it to ManagedSession.
		sid := s.getSessionID()
		if sid == "" && s.process != nil {
			sid = s.process.GetSessionID()
		}
		if sid != "" {
			var cost float64
			if s.process != nil {
				cost = s.process.TotalCost()
			} else {
				cost = s.totalCost
			}
			entries = append(entries, storeEntry{
				Key:       s.Key,
				SessionID: sid,
				TotalCost: cost,
				Workspace: s.workspace,
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
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func loadStore(path string) map[string]*storeEntry {
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

	var entries []storeEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		slog.Warn("parse session store failed", "err", err)
		return nil
	}

	m := make(map[string]*storeEntry, len(entries))
	for i, e := range entries {
		if e.Key != "" && e.SessionID != "" {
			m[e.Key] = &entries[i]
		}
	}
	slog.Info("loaded session store", "count", len(m), "path", path)
	return m
}
