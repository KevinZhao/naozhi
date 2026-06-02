package session

import (
	"path/filepath"
	"testing"
)

// bootRouterWithStore persists the given sessions then boots a router from
// that store, returning the restored router. The startup path runs
// retireAutoChainOnce before history loaders, so assertions on the restored
// chains observe the post-retire state.
func bootRouterWithStore(t *testing.T, sessions map[string]*ManagedSession) *Router {
	t.Helper()
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")
	if err := saveStore(storePath, sessions); err != nil {
		t.Fatalf("saveStore: %v", err)
	}
	r := NewRouter(RouterConfig{MaxProcs: 3, StorePath: storePath})
	t.Cleanup(func() { r.Shutdown() })
	return r
}

func chainOf(t *testing.T, r *Router, key string) []string {
	t.Helper()
	r.mu.RLock()
	s := r.sessions[key]
	r.mu.RUnlock()
	if s == nil {
		t.Fatalf("session %q not restored", key)
	}
	return s.SnapshotPrevSessionIDs()
}

// TestAutoChainRetire_RemovesAutoSegmentsKeepsManual: a mixed chain loses its
// auto-* entries on startup but keeps the manual/resume ones, aligned.
func TestAutoChainRetire_RemovesAutoSegmentsKeepsManual(t *testing.T) {
	key := "dashboard:direct:ts-proj:general"
	src := newSessionWithID(key, "cur")
	src.setWorkspace("/srv/proj")
	src.historyMu.Lock()
	src.prevSessionIDs = []string{"real-1", "auto-a", "auto-b", "real-2"}
	src.prevSessionOrigins = []string{"manual", "auto-spawn", "auto-backfill", "resume"}
	src.historyMu.Unlock()

	r := bootRouterWithStore(t, map[string]*ManagedSession{key: src})
	chain := chainOf(t, r, key)
	if len(chain) != 2 || chain[0] != "real-1" || chain[1] != "real-2" {
		t.Errorf("chain = %v, want [real-1 real-2]", chain)
	}
}

// TestAutoChainRetire_AllAutoClearsChain: a fully machine-guessed chain is
// emptied on startup (this is the gaokao-1 / workspace-14 real-data case).
func TestAutoChainRetire_AllAutoClearsChain(t *testing.T) {
	key := "dashboard:direct:ts-ws:general"
	src := newSessionWithID(key, "cur")
	src.setWorkspace("/home/ec2-user/workspace")
	src.historyMu.Lock()
	src.prevSessionIDs = []string{"g1", "g2", "g3"}
	src.prevSessionOrigins = []string{"auto-spawn", "auto-spawn", "auto-backfill"}
	src.historyMu.Unlock()

	r := bootRouterWithStore(t, map[string]*ManagedSession{key: src})
	if chain := chainOf(t, r, key); len(chain) != 0 {
		t.Errorf("chain = %v, want empty", chain)
	}
}

// TestAutoChainRetire_PureManualUntouched: a real rotation chain (no auto
// origins) survives startup verbatim — the retire must not touch it.
func TestAutoChainRetire_PureManualUntouched(t *testing.T) {
	key := "dashboard:direct:ts-keep:general"
	src := newSessionWithID(key, "cur")
	src.setWorkspace("/srv/keep")
	src.historyMu.Lock()
	src.prevSessionIDs = []string{"m1", "m2", "m3"}
	src.prevSessionOrigins = []string{"manual", "manual", "resume"}
	src.historyMu.Unlock()

	r := bootRouterWithStore(t, map[string]*ManagedSession{key: src})
	chain := chainOf(t, r, key)
	if len(chain) != 3 || chain[0] != "m1" || chain[2] != "m3" {
		t.Errorf("chain = %v, want [m1 m2 m3]", chain)
	}
}

// TestAutoChainRetire_Idempotent: booting twice from a store that already had
// auto segments stripped leaves the chain unchanged on the second boot (no
// auto origins remain to strip). We simulate by retiring on an in-memory
// router twice.
func TestAutoChainRetire_Idempotent(t *testing.T) {
	key := "dashboard:direct:ts-idem:general"
	src := newSessionWithID(key, "cur")
	src.setWorkspace("/srv/idem")
	src.historyMu.Lock()
	src.prevSessionIDs = []string{"real", "auto-x"}
	src.prevSessionOrigins = []string{"manual", "auto-spawn"}
	src.historyMu.Unlock()

	r := NewRouter(RouterConfig{MaxProcs: 3})
	t.Cleanup(func() { r.Shutdown() })
	r.mu.Lock()
	r.sessions[key] = src
	r.mu.Unlock()

	r.retireAutoChainOnce()
	first := src.SnapshotPrevSessionIDs()
	if len(first) != 1 || first[0] != "real" {
		t.Fatalf("after first retire chain = %v, want [real]", first)
	}
	// Second run: nothing to strip.
	r.retireAutoChainOnce()
	second := src.SnapshotPrevSessionIDs()
	if len(second) != 1 || second[0] != "real" {
		t.Errorf("after second retire chain = %v, want [real] (idempotent)", second)
	}
}
