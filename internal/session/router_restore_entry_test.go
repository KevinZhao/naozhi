package session

import (
	"path/filepath"
	"testing"
)

// TestRestoreSessionFromEntry_AllFields pins R20260531A-ARCH-4 (#1528): the
// extracted restoreSessionFromEntry helper must rebuild every persisted field
// exactly as the old inline NewRouter loop did. We persist a fully-populated
// storeEntry, boot a router, and assert each field round-trips.
func TestRestoreSessionFromEntry_AllFields(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")

	key := "feishu:direct:alice:general"
	src := newSessionWithID(key, "sess-111")
	src.setWorkspace("/srv/work/alice")
	src.SetUserLabel("重构会话")
	src.setLabelOrigin("auto")
	src.SetModel("claude-opus-4.7")
	storeTotalCost(&src.totalCost, 3.50)
	src.lastActive.Store(1_700_000_000_000_000_000)
	src.createdAt.Store(1_600_000_000_000_000_000)
	src.historyMu.Lock()
	src.prevSessionIDs = []string{"old-1", "old-2"}
	// Both origins are real-rotation labels (manual/resume): the startup
	// auto-chain retire (retireAutoChainOnce) strips only auto-spawn /
	// auto-backfill, so this round-trip must preserve the full 2-entry chain.
	src.prevSessionOrigins = []string{"manual", "resume"}
	src.historyMu.Unlock()

	if err := saveStore(storePath, map[string]*ManagedSession{key: src}); err != nil {
		t.Fatalf("saveStore: %v", err)
	}

	r := NewRouter(RouterConfig{MaxProcs: 3, StorePath: storePath})
	t.Cleanup(func() { r.Shutdown() })

	r.mu.RLock()
	got := r.ss.sessions[key]
	r.mu.RUnlock()
	if got == nil {
		t.Fatal("session not restored")
	}

	if got.getSessionID() != "sess-111" {
		t.Errorf("sessionID = %q, want sess-111", got.getSessionID())
	}
	if got.Workspace() != "/srv/work/alice" {
		t.Errorf("workspace = %q, want /srv/work/alice", got.Workspace())
	}
	if got.UserLabel() != "重构会话" {
		t.Errorf("userLabel = %q, want 重构会话", got.UserLabel())
	}
	if got.LabelOrigin() != "auto" {
		t.Errorf("labelOrigin = %q, want auto", got.LabelOrigin())
	}
	if got.Model() != "claude-opus-4.7" {
		t.Errorf("model = %q, want claude-opus-4.7", got.Model())
	}
	if c := loadTotalCost(&got.totalCost); c != 3.50 {
		t.Errorf("totalCost = %v, want 3.50", c)
	}
	if got.lastActive.Load() != 1_700_000_000_000_000_000 {
		t.Errorf("lastActive = %d, want 1700000000000000000", got.lastActive.Load())
	}
	if got.createdAt.Load() != 1_600_000_000_000_000_000 {
		t.Errorf("createdAt = %d, want persisted createdAt", got.createdAt.Load())
	}
	chain := got.SnapshotPrevSessionIDs()
	if len(chain) != 2 || chain[0] != "old-1" || chain[1] != "old-2" {
		t.Errorf("prevSessionIDs = %v, want [old-1 old-2]", chain)
	}
}

// TestRestoreSessionFromEntry_CreatedAtFallback pins the createdAt fallback
// branch: a pre-feature entry with CreatedAt==0 must inherit LastActive as the
// sidebar anchor.
func TestRestoreSessionFromEntry_CreatedAtFallback(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")

	key := "feishu:direct:bob:general"
	src := newSessionWithID(key, "sess-222")
	src.lastActive.Store(1_500_000_000_000_000_000)
	// createdAt deliberately left at zero (pre-feature store shape).

	if err := saveStore(storePath, map[string]*ManagedSession{key: src}); err != nil {
		t.Fatalf("saveStore: %v", err)
	}

	r := NewRouter(RouterConfig{MaxProcs: 3, StorePath: storePath})
	t.Cleanup(func() { r.Shutdown() })

	r.mu.RLock()
	got := r.ss.sessions[key]
	r.mu.RUnlock()
	if got == nil {
		t.Fatal("session not restored")
	}
	if got.createdAt.Load() != 1_500_000_000_000_000_000 {
		t.Errorf("createdAt fallback = %d, want LastActive 1500000000000000000", got.createdAt.Load())
	}
}
