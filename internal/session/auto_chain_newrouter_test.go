package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
)

// TestNewRouter_AutoChainBackfillEndToEnd is the full-stack integration
// test: NewRouter restores a session whose prev_session_ids is empty,
// scans the workspace's claude project directory for sibling JSONL
// files, and back-fills prev_session_ids before the Tier 1 / Tier 2
// async history loaders kick off.
//
// Pins the §4.4-B ordering contract: backfill MUST run synchronously
// before Tier 2 launches, otherwise Tier 2 would observe an empty chain
// and dashboard pagination would stop at the current sessionID.
//
// Uses real filesystem (claudeDir + sessions.json + JSONL), not the
// fake listJSONL hook, so the wiring through discovery.ListWorkspaceJSONL
// is exercised end-to-end.
func TestNewRouter_AutoChainBackfillEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	claudeDir := filepath.Join(tmp, "claude")
	storePath := filepath.Join(tmp, "sessions.json")
	workspace := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	// Create the projects/<slug>/ dir with three JSONL files: one is
	// the current session (target), two are older candidates.
	slug := strings.ReplaceAll(workspace, "/", "-")
	projDir := filepath.Join(claudeDir, "projects", slug)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir projDir: %v", err)
	}
	now := time.Now()
	target := "00000000-0000-4000-8000-aaaaaaaaaaaa"
	cand1 := "00000000-0000-4000-8000-000000000001"
	cand2 := "00000000-0000-4000-8000-000000000002"
	for id, age := range map[string]time.Duration{
		cand1:  -3 * time.Hour,
		cand2:  -2 * time.Hour,
		target: -time.Hour,
	} {
		path := filepath.Join(projDir, id+".jsonl")
		if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
			t.Fatalf("write jsonl: %v", err)
		}
		mt := now.Add(age)
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	// Persist a session with empty prev_session_ids so the backfill
	// path is the only thing that can populate it.
	saved := map[string]*ManagedSession{
		"dashboard:direct:user:general": func() *ManagedSession {
			s := newSessionWithID("dashboard:direct:user:general", target)
			s.setWorkspace(workspace)
			s.lastActive.Store(now.Add(-30 * time.Minute).UnixNano())
			return s
		}(),
	}
	if err := saveStore(storePath, saved); err != nil {
		t.Fatalf("saveStore: %v", err)
	}

	// Boot the router — auto-chain default-on, window 7d, cap 32.
	r := NewRouter(RouterConfig{
		MaxProcs:  3,
		StorePath: storePath,
		ClaudeDir: claudeDir,
		AutoChainPolicy: GlobalAutoChainPolicy{
			EnabledFlag: true,
			WindowDur:   7 * 24 * time.Hour,
			CapValue:    32,
		},
	})
	t.Cleanup(func() { r.Shutdown() })

	r.mu.RLock()
	s := r.sessions["dashboard:direct:user:general"]
	r.mu.RUnlock()
	if s == nil {
		t.Fatal("session not restored")
	}

	chain := s.SnapshotChainIDs()
	want := []string{cand1, cand2, target}
	if len(chain) != len(want) {
		t.Fatalf("chain = %v, want %v", chain, want)
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Errorf("chain[%d] = %s, want %s", i, chain[i], want[i])
		}
	}

	// Origin labels must be auto-backfill on the two newly-prepended
	// entries. SnapshotPrevSessionOrigins is parallel to prev_session_ids
	// (does NOT include current).
	origins := s.SnapshotPrevSessionOrigins()
	if len(origins) != 2 {
		t.Fatalf("origins len = %d, want 2 (%v)", len(origins), origins)
	}
	if origins[0] != "auto-backfill" || origins[1] != "auto-backfill" {
		t.Errorf("origins = %v, want [auto-backfill auto-backfill]", origins)
	}
}

// TestNewRouter_AutoChainDisabledLeaveStoreUntouched: when the policy
// is disabled at config time, NewRouter must not touch any session's
// prev_session_ids. Pins the opt-out path.
func TestNewRouter_AutoChainDisabledLeaveStoreUntouched(t *testing.T) {
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "sessions.json")
	saved := map[string]*ManagedSession{
		"dashboard:direct:user:general": newSessionWithID("dashboard:direct:user:general", "00000000-0000-4000-8000-aaaaaaaaaaaa"),
	}
	if err := saveStore(storePath, saved); err != nil {
		t.Fatalf("saveStore: %v", err)
	}

	r := NewRouter(RouterConfig{
		MaxProcs:  3,
		StorePath: storePath,
		AutoChainPolicy: GlobalAutoChainPolicy{
			EnabledFlag: false,
			WindowDur:   7 * 24 * time.Hour,
			CapValue:    32,
		},
		// Stub out listJSONL so even if the policy gate failed open we
		// would still fall through to a no-op.
		AutoChainListJSONL: staticListJSONL(map[string][]discovery.WorkspaceJSONL{}),
	})
	t.Cleanup(func() { r.Shutdown() })

	r.mu.RLock()
	s := r.sessions["dashboard:direct:user:general"]
	r.mu.RUnlock()
	if got := len(s.prevSessionIDs); got != 0 {
		t.Errorf("disabled policy must leave prev empty; got len=%d (%v)", got, s.prevSessionIDs)
	}
}
