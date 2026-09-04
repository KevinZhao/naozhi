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
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/osutil"
)

// maxStoreFileBytes caps how much Load reads from any session-store file so a
// corrupt or maliciously extended file cannot OOM the process during startup.
const maxStoreFileBytes = 4 * 1024 * 1024

// readCappedFile reads up to maxStoreFileBytes from path. Returns nil, nil for
// a missing file ("empty" store); a file over the cap is rejected outright.
func readCappedFile(path string, label string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxStoreFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > maxStoreFileBytes {
		slog.Warn(label+" exceeds size cap; refusing to load",
			"path", path, "cap_bytes", maxStoreFileBytes, "observed_bytes", len(data))
		return nil, fmt.Errorf("%s %s exceeds %d-byte cap", label, path, maxStoreFileBytes)
	}
	return data, nil
}

// preserveCorruptFile renames a file that failed to JSON-parse to a
// ".corrupt.<ts>" sibling so the next atomic save does not silently overwrite
// it. Used by every loader in this package for a consistent breadcrumb (#673).
func preserveCorruptFile(path, label string, parseErr error) {
	corruptPath := path + ".corrupt." + time.Now().Format("20060102-150405")
	if renameErr := os.Rename(path, corruptPath); renameErr != nil {
		slog.Warn("parse "+label+" failed; could not rename corrupt file",
			"err", parseErr, "rename_err", renameErr, "path", path)
		return
	}
	slog.Warn("parse "+label+" failed; corrupt file preserved",
		"err", parseErr, "corrupt_path", corruptPath)
}

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

	// prevGen is ManagedSession.prevHistoryGen when the chain slices above were
	// snapshotted (unexported: never hits disk). Bumped under historyMu on every
	// chain mutation, so equal gen ⇒ identical chains; see equalStoreEntry (#2346).
	prevGen uint64
}

// storeFormatVersion is the schema version for `sessions.json`; bump only for
// changes older binaries cannot parse (additive `omitempty` fields need none).
// It lives in the sidecar `sessions.meta.json` (sessions.json stays a bare
// array for back-compat); loadStore warns on a newer version and never fails
// on a missing sidecar (treated as v1).
const storeFormatVersion = 1

// storeMeta is the sessions.meta.json payload.
type storeMeta struct {
	Version   int    `json:"version"`
	WrittenAt int64  `json:"written_at"`          // unix nano when saveStore last succeeded
	Generator string `json:"generator,omitempty"` // human-readable "naozhi <tag>"; omitempty for test paths
}

// storeMetaPath derives the sidecar path: `.../sessions.json` → `.../sessions.meta.json`.
func storeMetaPath(storePath string) string {
	if storePath == "" {
		return ""
	}
	base := filepath.Base(storePath)
	ext := filepath.Ext(base)
	stem := base[:len(base)-len(ext)]
	return filepath.Join(filepath.Dir(storePath), stem+".meta"+ext)
}

// sessionToStoreEntry converts a ManagedSession to its on-disk storeEntry,
// returning (_, false) when the session is intentionally not persisted
// (scratch / sys daemon stub / no SessionID yet).
//
// CONTRACT: callers MUST hold no Router-level lock. This takes
// ManagedSession.historyMu RLock for the prevSession* read and per-field
// atomics/mutexes via accessors; holding r.mu here would violate the Router
// contract (router_core.go) that historyMu is never held together with r.mu.
func sessionToStoreEntry(s *ManagedSession) (storeEntry, bool) {
	// Scratch (ephemeral aside) sessions must not persist, or loadStore would
	// resurrect a zombie whose quoted-context system prompt is gone and whose
	// dashboard tab is closed. The pool TTL sweeper handles live cleanup.
	if IsScratchKey(s.key) {
		return storeEntry{}, false
	}
	// System daemon stubs (sys:{name}) are register-on-startup. Independent of
	// isExemptKey (TTL/LRU only): persisting them would leave dangling entries
	// and broaden the sessions.json tampering surface (system-session RFC §14).
	if IsSysKey(s.key) {
		return storeEntry{}, false
	}
	// getSessionID avoids a data race with concurrent Send; fall back to the
	// process's SessionID (set on system/init, before Send propagates it).
	// Snapshot loadProcess() once: a second call could observe a fresh process
	// from a concurrent spawnSession whose TotalCost() is 0 and clobber the real cost.
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
	var prevGen uint64
	s.historyMu.RLock()
	// Read prevHistoryGen inside the same RLock so the (gen, slices) pair is
	// mutually consistent: a writer that bumps gen holds historyMu.Lock (#2346).
	prevGen = s.prevHistoryGen.Load()
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
		prevGen:            prevGen,
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
	}, true
}

// equalStoreEntry reports whether two storeEntry values are identical; used by
// the per-session marshal cache to decide whether cached JSON can be reused.
// The chain slices are compared via prevGen (bumped under historyMu on every
// chain mutation, so equal gen ⇒ identical contents); both entries come from
// sessionToStoreEntry, so both carry a populated prevGen (#2346).
func equalStoreEntry(a, b storeEntry) bool {
	return a.Key == b.Key &&
		a.SessionID == b.SessionID &&
		a.TotalCost == b.TotalCost &&
		a.CostSpent == b.CostSpent &&
		a.LastCumulativeCost == b.LastCumulativeCost &&
		a.Workspace == b.Workspace &&
		a.Backend == b.Backend &&
		a.AccessProfile == b.AccessProfile &&
		a.LastActive == b.LastActive &&
		a.CreatedAt == b.CreatedAt &&
		a.UserLabel == b.UserLabel &&
		a.LabelOrigin == b.LabelOrigin &&
		a.Model == b.Model &&
		a.TuningModel == b.TuningModel &&
		a.TuningEffort == b.TuningEffort &&
		a.prevGen == b.prevGen
}

// encodeStoreEntryCached returns the JSON encoding of s's current storeEntry,
// reusing the per-session memo when unchanged since the last save. Returns
// (_, false) when the session is not persisted. The returned slice is owned by
// the cache and must NOT be mutated by the caller.
//
// CONTRACT mirrors sessionToStoreEntry: callers MUST hold no Router-level lock.
// The cache pointer is only touched on the single-goroutine save path
// (cleanup loop), so it needs no synchronisation.
func encodeStoreEntryCached(s *ManagedSession) ([]byte, bool) {
	entry, ok := sessionToStoreEntry(s)
	if !ok {
		return nil, false
	}
	if c := s.storeMarshalCache.Load(); c != nil && equalStoreEntry(c.entry, entry) {
		return c.data, true
	}
	data, err := json.Marshal(entry)
	if err != nil {
		// Cannot fail in practice (plain scalars + string slices). Drop the cache
		// and skip this entry rather than poison the whole save; log the drop.
		slog.Error("encodeStoreEntryCached: marshal failed, session dropped from persistence", "key", s.key, "err", err)
		s.storeMarshalCache.Store(nil)
		return nil, false
	}
	s.storeMarshalCache.Store(&storeEntryCache{entry: entry, data: data})
	return data, true
}

// marshalStoreEntries builds the sessions.json array by concatenating each
// session's cached encoding — on-the-wire equal to json.Marshal([]storeEntry)
// but skipping unchanged sessions (#1523). Map order is fine: loadStore is order-insensitive.
func marshalStoreEntries(sessions map[string]*ManagedSession) ([]byte, error) {
	return marshalStoreEntriesFunc(len(sessions), func(yield func(*ManagedSession)) {
		for _, s := range sessions {
			yield(s)
		}
	})
}

// marshalStoreEntriesSlice is the slice-input twin of marshalStoreEntries so the
// periodic save path avoids allocating a map every tick (#1606). Output is identical.
func marshalStoreEntriesSlice(sessions []*ManagedSession) ([]byte, error) {
	return marshalStoreEntriesFunc(len(sessions), func(yield func(*ManagedSession)) {
		for _, s := range sessions {
			yield(s)
		}
	})
}

// storeMarshalBufPool recycles the assembly buffer of marshalStoreEntriesFunc
// so the steady-state save tick is allocation-free (#2073). Safe because the
// buffer is fully consumed before return: save paths hand it to writeStoreData
// (synchronous WriteFileAtomic that does not retain the slice), then call
// putStoreMarshalBuf. Mirrors bridgeEncPool in eventlog_bridge.go.
var storeMarshalBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 4096)
		return &b
	},
}

// storeMarshalBufMaxCap caps buffer reuse so a one-off oversized store does not
// permanently pin a large backing array in the pool. Same rationale as bridgeEncMaxCap.
const storeMarshalBufMaxCap = 256 * 1024

// storeMarshalBufRecyclable is the cap gate as a pure predicate, testable
// without sync.Pool (whose Get/Put give no reuse guarantee under GC).
func storeMarshalBufRecyclable(capacity int) bool {
	return capacity <= storeMarshalBufMaxCap
}

// putStoreMarshalBuf returns an assembly buffer to the pool once writeStoreData
// has copied the bytes to disk. Buffers that grew past the cap are dropped.
func putStoreMarshalBuf(buf []byte) {
	if !storeMarshalBufRecyclable(cap(buf)) {
		return
	}
	b := buf[:0]
	storeMarshalBufPool.Put(&b)
}

// marshalStoreEntriesFunc assembles the JSON array from each session's cached
// encoding, driven by an iteration closure shared by the map and slice inputs.
// The returned slice is borrowed from storeMarshalBufPool; the caller MUST
// return it via putStoreMarshalBuf once written (not returning it only
// forfeits reuse).
func marshalStoreEntriesFunc(n int, iter func(yield func(*ManagedSession))) ([]byte, error) {
	bufp := storeMarshalBufPool.Get().(*[]byte)
	buf := (*bufp)[:0]
	buf = append(buf, '[')
	first := true
	iter(func(s *ManagedSession) {
		data, ok := encodeStoreEntryCached(s)
		if !ok {
			return
		}
		if !first {
			buf = append(buf, ',')
		}
		first = false
		buf = append(buf, data...)
	})
	buf = append(buf, ']')
	return buf, nil
}

func saveStore(path string, sessions map[string]*ManagedSession) error {
	if path == "" {
		return nil
	}
	// Per-session cached encodings: per tick O(changed), not O(N) (#1523).
	data, err := marshalStoreEntries(sessions)
	if err != nil {
		return fmt.Errorf("marshal session store: %w", err)
	}
	// data is a pooled buffer; writeStoreData copies it synchronously and does
	// not retain it, so recycling after the write returns is safe either way.
	writeErr := writeStoreData(path, data)
	putStoreMarshalBuf(data)
	return writeErr
}

// saveStoreSlice persists a slice snapshot of sessions (periodic Cleanup /
// saveIfDirty paths, #1606). On-disk output is identical to saveStore.
func saveStoreSlice(path string, sessions []*ManagedSession) error {
	if path == "" {
		return nil
	}
	data, err := marshalStoreEntriesSlice(sessions)
	if err != nil {
		return fmt.Errorf("marshal session store: %w", err)
	}
	// See saveStore: data is a pooled buffer that writeStoreData does not retain.
	writeErr := writeStoreData(path, data)
	putStoreMarshalBuf(data)
	return writeErr
}

// storeDirEnsured memoizes, per store directory, that datadir.EnsureDir has
// succeeded so the 30s save tick skips its syscalls. Keyed by path (not a
// sync.Once) so Routers with distinct store dirs each get their own MkdirAll.
// A failed EnsureDir is NOT recorded, so a later save retries.
var storeDirEnsured sync.Map // map[string]struct{}

// storeMetaWritten tracks store paths whose advisory version sidecar was
// written in this process; its content is a compile-time constant, so once per
// path suffices. writeStoreMeta is best-effort (no error return), so the path
// is recorded after the first attempt regardless of outcome.
var storeMetaWritten sync.Map // map[string]struct{}

// writeStoreData ensures the store directory exists, atomically writes the
// pre-marshalled bytes, then writes the advisory version sidecar.
func writeStoreData(path string, data []byte) error {
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
	// Best-effort sidecar: a failure does NOT fail the save (main store is
	// already durable; meta is advisory). Written once per path; see storeMetaWritten.
	if _, done := storeMetaWritten.Load(path); !done {
		writeStoreMeta(path)
		storeMetaWritten.Store(path, struct{}{})
	}
	return nil
}

// writeStoreMeta writes the version sidecar after the main store is durable so
// the two stay correlated. Uses WriteFileAtomic so a crash mid-write leaves the
// previous or the new meta, never a torn one that would make readStoreMeta warn
// on every load (#1023). Its fsyncs run after the main write, so save latency is unaffected.
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
	if err := osutil.WriteFileAtomic(metaPath, data, 0600); err != nil {
		slog.Warn("write session store meta failed", "path", metaPath, "err", err)
	}
}

// readStoreMeta loads the sidecar, reporting whether one was present. A missing
// sidecar is "legacy" — the caller treats it as v1 so sessions.json from any
// prior naozhi version stays readable.
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
	// Warn about a future-version downgrade BEFORE the main parse: it may still
	// succeed (schema grows additively) while silently dropping fields the
	// newer binary wrote. Missing meta is the legacy case.
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
		preserveCorruptFile(path, "session store", err)
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

// knownIDsPath derives the known session IDs path (sessions.json → session-ids.json).
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
		// #673: preserve the corrupt file (see loadStore / loadWorkspaceOverrides).
		preserveCorruptFile(path, "known session IDs", err)
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
// PRECONDITION: sortedIDs is ALREADY sorted ascending (see
// Router.snapshotKnownIDsSortedLocked, memoised by gen, #1638). Deterministic
// on-disk order keeps backup/audit diffs noise-free; this does NOT re-sort.
func saveKnownIDs(storePath string, sortedIDs []string) error {
	data, err := json.Marshal(sortedIDs)
	if err != nil {
		return fmt.Errorf("marshal known IDs: %w", err)
	}
	return saveKnownIDsBytes(storePath, data)
}

// saveKnownIDsBytes persists already-marshaled known-ID JSON. The periodic save
// path hands it the gen-memoised bytes from snapshotKnownIDsMarshaledLocked
// (#2143); saveKnownIDs is the thin marshal+delegate wrapper for []string callers.
func saveKnownIDsBytes(storePath string, data []byte) error {
	path := knownIDsPath(storePath)
	if path == "" {
		return nil
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

// sessionStore groups the correlated session-table fields (#383). Value field
// on Router, NO lock of its own, read/written ONLY under Router.mu.
// INVARIANT: sessions + byChat + keyhash + idToKey MUST mutate together inside
// ONE r.mu write critical section (indexAdd/indexDel paired with each
// r.ss.sessions set/delete and the idToKey helpers) or a reader observes a torn
// index. activeCount/gen are read lock-free by Stats()/Version() and MUST stay
// atomic; dirty is a plain bool. The annotation on the Router embed line covers
// the UNION of all accessing domains; the lint recurses so each field carries
// its own per-domain `// 读写:` annotation, copied verbatim from the original
// router_core.go field docs.
type sessionStore struct {
	// 读写: core (init), lifecycle (spawn/reset/rename), shim (reconnect), cleanup (remove/cleanup), discovery (takeover/register), capacity (reconcile active-gauge scan)
	sessions map[string]*ManagedSession
	// byChat: chat key → set of session keys, for O(k) ResetChat with O(1)
	// dedupe/removal. Nil in test-created routers; all helpers are nil-safe.
	// 读写: core (indexAdd/Del helpers), lifecycle (ResetChat/install/unregister), cleanup, discovery
	byChat map[string]map[string]struct{}
	// keyhash: persist.KeyHash(sessionKey) → sessionKey, an O(1) lookup for the
	// attachment tracker's workspace resolver (#1646). Maintained at the publish
	// funnel + indexDel; the resolver self-heals on a miss by re-verifying
	// against r.ss.sessions and re-populating via a one-off scan. Nil in tests.
	// 读写: core (indexAdd/Del helpers + resolver), lifecycle (install/unregister)
	keyhash map[string]string
	// idToKey: session ID → session key, for O(1) RegisterForResume dedupe.
	// Maintained under r.mu by setSessionIDIndex/clearSessionIDIndex.
	// 读写: core (init), lifecycle (install/unregister), discovery (RegisterForResume), shim (reconnectShims index write)
	idToKey map[string]string
	// activeCount counts alive non-exempt processes. Writes happen under r.mu;
	// atomic so Stats() reads lock-free on the dashboard /api/sessions hot path.
	// 读写: core (Stats lock-free read), lifecycle (countActive/evict/install), capacity (reconcile Store), cleanup (remove/reconcile Add/Store), discovery (Takeover orphan Add), shim (reconnect Add)
	activeCount atomic.Int64
	// 读写: lifecycle (spawn/Reset/Rename mutations), shim (reconnect post-attach), discovery (label/register/takeover), cleanup (saveIfDirty consume), capacity (evictOldest mutation)
	dirty bool // true when sessions changed since last save
	// gen increments on each mutation under r.mu; atomic so Version() reads
	// lock-free on the dashboard poll path.
	// 读写: core (Version lock-free), lifecycle (BumpVersion), cleanup (BumpVersion), discovery (BumpVersion), capacity (evictOldest BumpVersion), shim (reconnect BumpVersion)
	gen atomic.Uint64
}

// knownIDsStore groups the correlated known-session-ID fields (#600). Value
// field on Router, NO lock of its own, read/written ONLY under Router.mu.
// The gen-invalidation chain lives inside this struct under one lock:
// trackSessionID bumps gen (the SOLE mutator) and the snapshot helpers rebuild
// only when their cached gen != gen, so the invalidation cannot tear.
// gen and sortedGen are PLAIN uint64, NOT atomic — there is no lock-free reader.
type knownIDsStore struct {
	// ids tracks ALL session IDs ever used, including removed/reset/evicted
	// ones, so discovery can match CLI processes to naozhi keys.
	// 读写: core (init), discovery (trackSessionID/Discovery*), cleanup (saveIfDirty)
	ids map[string]bool
	// order preserves insertion order so overflow eviction is FIFO: random
	// map-order eviction could drop a still-active ID and make discovery
	// misclassify its CLI process as external. Live window is order[orderHead:]
	// (amortized O(1) eviction); the dead prefix is compacted when large.
	// 读写: core (init), discovery (trackSessionID)
	order []string
	// orderHead is the index of the oldest live entry in order; entries before
	// it are evicted and cleared. order[orderHead:] mirrors the keys of ids.
	// 读写: core (init reset), discovery (trackSessionID)
	orderHead int
	// 读写: discovery, cleanup
	dirty bool
	// 读写: discovery, cleanup
	gen uint64 // incremented on each ids mutation (add/evict)
	// sortedCache memoises the sorted input for saveKnownIDs by gen so the sort
	// is paid once per mutation generation, not per save tick (#1638).
	// 读写: cleanup (Cleanup/saveIfDirty snapshot), discovery (invalidated via gen)
	sortedCache []string
	// 读写: store.go (snapshotKnownIDsSortedLocked rebuild/compare; invoked from cleanup saveIfDirty)
	sortedGen uint64 // gen the cache slice was sorted at; 0 = unbuilt
	// marshaledCache memoises the JSON of sortedCache by gen so an unchanged set
	// is not re-marshaled every throttled tick (#2143). nil = unbuilt.
	// 读写: store.go (snapshotKnownIDsMarshaledLocked rebuild; invoked from cleanup saveIfDirty/Shutdown)
	marshaledCache []byte
	// 读写: store.go (snapshotKnownIDsMarshaledLocked rebuild/compare)
	marshaledGen uint64 // gen the marshaled cache was built at; 0 = unbuilt
	// 读写: cleanup (Cleanup/saveIfDirty)
	savedAt time.Time // last successful saveKnownIDs; throttles fsync to 5min
}

// snapshotKnownIDsSortedLocked returns a sorted copy of the known-session-ID
// set for saveKnownIDs. Caller MUST hold r.mu. The sort is memoised by gen
// (#1638), so an unchanged tick is an O(N) copy with no sort. A fresh copy is
// always returned so it can be serialized outside the lock without aliasing
// the cache, whose backing array a later rebuild would replace.
func (r *Router) snapshotKnownIDsSortedLocked() []string {
	if r.kid.sortedCache == nil || r.kid.sortedGen != r.kid.gen {
		sorted := make([]string, 0, len(r.kid.ids))
		for id := range r.kid.ids {
			sorted = append(sorted, id)
		}
		// Deterministic on-disk order — see saveKnownIDs.
		slices.Sort(sorted)
		r.kid.sortedCache = sorted
		r.kid.sortedGen = r.kid.gen
	}
	// Return a copy: callers serialize outside r.mu, and a later rebuild
	// would otherwise replace the cache slice's backing array under them.
	return slices.Clone(r.kid.sortedCache)
}

// snapshotKnownIDsMarshaledLocked returns the JSON of the known-session-ID set
// for saveKnownIDsBytes. Caller MUST hold r.mu. The marshal is memoised by gen
// like sortedCache (#2143); one gen bump invalidates both. Returns a copy so
// callers can write outside r.mu without aliasing the cache, whose backing
// array a concurrent trackSessionID-triggered rebuild would replace.
func (r *Router) snapshotKnownIDsMarshaledLocked() ([]byte, error) {
	if r.kid.marshaledCache == nil || r.kid.marshaledGen != r.kid.gen {
		// Refresh the sorted cache first (same gen gate) and marshal it directly
		// rather than a clone, avoiding an extra copy on the rebuild path.
		if r.kid.sortedCache == nil || r.kid.sortedGen != r.kid.gen {
			sorted := make([]string, 0, len(r.kid.ids))
			for id := range r.kid.ids {
				sorted = append(sorted, id)
			}
			// Deterministic on-disk order — see saveKnownIDsBytes.
			slices.Sort(sorted)
			r.kid.sortedCache = sorted
			r.kid.sortedGen = r.kid.gen
		}
		data, err := json.Marshal(r.kid.sortedCache)
		if err != nil {
			return nil, fmt.Errorf("marshal known IDs: %w", err)
		}
		r.kid.marshaledCache = data
		r.kid.marshaledGen = r.kid.gen
	}
	return slices.Clone(r.kid.marshaledCache), nil
}

// workspaceOverridesPath derives the overrides path (sessions.json → workspace-overrides.json).
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
		// Preserve the corrupt file: the next save would otherwise overwrite
		// the evidence of a partial write (#673).
		preserveCorruptFile(path, "workspace overrides", err)
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
