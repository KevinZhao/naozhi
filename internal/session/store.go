package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// maxStoreFileBytes caps how much data we read from any session-store file
// during Load. sessions.json for 1000 sessions with full PrevSessionIDs stays
// well under 500 KB; 4 MB gives ample headroom without letting a corrupt or
// maliciously extended file OOM the process during startup.
const maxStoreFileBytes = 4 * 1024 * 1024

// readCappedFile reads up to maxStoreFileBytes from path. Returns nil, nil if
// the file does not exist so callers can treat a missing store as "empty".
// A file that exceeds the cap is logged and rejected — the caller falls back
// to an empty store rather than parsing a truncated JSON prefix.
func readCappedFile(path string, label string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStoreFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxStoreFileBytes {
		slog.Warn(label+" exceeds size cap; refusing to load",
			"path", path, "cap_bytes", maxStoreFileBytes, "observed_bytes", len(data))
		return nil, fmt.Errorf("%s %s exceeds %d-byte cap", label, path, maxStoreFileBytes)
	}
	return data, nil
}

type storeEntry struct {
	Key            string   `json:"key"`
	SessionID      string   `json:"session_id"`
	PrevSessionIDs []string `json:"prev_session_ids,omitempty"` // oldest → newest
	TotalCost      float64  `json:"total_cost,omitempty"`
	Workspace      string   `json:"workspace,omitempty"`
	Backend        string   `json:"backend,omitempty"`     // "claude" | "kiro" | ...
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
		// Snapshot loadProcess() once — calling it twice (once for sid,
		// again for cost) can observe different processes across a
		// concurrent spawnSession, where the second call hits a fresh
		// process whose TotalCost() is 0 and silently clobbers the real
		// historical cost that should have been persisted.
		proc := s.loadProcess()
		sid := s.getSessionID()
		if sid == "" && proc != nil {
			sid = proc.GetSessionID()
		}
		if sid != "" {
			var cost float64
			if proc != nil {
				cost = proc.TotalCost()
			} else {
				cost = s.totalCost
			}
			// Clone PrevSessionIDs so the persistence path does not share
			// the backing array with live session mutations (spawnSession
			// reassigns s.prevSessionIDs but callers could in theory hold
			// the original slice; clone is cheap and forward-safe).
			var prevIDs []string
			if len(s.prevSessionIDs) > 0 {
				prevIDs = slices.Clone(s.prevSessionIDs)
			}
			entries = append(entries, storeEntry{
				Key:            s.key,
				SessionID:      sid,
				PrevSessionIDs: prevIDs,
				TotalCost:      cost,
				Workspace:      s.workspace,
				Backend:        s.Backend(),
				LastActive:     s.lastActive.Load(),
			})
		}
	}

	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("marshal session store: %w", err)
	}
	if err := osutil.WriteFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("save session store: %w", err)
	}
	return nil
}

func loadStore(path string) map[string]*storeEntry {
	if path == "" {
		return nil
	}
	data, err := readCappedFile(path, "session store")
	if err != nil {
		slog.Warn("load session store failed", "path", path, "err", err)
		return nil
	}
	if data == nil {
		return nil
	}

	var entries []storeEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		// Preserve the corrupt file for forensic analysis so the next save
		// does not silently overwrite it.
		corruptPath := path + ".corrupt." + time.Now().Format("20060102-150405")
		if renameErr := os.Rename(path, corruptPath); renameErr != nil {
			slog.Warn("parse session store failed; could not rename corrupt file",
				"err", err, "rename_err", renameErr, "path", path)
		} else {
			slog.Warn("parse session store failed; corrupt file preserved",
				"err", err, "corrupt_path", corruptPath)
		}
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
	data, err := readCappedFile(path, "known session IDs")
	if err != nil {
		slog.Warn("load known session IDs failed", "path", path, "err", err)
		return nil
	}
	if data == nil {
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
	if err := osutil.WriteFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("save known IDs: %w", err)
	}
	return nil
}

// workspaceOverridesPath returns the path to the workspace overrides file,
// derived from the store path (e.g. sessions.json → workspace-overrides.json).
func workspaceOverridesPath(storePath string) string {
	if storePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(storePath), "workspace-overrides.json")
}

// loadWorkspaceOverrides reads persisted per-chat workspace overrides.
func loadWorkspaceOverrides(storePath string) map[string]string {
	path := workspaceOverridesPath(storePath)
	if path == "" {
		return nil
	}
	data, err := readCappedFile(path, "workspace overrides")
	if err != nil {
		slog.Warn("load workspace overrides failed", "path", path, "err", err)
		return nil
	}
	if data == nil {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		slog.Warn("parse workspace overrides failed", "err", err)
		return nil
	}
	if len(m) > 0 {
		slog.Info("loaded workspace overrides", "count", len(m))
	}
	return m
}

// saveWorkspaceOverrides persists per-chat workspace overrides.
// Uses write-tmp → fsync → rename for crash-safe atomicity.
func saveWorkspaceOverrides(storePath string, overrides map[string]string) error {
	path := workspaceOverridesPath(storePath)
	if path == "" {
		return nil
	}
	if len(overrides) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("remove empty workspace overrides file", "path", path, "err", err)
		}
		return nil
	}
	data, err := json.Marshal(overrides)
	if err != nil {
		return fmt.Errorf("marshal workspace overrides: %w", err)
	}
	if err := osutil.WriteFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("save workspace overrides: %w", err)
	}
	return nil
}
