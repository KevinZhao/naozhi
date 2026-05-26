package session

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
)

// TestPendingExcluders_DefaultFalse pins the backward-compatibility
// invariant: the R242-ARCH-16 (#760) startup gate is OPT-IN. A Router
// constructed without calling SetPendingExcluders must report
// excludersPending() == false so existing tests and pre-#760 cmd wiring
// keep their pre-fix behaviour.
func TestPendingExcluders_DefaultFalse(t *testing.T) {
	r := makeRouterForAutoChain(t)
	if r.excludersPending() {
		t.Fatal("default pendingExcluders must be false; gate is opt-in")
	}
}

// TestSetPendingExcluders_FlipBoth pins SetPendingExcluders as a plain
// atomic toggle. Used by cmd-side wiring as set(true) → register
// excluders → set(false) → RunAutoChainBackfillOnce.
func TestSetPendingExcluders_FlipBoth(t *testing.T) {
	r := makeRouterForAutoChain(t)
	r.SetPendingExcluders(true)
	if !r.excludersPending() {
		t.Fatal("after SetPendingExcluders(true), excludersPending() must report true")
	}
	r.SetPendingExcluders(false)
	if r.excludersPending() {
		t.Fatal("after SetPendingExcluders(false), excludersPending() must report false")
	}
}

// TestRunAutoChainBackfillOnce_SkipsWhenPending is the headline #760 fix
// invariant: while the startup gate is set, the backfill path skips
// without applying ANY chain. Without the gate, an untimely backfill
// running against a half-registered excluder set could persist an
// internal sessionID into a user-facing prev_session_ids chain.
func TestRunAutoChainBackfillOnce_SkipsWhenPending(t *testing.T) {
	r := makeRouterForAutoChain(t)
	r.SetPendingExcluders(true)

	now := time.Now()
	ws := "/home/test/ws"
	target := "00000000-0000-4000-8000-aaaaaaaaaaaa"
	candidate := "00000000-0000-4000-8000-000000000001"

	s := &ManagedSession{key: "dashboard:direct:user:general"}
	s.setWorkspace(ws)
	s.setSessionID(target)
	s.lastActive.Store(now.Add(-time.Hour).UnixNano())
	r.sessions[s.key] = s

	r.autoChainListJSONL = staticListJSONL(map[string][]discovery.WorkspaceJSONL{
		ws: {
			{SessionID: candidate, Mtime: now.Add(-2 * time.Hour).UnixMilli()},
			{SessionID: target, Mtime: now.UnixMilli()},
		},
	})

	// With gate set: must NOT backfill anything.
	r.runAutoChainBackfillOnce()
	if got := s.SnapshotChainIDs(); len(got) != 1 || got[0] != target {
		t.Fatalf("backfill ran while gate was set: chain=%v want=[%s]", got, target)
	}

	// Flip gate down and re-run via the exported trigger.
	r.SetPendingExcluders(false)
	r.RunAutoChainBackfillOnce()

	got := s.SnapshotChainIDs()
	want := []string{candidate, target}
	if len(got) != len(want) {
		t.Fatalf("after flipping gate, chain len = %d (%v), want %d (%v)",
			len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("chain[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

// TestMaybeAttachAutoChainOnSpawn_SkipsWhenPending pins the spawn-path
// half of #760. While the gate is set, maybeAttachAutoChainOnSpawn
// returns nil (no chain) regardless of how many candidates the workspace
// scanner would otherwise surface.
//
// We exercise the function indirectly via a helper that mirrors the
// real spawn call site's preconditions: empty prevIDs, empty oldHistory,
// non-cron/sys/scratch key, enabled policy, non-empty workspace.
func TestMaybeAttachAutoChainOnSpawn_SkipsWhenPending(t *testing.T) {
	r := makeRouterForAutoChain(t)

	now := time.Now()
	ws := "/home/test/ws"
	candidate := "00000000-0000-4000-8000-000000000001"

	r.autoChainListJSONL = staticListJSONL(map[string][]discovery.WorkspaceJSONL{
		ws: {{SessionID: candidate, Mtime: now.UnixMilli()}},
	})

	// Sanity: gate down → attach returns the candidate.
	got := r.maybeAttachAutoChainOnSpawn(
		"dashboard:direct:user:general",
		ws,
		nil, // no prev
		nil, // no history
	)
	if len(got) != 1 || got[0] != candidate {
		t.Fatalf("baseline (gate down) attach = %v, want [%s]", got, candidate)
	}

	// Gate up → attach must skip and return nil.
	r.SetPendingExcluders(true)
	got = r.maybeAttachAutoChainOnSpawn(
		"dashboard:direct:user:general",
		ws,
		nil,
		nil,
	)
	if got != nil {
		t.Errorf("gate set: maybeAttachAutoChainOnSpawn must return nil, got %v", got)
	}
}
