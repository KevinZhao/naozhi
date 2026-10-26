package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// TestRouter_NilRouterCostLedgerIsNil: the RunLedger facet of a nil Router is
// nil-safe, matching History()'s accessors.
func TestRouter_NilRouterCostLedgerIsNil(t *testing.T) {
	var nilRouter *Router
	if l := nilRouter.Runs().CostLedger(); l != nil {
		t.Errorf("nil router CostLedger() = %v, want nil", l)
	}
}

// TestRouter_RemoveFreesRunHistoryRing: Remove drops the session's resident
// run-history ring (on-disk records stay), so the per-session ring map stays
// bounded. Observed through staleness: a record another writer put on disk
// after the ring warmed is invisible until the ring is freed and re-warmed.
func TestRouter_RemoveFreesRunHistoryRing(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	r := NewRouter(RouterConfig{MaxProcs: 2, StorePath: storePath})
	t.Cleanup(r.Shutdown)
	const key = "test:direct:ring:general"
	installSession(t, r, key, nil)

	start := time.Now().Add(-time.Hour)
	run := func(id string, d int64) runhistory.SessionRun {
		return runhistory.SessionRun{RunID: id, SessionKey: key, StartedAt: start,
			EndedAt: start.Add(time.Duration(d) * time.Millisecond), DurationMS: d,
			Outcome: runhistory.OutcomeCompleted}
	}
	r.runs.runs.Append(run("00000000000000a1", 100)) // warms the ring
	other := runhistory.NewStore(datadir.ForStore(storePath).SessionRunsRoot(), 0, 0)
	other.Append(run("00000000000000a2", 200))
	other.Close()
	if got := r.Runs().List(key, 0, time.Time{}); len(got) != 1 {
		t.Fatalf("before Remove: %d runs, want the 1 resident in the warmed ring", len(got))
	}

	if !r.Remove(key) {
		t.Fatal("Remove returned false")
	}
	if got := r.Runs().List(key, 0, time.Time{}); len(got) != 2 {
		t.Errorf("after Remove: %d runs, want 2 re-warmed from disk (ring not freed?)", len(got))
	}
}
