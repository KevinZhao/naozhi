package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/session/runhistory"
)

// TestRouter_SessionRunsHealth_MapsTheStoreCounters: the router's snapshot is
// what /health's run_stores.session serves (#2792), so it must carry the
// store's real state — disabled when nothing persists, enabled with its loss
// counters otherwise.
func TestRouter_SessionRunsHealth_MapsTheStoreCounters(t *testing.T) {
	disabled := &Router{sessionRuns: runhistory.NewStore("", 0, 0)}
	if h := disabled.SessionRunsHealth(); h.Enabled {
		t.Errorf("no-persistence router reported %+v, want Enabled=false", h)
	}
	var nilRouter *Router
	if h := nilRouter.SessionRunsHealth(); h.Enabled {
		t.Error("nil router must report disabled")
	}

	root := filepath.Join(t.TempDir(), "session-runs")
	store := runhistory.NewStore(root, 5, time.Hour)
	t.Cleanup(store.Close)
	r := &Router{sessionRuns: store}
	if h := r.SessionRunsHealth(); !h.Enabled || h.WriteFailedOther != 0 {
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

	if h := r.SessionRunsHealth(); h.WriteFailedOther != 1 || h.WriteFailedDiskFull != 0 {
		t.Errorf("snapshot = %+v, want write_failed_other=1 disk_full=0", h)
	}
}
