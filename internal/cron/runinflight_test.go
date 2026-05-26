package cron

import (
	"expvar"
	"sync"
	"testing"
	"time"
)

// TestRunInflight_SnapshotAtomic verifies that snapshot() returns a
// self-consistent view even when a writer interleaves field updates.
// R238-ARCH-3 (#742): the prior 6-pointer layout could return a torn
// snapshot (e.g. RunID from run N alongside Phase from run N+1). The
// new atomic.Pointer[runInflightView] layout makes that structurally
// impossible — every snapshot is exactly some Stored view.
//
// We exercise this with a writer goroutine that alternates between two
// fully-populated views (A/B) and a reader goroutine that snapshots
// repeatedly, asserting the observed combination of fields always
// matches one of the two source views (no field-level mixing).
func TestRunInflight_SnapshotAtomic(t *testing.T) {
	if testing.Short() {
		t.Skip("skip race scenario in short mode")
	}
	inf := &runInflight{}
	if !inf.running.CompareAndSwap(false, true) {
		t.Fatal("CAS")
	}

	viewA := runInflightView{
		RunID:     "aaaaaaaaaaaaaaaa",
		StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Phase:     PhaseQueued,
		Trigger:   TriggerScheduled,
		SessionID: "sess-A",
		Fresh:     true,
	}
	viewB := runInflightView{
		RunID:     "bbbbbbbbbbbbbbbb",
		StartedAt: time.Date(2026, 6, 6, 6, 6, 6, 0, time.UTC),
		Phase:     PhaseSending,
		Trigger:   TriggerManual,
		SessionID: "sess-B",
		Fresh:     false,
	}
	// Seed so the first reader sample is one of the two known views (not
	// the all-zero default).
	inf.populate(viewA)

	const iters = 5000
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: alternate between A and B as fast as possible.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if i%2 == 0 {
				inf.populate(viewA)
			} else {
				inf.populate(viewB)
			}
		}
		close(stop)
	}()

	// Reader: snapshot until writer signals done; every observed view
	// must match A or B exactly (no field crossing).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			v, ok := inf.snapshot()
			if !ok {
				t.Errorf("running gate must stay open: ok=false")
				return
			}
			if v != viewA && v != viewB {
				t.Errorf("torn snapshot: %+v matches neither A=%+v nor B=%+v", v, viewA, viewB)
				return
			}
		}
	}()

	wg.Wait()
}

// TestRunInflight_ResetClearsView ensures reset() drops the view but
// keeps the running flag (matching the executeOpt defer ordering, where
// reset runs before running.Store(false)). Subsequent snapshot returns
// ok=true with zero fields until running flips off.
func TestRunInflight_ResetClearsView(t *testing.T) {
	inf := &runInflight{}
	if !inf.running.CompareAndSwap(false, true) {
		t.Fatal("CAS")
	}
	inf.populate(runInflightView{RunID: "abc", Phase: PhaseSending})
	if v, ok := inf.snapshot(); !ok || v.RunID != "abc" {
		t.Fatalf("populated snapshot wrong: ok=%v v=%+v", ok, v)
	}
	inf.reset()
	v, ok := inf.snapshot()
	if !ok {
		t.Fatal("reset alone must not flip running")
	}
	if (v != runInflightView{}) {
		t.Errorf("reset must zero observable fields, got %+v", v)
	}
	inf.running.Store(false)
	if _, ok := inf.snapshot(); ok {
		t.Error("running=false must yield ok=false")
	}
}

// TestRunInflight_SetPhaseFastPath ensures setPhase is a no-op when the
// phase is unchanged (preserving the cache-line write economy of the
// pre-refactor implementation).
func TestRunInflight_SetPhaseFastPath(t *testing.T) {
	inf := &runInflight{}
	inf.running.Store(true)
	inf.populate(runInflightView{Phase: PhaseQueued, RunID: "x"})
	before := inf.view.Load()
	inf.setPhase(PhaseQueued) // same — fast path skips Store
	after := inf.view.Load()
	if before != after {
		t.Errorf("setPhase with unchanged value must not Store: before=%p after=%p", before, after)
	}
	inf.setPhase(PhaseSending) // change — must Store
	v, ok := inf.snapshot()
	if !ok || v.Phase != PhaseSending || v.RunID != "x" {
		t.Errorf("phase update lost siblings: ok=%v v=%+v", ok, v)
	}
}

// TestRunInflight_ReleaseRun_ContractOrder pins the 3-step terminal
// release contract that R246-CR-017 (#759) extracted into releaseRun:
// after the call, the CAS gate must be released, snapshot() must return
// ok=false (so list handlers stop surfacing stale RunID/Phase), and the
// inflight gauge must have decremented once. Order matters — see
// releaseRun's godoc and R238-GO-2.
func TestRunInflight_ReleaseRun_ContractOrder(t *testing.T) {
	inf := &runInflight{}
	if !inf.running.CompareAndSwap(false, true) {
		t.Fatal("CAS")
	}
	inf.populate(runInflightView{
		RunID:     "aaaaaaaaaaaaaaaa",
		StartedAt: time.Now(),
		Phase:     PhaseSending,
		SessionID: "sess-x",
		Trigger:   TriggerScheduled,
	})

	gauge := expvar.NewInt("test_release_run_gauge_" + t.Name())
	gauge.Add(1) // mirror executeOpt's CAS-true Add(+1).

	if v, ok := inf.snapshot(); !ok || v.RunID == "" {
		t.Fatalf("precondition: snapshot before release must be live: ok=%v v=%+v", ok, v)
	}

	inf.releaseRun(gauge)

	if inf.running.Load() {
		t.Errorf("releaseRun must Store(false) on running CAS gate")
	}
	if v, ok := inf.snapshot(); ok || v.RunID != "" {
		t.Errorf("releaseRun must reset view (no stale metadata): ok=%v v=%+v", ok, v)
	}
	if got := gauge.Value(); got != 0 {
		t.Errorf("releaseRun must decrement gauge: got=%d want=0", got)
	}
}

// TestRunInflight_ReleaseRun_NilSafe locks in the nil-receiver and
// nil-gauge contract documented on releaseRun. Test fixtures that build
// a runInflight without wiring metrics rely on the nil-gauge branch.
func TestRunInflight_ReleaseRun_NilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("releaseRun on nil receiver / nil gauge panicked: %v", r)
		}
	}()
	var inf *runInflight
	inf.releaseRun(nil) // nil receiver

	live := &runInflight{}
	live.running.Store(true)
	live.populate(runInflightView{RunID: "y"})
	live.releaseRun(nil) // nil gauge — must still reset + release CAS
	if live.running.Load() {
		t.Errorf("nil-gauge releaseRun must still release CAS gate")
	}
	if _, ok := live.snapshot(); ok {
		t.Errorf("nil-gauge releaseRun must still reset view")
	}
}
