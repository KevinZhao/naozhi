package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/osutil/jsonfile"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// maxStoreFileBytes caps how much Load reads from any session-store file so a
// corrupt or maliciously extended file cannot OOM the process during startup.
const maxStoreFileBytes = 4 * 1024 * 1024

type storeEntry struct {
	Key            string   `json:"key"`
	SessionID      string   `json:"session_id"`
	PrevSessionIDs []string `json:"prev_session_ids,omitempty"` // oldest → newest
	// PrevSessionOrigins is parallel to PrevSessionIDs: "manual" / "auto-spawn"
	// / "auto-backfill" / "resume". A shorter slice is forward-compatible
	// (loadStore fills the tail with "manual"). docs/rfc/auto-workspace-chain.md §4.6.
	PrevSessionOrigins []string `json:"prev_session_origins,omitempty"`
	TotalCost          float64  `json:"total_cost,omitempty"`
	// CostSpent is the authoritative cumulative spend (monotonic across
	// resume/restart); TotalCost mirrors the CLI's per-incarnation total.
	// LastCumulativeCost is the baseline the next per-turn delta diffs against.
	// Legacy stores lack both; restore seeds costSpent from TotalCost.
	CostSpent          float64 `json:"cost_spent,omitempty"`
	LastCumulativeCost float64 `json:"last_cumulative_cost,omitempty"`
	Workspace          string  `json:"workspace,omitempty"`
	Backend            string  `json:"backend,omitempty"` // "claude" | "kiro" | ...
	// AccessProfile is the access-profile ID this session spawned under ("" =
	// global default), so a post-restart resume relocks the same auth chain —
	// resuming a Bedrock conversation against a 1P endpoint would cross-charge and fail.
	AccessProfile string `json:"access_profile,omitempty"`
	LastActive    int64  `json:"last_active,omitempty"` // unix nano
	// CreatedAt anchors the sidebar position (unix nano, written once). When 0,
	// restore copies LastActive into createdAt so existing sessions keep their order.
	CreatedAt int64  `json:"created_at,omitempty"`
	UserLabel string `json:"user_label,omitempty"` // operator-set display name override
	// LabelOrigin records who set UserLabel: "" / "user" (human) or "auto"
	// (sysession daemon). Empty is treated as "user" — daemons must leave it alone.
	LabelOrigin string `json:"label_origin,omitempty"`
	// Model is the last-known CLI model (system/init for claude, SpawnOptions
	// for kiro) so the dashboard shows it after a restart before the next init.
	Model string `json:"model,omitempty"`
	// TuningModel / TuningEffort are the operator's per-session overrides ("" =
	// none); the only durable record since the kiro-side switch is process-bound.
	// Re-validated on load so a hand-edited file cannot smuggle argv into --model/--effort.
	TuningModel  string `json:"tuning_model,omitempty"`
	TuningEffort string `json:"tuning_effort,omitempty"`
	// CodeChanges is the PRs the session published or touched, newest last,
	// re-validated on load (the URLs become dashboard links).
	CodeChanges []clievent.CodeChange `json:"code_changes,omitempty"`
	// Workflows is the header of each workflow on the session's board, so a
	// restart shows runs the shim replay no longer holds; re-validated on load.
	Workflows []workflow.Ref `json:"workflows,omitempty"`
}

// sessionToStoreEntry converts a ManagedSession to its on-disk storeEntry,
// returning (_, false) when the session is intentionally not persisted
// (scratch / sys daemon stub / no SessionID yet).
//
// CONTRACT: callers MUST hold no Router-level lock. This takes
// ManagedSession.historyMu RLock for the prevSession* read and per-field
// atomics/mutexes via accessors; holding the table lock here would violate the Router
// contract (router_core.go) that historyMu is never held together with the table lock.
func sessionToStoreEntry(s *ManagedSession) (storeEntry, bool) {
	// Scratch (ephemeral aside) sessions must not persist, or loadStore would
	// resurrect a zombie whose quoted-context system prompt is gone and whose
	// dashboard tab is closed. The pool TTL sweeper handles live cleanup.
	if sessionkey.IsScratchKey(s.key) {
		return storeEntry{}, false
	}
	// System daemon stubs (sys:{name}) are register-on-startup. Independent of
	// isExemptKey (TTL/LRU only): persisting them would leave dangling entries
	// and broaden the sessions.json tampering surface (system-session RFC §14).
	if sessionkey.IsSysKey(s.key) {
		return storeEntry{}, false
	}
	// getSessionID avoids a data race with concurrent Send; fall back to the
	// process's SessionID (set on system/init, before Send propagates it).
	// Snapshot loadProcess() once: a second call could observe a fresh process
	// from a concurrent spawn whose TotalCost() is 0 and clobber the real cost.
	proc := s.loadProcess()
	sid := s.getSessionID()
	if sid == "" && proc != nil {
		sid = proc.SessionID()
	}
	if sid == "" {
		return storeEntry{}, false
	}
	var cost float64
	if proc != nil {
		cost = proc.TotalCost()
	} else {
		cost = loadTotalCost(&s.totalCost)
	}
	// Clone both chain slices in ONE historyMu critical section so the
	// persistence path never aliases live mutations and a concurrent
	// SetPrevSessionOrigins cannot publish a half-mutated pair.
	var prevIDs []string
	var prevOrigins []string
	s.historyMu.RLock()
	if len(s.prevSessionIDs) > 0 {
		prevIDs = slices.Clone(s.prevSessionIDs)
	}
	if len(s.prevSessionOrigins) > 0 {
		prevOrigins = slices.Clone(s.prevSessionOrigins)
	}
	s.historyMu.RUnlock()
	return storeEntry{
		Key:                s.key,
		SessionID:          sid,
		PrevSessionIDs:     prevIDs,
		PrevSessionOrigins: prevOrigins,
		TotalCost:          cost,
		CostSpent:          loadTotalCost(&s.costSpent),
		LastCumulativeCost: loadTotalCost(&s.lastCumulativeCost),
		Workspace:          s.Workspace(),
		Backend:            s.Backend(),
		AccessProfile:      s.AccessProfile(),
		LastActive:         s.lastActive.Load(),
		CreatedAt:          s.createdAt.Load(),
		UserLabel:          s.UserLabel(),
		LabelOrigin:        s.LabelOrigin(),
		Model:              s.Model(),
		TuningModel:        s.TuningModel(),
		TuningEffort:       s.TuningEffort(),
		CodeChanges:        s.CodeChanges(),
		Workflows:          s.WorkflowBoard().refs(time.Now()),
	}, true
}

// saveStore persists sessions as the sessions.json array. Each entry is
// marshalled on its own so one that cannot be encoded (e.g. a NaN cost) is
// dropped and logged instead of failing the whole save. An empty input writes
// `[]`. Order is irrelevant: loadStore is order-insensitive.
func saveStore(path string, sessions []*ManagedSession) error {
	if path == "" {
		return nil
	}
	// Before the marshal, not only inside writeStoreData: a blocked store used
	// to serialise every session each tick just to be refused (#2972).
	if err := blockedIfUnreadable(path); err != nil {
		return err
	}
	buf := []byte{'['}
	for _, s := range sessions {
		entry, ok := sessionToStoreEntry(s)
		if !ok {
			continue
		}
		data, err := json.Marshal(entry)
		if err != nil {
			slog.Error("saveStore: marshal failed, session dropped from persistence", "key", s.key, "err", err)
			continue
		}
		if len(buf) > 1 {
			buf = append(buf, ',')
		}
		buf = append(buf, data...)
	}
	return writeStoreData(path, append(buf, ']'))
}

// storeDirEnsured memoizes, per store directory, that datadir.EnsureDir has
// succeeded so the 30s save tick skips its syscalls. Keyed by path (not a
// sync.Once) so Routers with distinct store dirs each get their own MkdirAll.
// A failed EnsureDir is NOT recorded, so a later save retries.
var storeDirEnsured sync.Map // map[string]struct{}

// writeStoreData ensures the store directory exists and atomically writes the
// pre-marshalled bytes.
func writeStoreData(path string, data []byte) error {
	if err := blockedIfUnreadable(path); err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if _, done := storeDirEnsured.Load(dir); !done {
			// Shared dir policy (MkdirAll 0700 + symlink/non-dir guard + chmod)
			// so the session store gets the cron run store's hardening (#1175).
			if err := datadir.EnsureDir(dir); err != nil {
				return fmt.Errorf("create store directory: %w", err)
			}
			storeDirEnsured.Store(dir, struct{}{})
		}
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
	entries, out, err := jsonfile.Load[[]storeEntry](path, jsonfile.Options{
		MaxBytes: maxStoreFileBytes,
		Label:    "session store",
	})
	if err != nil {
		markStoreReadUnreadable(path, "session store", err)
		return nil
	}
	clearStoreReadUnreadable(path)
	if out != jsonfile.Parsed {
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

// StoredSessionIDs maps each key in the session store at path to the CLI
// session ids it has held, oldest first and the current one last. It only
// reads: a missing, unreadable or corrupt store yields nil and stays put.
func StoredSessionIDs(path string) map[string][]string {
	entries, out, err := jsonfile.Load[[]storeEntry](path, jsonfile.Options{
		MaxBytes: maxStoreFileBytes, Label: "session store", Corrupt: jsonfile.LeaveCorrupt,
	})
	if err != nil || out != jsonfile.Parsed {
		return nil
	}
	m := make(map[string][]string, len(entries))
	for _, e := range entries {
		if e.Key != "" && e.SessionID != "" {
			m[e.Key] = append(slices.Clone(e.PrevSessionIDs), e.SessionID)
		}
	}
	return m
}

// StoredKnownIDs lists every CLI session id naozhi has used, from the
// known-ids file beside the session store at storePath. Like
// StoredSessionIDs it only reads: a corrupt file yields nil and stays put.
func StoredKnownIDs(storePath string) []string {
	path := knownIDsPath(storePath)
	if path == "" {
		return nil
	}
	ids, out, err := jsonfile.Load[[]string](path, jsonfile.Options{
		MaxBytes: maxStoreFileBytes, Label: "known session IDs", Corrupt: jsonfile.LeaveCorrupt,
	})
	if err != nil || out != jsonfile.Parsed {
		return nil
	}
	return ids
}

// knownIDsPath derives the known session IDs path (sessions.json → session-ids.json).
func knownIDsPath(storePath string) string {
	if storePath == "" {
		return ""
	}
	return datadir.ForStore(storePath).SessionIDsPath()
}

// loadKnownIDs reads the persistent set of all session IDs ever used by naozhi.
func loadKnownIDs(storePath string) map[string]bool {
	path := knownIDsPath(storePath)
	if path == "" {
		return nil
	}
	// #673: jsonfile.Load preserves a corrupt file as .corrupt.<ts> before
	// returning !ok, so the next save does not overwrite the evidence.
	ids, out, err := jsonfile.Load[[]string](path, jsonfile.Options{
		MaxBytes: maxStoreFileBytes,
		Label:    "known session IDs",
	})
	if err != nil {
		markStoreReadUnreadable(path, "known session IDs", err)
		return nil
	}
	clearStoreReadUnreadable(path)
	if out != jsonfile.Parsed {
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
// PRECONDITION: sortedIDs is ALREADY sorted ascending
// (knownids.Store.SortedSnapshot). Deterministic on-disk order keeps
// backup/audit diffs noise-free; this does NOT re-sort.
func saveKnownIDs(storePath string, sortedIDs []string) error {
	data, err := json.Marshal(sortedIDs)
	if err != nil {
		return fmt.Errorf("marshal known IDs: %w", err)
	}
	return saveKnownIDsBytes(storePath, data)
}

// saveKnownIDsBytes persists already-marshaled known-ID JSON. The save paths
// hand it the gen-memoised bytes from knownids.Store.ClaimSave /
// MarshalSnapshot; saveKnownIDs is the marshal+delegate wrapper for []string callers.
func saveKnownIDsBytes(storePath string, data []byte) error {
	path := knownIDsPath(storePath)
	if path == "" {
		return nil
	}
	if err := blockedIfUnreadable(path); err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		// Shares storeDirEnsured with writeStoreData (same directory). A failed
		// EnsureDir is NOT recorded, allowing retry next tick.
		if _, done := storeDirEnsured.Load(dir); !done {
			if err := datadir.EnsureDir(dir); err != nil {
				return fmt.Errorf("create known IDs directory: %w", err)
			}
			storeDirEnsured.Store(dir, struct{}{})
		}
	}
	if err := osutil.WriteFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("save known IDs: %w", err)
	}
	return nil
}

// workspaceOverridesPath derives the overrides path (sessions.json → workspace-overrides.json).
func workspaceOverridesPath(storePath string) string {
	if storePath == "" {
		return ""
	}
	return datadir.ForStore(storePath).WorkspaceOverridesPath()
}

// loadWorkspaceOverrides reads persisted per-chat workspace overrides.
func loadWorkspaceOverrides(storePath string) map[string]string {
	path := workspaceOverridesPath(storePath)
	if path == "" {
		return nil
	}
	// A corrupt file is preserved as .corrupt.<ts> by jsonfile.Load, so the next
	// save does not overwrite the evidence of a partial write (#673).
	m, out, err := jsonfile.Load[map[string]string](path, jsonfile.Options{
		MaxBytes: maxStoreFileBytes,
		Label:    "workspace overrides",
	})
	if err != nil {
		markStoreReadUnreadable(path, "workspace overrides", err)
		return nil
	}
	clearStoreReadUnreadable(path)
	if out != jsonfile.Parsed {
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
	if err := blockedIfUnreadable(path); err != nil {
		return err
	}
	if len(overrides) == 0 {
		if err := os.Remove(path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				// A non-ENOENT failure leaves the stale file on disk. Propagate
				// like the WriteFileAtomic path so the caller keeps dirty set and
				// retries; returning nil would let the override resurrect on restart (#2337).
				return fmt.Errorf("remove empty workspace overrides: %w", err)
			}
			// ENOENT: already absent — nothing to remove or fsync.
			return nil
		}
		// Crash-durability parity with WriteFileAtomic (#673): an unlink leaves
		// the directory entry un-fsynced, so after power loss the deleted file
		// could resurrect. SyncDir degrades gracefully on EPERM/EINVAL. Reached
		// only when os.Remove succeeded, so the unlink being synced happened.
		if err := osutil.SyncDir(filepath.Dir(path)); err != nil {
			slog.Warn("fsync dir after workspace overrides removal", "path", path, "err", err)
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
