package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/datadir"
	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// TestRouter_SessionRunsHealth_MapsTheStoreCounters: the router's snapshot is
// what /health's run_stores.session serves (#2792), so it must carry the
// store's real state — disabled when nothing persists, enabled with its loss
// counters otherwise. Goes through Runs().Health(), the RunLedger facet
// accessor: a nil Router must report disabled rather than panic.
func TestRouter_SessionRunsHealth_MapsTheStoreCounters(t *testing.T) {
	disabled := &Router{ss: newSessionTable(), runs: RunLedger{runs: runhistory.NewStore("", 0, 0)}}
	if h := disabled.Runs().Health(); h.Enabled {
		t.Errorf("no-persistence router reported %+v, want Enabled=false", h)
	}
	var nilRouter *Router
	if h := nilRouter.Runs().Health(); h.Enabled {
		t.Error("nil router must report disabled")
	}
	if l := nilRouter.Runs().CostLedger(); l != nil {
		t.Errorf("nil router CostLedger() = %v, want nil", l)
	}

	root := filepath.Join(t.TempDir(), "session-runs")
	store := runhistory.NewStore(root, 5, time.Hour)
	t.Cleanup(store.Close)
	r := &Router{ss: newSessionTable(), runs: RunLedger{runs: store}}
	if h := r.Runs().Health(); !h.Enabled || h.WriteFailedOther != 0 {
		t.Fatalf("fresh store = %+v, want enabled with zero counters", h)
	}

	// One real write failure: land a first record so the session's dir exists,
	// then make that dir unwritable. Zero counters alone could not tell a
	// correct mapping from one that swaps diskFull and other.
	const key = "dashboard:direct:health"
	store.Append(runhistory.SessionRun{SessionKey: key, RunID: "aaaa0001", StartedAt: time.Now()})
	ents, err := os.ReadDir(root)
	if err != nil || len(ents) != 1 {
		t.Fatalf("expected one session dir: %v %d", err, len(ents))
	}
	dir := filepath.Join(root, ents[0].Name())
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()
	store.Append(runhistory.SessionRun{SessionKey: key, RunID: "aaaa0002", StartedAt: time.Now()})
	if _, err := os.Stat(filepath.Join(dir, "aaaa0002.json")); err == nil {
		t.Skip("write into a 0500 dir succeeded (running as root?); failure path not exercised")
	}

	if h := r.Runs().Health(); h.WriteFailedOther != 1 || h.WriteFailedDiskFull != 0 {
		t.Errorf("snapshot = %+v, want write_failed_other=1 disk_full=0", h)
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
