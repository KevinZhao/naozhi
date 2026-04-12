package session

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

type storeEntry struct {
	Key            string   `json:"key"`
	SessionID      string   `json:"session_id"`
	PrevSessionIDs []string `json:"prev_session_ids,omitempty"` // oldest → newest
	TotalCost      float64  `json:"total_cost,omitempty"`
	Workspace      string   `json:"workspace,omitempty"`
	LastActive     int64    `json:"last_active,omitempty"` // unix nano
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
		if sid == "" {
			if p := s.loadProcess(); p != nil {
				sid = p.GetSessionID()
			}
		}
		if sid != "" {
			var cost float64
			if p := s.loadProcess(); p != nil {
				cost = p.TotalCost()
			} else {
				cost = s.totalCost
			}
			entries = append(entries, storeEntry{
				Key:            s.Key,
				SessionID:      sid,
				PrevSessionIDs: s.prevSessionIDs,
				TotalCost:      cost,
				Workspace:      s.workspace,
				LastActive:     s.lastActive.Load(),
			})
		}
	}

	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	// Fsync before rename to prevent data loss on power failure.
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	f.Close()
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
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

// knownIDsPath returns the path to the known session IDs file,
// derived from the store path (e.g. sessions.json → session-ids.json).
func knownIDsPath(storePath string) string {
	if storePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(storePath), "session-ids.json")
}

// loadKnownIDs reads the persistent set of all session IDs ever used by naozhi.
func loadKnownIDs(storePath string) map[string]bool {
	path := knownIDsPath(storePath)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("load known session IDs failed", "err", err)
		}
		return nil
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		slog.Warn("parse known session IDs failed", "err", err)
		return nil
	}
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	slog.Info("loaded known session IDs", "count", len(m), "path", path)
	return m
}

// saveKnownIDs persists the set of all session IDs ever used by naozhi.
func saveKnownIDs(storePath string, ids map[string]bool) error {
	path := knownIDsPath(storePath)
	if path == "" {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create known IDs directory: %w", err)
		}
	}
	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	data, err := json.Marshal(list)
	if err != nil {
		return fmt.Errorf("marshal known IDs: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("write known IDs %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write known IDs %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sync known IDs %s: %w", tmp, err)
	}
	f.Close()
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename known IDs to %s: %w", path, err)
	}
	return nil
}
