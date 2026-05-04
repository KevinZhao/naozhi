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
		// R180-GO-P2: wrap so the loadStore slog attr identifies the specific
		// path + open-phase failure instead of the bare os.PathError text.
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStoreFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	// R180-GO-P2: drop the unnecessary int64 cast. maxStoreFileBytes is an
	// untyped constant and len returns int; the comparison cannot overflow
	// on any 64-bit platform. Matches the unadorned len(data) below.
	if len(data) > maxStoreFileBytes {
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
	UserLabel      string   `json:"user_label,omitempty"`  // operator-set display name override
}

// storeFormatVersion is the current schema version for `sessions.json`.
// Bump this constant when the JSON shape changes in a way that older
// naozhi binaries cannot safely parse (e.g. adding a required field,
// renaming a key). Additive fields with `omitempty` do NOT require a bump
// — old binaries tolerate unknown fields.
//
// The version is NOT embedded in sessions.json itself (that file is a
// bare JSON array for back-compat with every prior release); instead it
// lives in a sidecar `sessions.meta.json`. loadStore reads the sidecar,
// warns if the observed version is newer than this constant, and then
// proceeds — operators get a heads-up that their binary may mis-parse
// the on-disk data, but the load never fails hard on a missing sidecar
// (treated as v1, the initial format).
const storeFormatVersion = 1

// storeMeta is the payload written to sessions.meta.json alongside the
// main store file. Kept in its own struct so future schema signalling
// (compression, sharding, etc.) can grow here without touching storeEntry.
type storeMeta struct {
	Version   int    `json:"version"`
	WrittenAt int64  `json:"written_at"`          // unix nano when saveStore last succeeded
	Generator string `json:"generator,omitempty"` // human-readable "naozhi <tag>"; omitempty for test paths
}

// storeMetaPath returns the sidecar meta path derived from the main
// store path: `.../sessions.json` → `.../sessions.meta.json`.
func storeMetaPath(storePath string) string {
	if storePath == "" {
		return ""
	}
	base := filepath.Base(storePath)
	ext := filepath.Ext(base)
	stem := base[:len(base)-len(ext)]
	return filepath.Join(filepath.Dir(storePath), stem+".meta"+ext)
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
		// Scratch (ephemeral aside) sessions are deliberately volatile: they
		// must not persist across restarts, or loadStore would resurrect a
		// zombie scratch whose quoted-context --append-system-prompt is long
		// gone and whose dashboard tab has been closed. Skip them here — the
		// pool TTL sweeper handles live cleanup; persistence simply never
		// records them.
		if IsScratchKey(s.key) {
			continue
		}
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
				cost = loadTotalCost(&s.totalCost)
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
				Workspace:      s.Workspace(),
				Backend:        s.Backend(),
				LastActive:     s.lastActive.Load(),
				UserLabel:      s.UserLabel(),
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
	// Best-effort write of the sidecar meta file. A failure here does NOT
	// fail the save: the main store is already durable, and the meta is
	// advisory (used to detect cross-version downgrades). Log so operators
	// catch partial-filesystem issues during normal ops.
	writeStoreMeta(path)
	return nil
}

// writeStoreMeta writes the version sidecar next to the main store. Called
// after a successful saveStore so the two files stay correlated. Uses the
// same atomic-write primitive so a crash between the two writes leaves the
// main store consistent even if the meta is stale by one cycle.
func writeStoreMeta(storePath string) {
	metaPath := storeMetaPath(storePath)
	if metaPath == "" {
		return
	}
	meta := storeMeta{
		Version:   storeFormatVersion,
		WrittenAt: time.Now().UnixNano(),
	}
	data, err := json.Marshal(meta)
	if err != nil {
		slog.Warn("marshal session store meta failed", "err", err)
		return
	}
	// The meta sidecar is advisory (used only for cross-version downgrade
	// detection) — failure here does not compromise the already-durable main
	// store. A plain write spares two fsyncs per save (vs. WriteFileAtomic's
	// fsync tmp + SyncDir pair), which matters on slower durable backends like
	// EBS gp2 where each fsync can run 20-50ms. A partial write on crash only
	// loses the sidecar; readStoreMeta treats a missing/malformed sidecar as
	// legacy v1, so the downgrade check degrades gracefully.
	if err := os.WriteFile(metaPath, data, 0600); err != nil {
		slog.Warn("write session store meta failed", "path", metaPath, "err", err)
	}
}

// readStoreMeta loads the sidecar. Returns the meta plus a flag indicating
// whether a sidecar was present at all. A missing sidecar is treated as
// "unknown / legacy" — the caller handles it as format v1, preserving the
// contract that sessions.json from any prior naozhi version is readable.
func readStoreMeta(storePath string) (storeMeta, bool) {
	metaPath := storeMetaPath(storePath)
	if metaPath == "" {
		return storeMeta{}, false
	}
	data, err := readCappedFile(metaPath, "session store meta")
	if err != nil {
		slog.Warn("read session store meta failed", "path", metaPath, "err", err)
		return storeMeta{}, false
	}
	if data == nil {
		return storeMeta{}, false
	}
	var m storeMeta
	if err := json.Unmarshal(data, &m); err != nil {
		slog.Warn("parse session store meta failed", "path", metaPath, "err", err)
		return storeMeta{}, false
	}
	return m, true
}

func loadStore(path string) map[string]*storeEntry {
	if path == "" {
		return nil
	}
	// Read the sidecar first so we can warn about future-version downgrades
	// BEFORE the main parse runs. If the meta claims a version newer than
	// we know, the parse below may still succeed (the entry schema has only
	// grown additively so far), but operators should be aware they may be
	// silently dropping fields the new naozhi binary wrote. Missing meta is
	// fine — that's the legacy case (sessions.json written by any naozhi
	// older than the sidecar introduction).
	if meta, ok := readStoreMeta(path); ok && meta.Version > storeFormatVersion {
		slog.Warn("session store was written by a newer naozhi; downgrade in progress?",
			"path", path,
			"observed_version", meta.Version,
			"supported_version", storeFormatVersion,
			"written_at_ns", meta.WrittenAt)
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
	// R180-GO-P2: Go map iteration is randomised per run; without this
	// sort each saveKnownIDs write produces different byte output for the
	// same logical set, which adds pointless diff noise to backups / audit
	// tooling and makes on-disk regression testing harder.
	slices.Sort(list)
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
