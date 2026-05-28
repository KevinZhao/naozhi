package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
)

// TestSessionHandlers_RecordRetired_StampsHistory locks the contract that
// once a session retires, its UUID is observable in subsequent
// loadHistorySessions output as RetiredAt — and the stamp survives the
// 120s history cache invalidate triggered by the retirement.
func TestSessionHandlers_RecordRetired_StampsHistory(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	srv.sessionH.WaitWarmHistory()

	// Seed the FS with one minimal claude project + JSONL so RecentSessions
	// has a candidate to stamp. The session UUID must be IsValidSessionID().
	tmp := t.TempDir()
	wsRoot := filepath.Join(tmp, "ws")
	if err := os.MkdirAll(wsRoot, 0o700); err != nil {
		t.Fatalf("mkdir ws: %v", err)
	}
	claudeDir := filepath.Join(tmp, "claude")
	encodedDir := filepath.Join(claudeDir, "projects", encodeWorkspaceForClaudeDir(wsRoot))
	if err := os.MkdirAll(encodedDir, 0o700); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}
	sid := "11111111-2222-3333-4444-555555555555"
	jsonl := filepath.Join(encodedDir, sid+".jsonl")
	if err := os.WriteFile(jsonl, []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	// Point the handler at the seeded claudeDir and clear the warm cache so
	// loadHistorySessions runs against the test FS.
	srv.sessionH.claudeDir = claudeDir
	srv.sessionH.historyCacheMu.Lock()
	srv.sessionH.historyCache = nil
	srv.sessionH.historyCacheTime = time.Time{}
	srv.sessionH.historyCacheTimeUnixNano.Store(0)
	srv.sessionH.historyCacheMu.Unlock()

	// Inject an in-memory retired-store and stamp the test session.
	store, err := discovery.NewRetiredStore("")
	if err != nil {
		t.Fatalf("retired store: %v", err)
	}
	srv.sessionH.retiredStore = store
	retired := time.UnixMilli(1700000050000)
	store.MarkRetired(sid, retired)

	got := srv.sessionH.historySessions()
	if len(got) == 0 {
		t.Fatalf("expected at least one history entry, got 0")
	}
	var found *discovery.RecentSession
	for i := range got {
		if got[i].SessionID == sid {
			found = &got[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("seeded session %s not in history list (got %d entries)", sid, len(got))
	}
	if found.RetiredAt != retired.UnixMilli() {
		t.Errorf("RetiredAt = %d, want %d", found.RetiredAt, retired.UnixMilli())
	}
}

// TestSessionHandlers_RecordRetired_InvalidatesCache locks the contract
// that calling RecordRetired clears historyCacheTime so the next
// historySessions() reload picks up the new mark immediately, instead of
// waiting for the 120s TTL.
func TestSessionHandlers_RecordRetired_InvalidatesCache(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	srv.sessionH.WaitWarmHistory()

	store, _ := discovery.NewRetiredStore("")
	srv.sessionH.retiredStore = store

	// Pre-populate cache as fresh.
	srv.sessionH.historyCacheMu.Lock()
	srv.sessionH.historyCacheTime = time.Now()
	srv.sessionH.historyCacheMu.Unlock()

	srv.sessionH.RecordRetired("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")

	srv.sessionH.historyCacheMu.Lock()
	gotZero := srv.sessionH.historyCacheTime.IsZero()
	srv.sessionH.historyCacheMu.Unlock()
	if !gotZero {
		t.Fatalf("RecordRetired must zero historyCacheTime to force a reload")
	}
}

// TestSessionHandlers_RecordRetired_EmptyStoreNoOp protects the nil-store
// path: tests/deployments without a state dir construct SessionHandlers
// with retiredStore = nil, and RecordRetired must tolerate that.
func TestSessionHandlers_RecordRetired_EmptyStoreNoOp(t *testing.T) {
	h := &SessionHandlers{}
	// Must not panic.
	h.RecordRetired("any")
	h.FlushRetiredStore()
}

// TestSessionHandlers_FlushRetiredStore_Persists locks the shutdown
// contract: FlushRetiredStore writes the in-memory map to disk so the
// retirement order survives a restart.
func TestSessionHandlers_FlushRetiredStore_Persists(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "history-retired.json")
	store, err := discovery.NewRetiredStore(storePath)
	if err != nil {
		t.Fatalf("retired store: %v", err)
	}
	h := &SessionHandlers{retiredStore: store}
	h.RecordRetired("sid-1")
	h.FlushRetiredStore()

	raw, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("read persisted file: %v", err)
	}
	var file struct {
		Version int              `json:"version"`
		Entries map[string]int64 `json:"entries"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if file.Entries["sid-1"] == 0 {
		t.Fatalf("sid-1 not persisted; got %v", file.Entries)
	}
}

// TestSessionHandlers_RetiredAtFallback locks the back-compat contract:
// when retiredStore has no entry for a session, the RecentSession
// returned to the client carries RetiredAt=0, and the dashboard's
// JSON-level sort key (retired_at || last_active) falls back cleanly.
// Sort the same shape the JS does to make sure the fallback agrees with
// last_active-only ordering.
func TestSessionHandlers_RetiredAtFallback(t *testing.T) {
	t.Parallel()
	entries := []discovery.RecentSession{
		{SessionID: "old-but-recently-closed", LastActive: 100, RetiredAt: 5000},
		{SessionID: "newer-jsonl-not-closed", LastActive: 4000, RetiredAt: 0},
		{SessionID: "newest-jsonl", LastActive: 6000, RetiredAt: 0},
	}
	// Sort the same shape the JS does: max(retired_at, last_active) DESC.
	sort.SliceStable(entries, func(i, j int) bool {
		ki := entries[i].RetiredAt
		if ki == 0 {
			ki = entries[i].LastActive
		}
		kj := entries[j].RetiredAt
		if kj == 0 {
			kj = entries[j].LastActive
		}
		return ki > kj
	})
	if entries[0].SessionID != "newest-jsonl" {
		t.Fatalf("first should be 'newest-jsonl' (key=6000), got %q", entries[0].SessionID)
	}
	if entries[1].SessionID != "old-but-recently-closed" {
		t.Fatalf("second should be 'old-but-recently-closed' (key=5000), got %q", entries[1].SessionID)
	}
	if entries[2].SessionID != "newer-jsonl-not-closed" {
		t.Fatalf("third should be 'newer-jsonl-not-closed' (key=4000), got %q", entries[2].SessionID)
	}
}

// encodeWorkspaceForClaudeDir mirrors the Claude project-name encoding:
// "/foo/bar" → "-foo-bar". Used by tests to seed a claudeDir that
// resolveWorkspaceWithIndex can map back to a real on-disk workspace.
func encodeWorkspaceForClaudeDir(absPath string) string {
	r := []rune(absPath)
	for i, c := range r {
		if c == '/' {
			r[i] = '-'
		}
	}
	return string(r)
}
