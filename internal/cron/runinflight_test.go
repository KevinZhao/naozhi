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

// TestRunInflight_SetSessionIDEmptyIsNoop pins the contract that the
// R242-ARCH-22 (#766) early-set hook in scheduler_run.go relies on:
// calling setSessionID("") right after GetOrCreate when sess.SessionID()
// is still empty (fresh-mode CLI mid-handshake) MUST NOT clobber the
// later post-Send setSessionID(result.SessionID) write. Without this
// contract the early hook would race the post-Send write and leave the
// inflight view's SessionID empty for the run's lifetime, defeating the
// entire purpose of the early-set fix (KnownSessionIDs miss during the
// Send window). Same-value writes also fast-path so the post-Send call
// stays cheap when sess.SessionID() already matched.
func TestRunInflight_SetSessionIDEmptyIsNoop(t *testing.T) {
	inf := &runInflight{}
	inf.running.Store(true)
	inf.populate(runInflightView{RunID: "r1", Phase: PhaseSpawning})

	// Empty-id call must be a no-op — used by the post-GetOrCreate early
	// hook on fresh-mode runs (sess.SessionID() == "" until init turn lands).
	inf.setSessionID("")
	if v, _ := inf.snapshot(); v.SessionID != "" {
		t.Errorf("setSessionID(\"\") leaked: got %q", v.SessionID)
	}

	// Real id from GetOrCreate (persistent-mode reuse path).
	inf.setSessionID("sess-early")
	if v, _ := inf.snapshot(); v.SessionID != "sess-early" {
		t.Errorf("first non-empty setSessionID lost: got %q", v.SessionID)
	}

	// Empty after a real id is also a no-op (defends against a hypothetical
	// future caller racing the early hook with a half-cleared session).
	inf.setSessionID("")
	if v, _ := inf.snapshot(); v.SessionID != "sess-early" {
		t.Errorf("setSessionID(\"\") clobbered prior value: got %q", v.SessionID)
	}

	// Same id again — fast-path skips Store. View pointer stays the same.
	before := inf.view.Load()
	inf.setSessionID("sess-early")
	after := inf.view.Load()
	if before != after {
		t.Errorf("same-value setSessionID must skip Store: before=%p after=%p", before, after)
	}

	// Replacing with a different id IS authoritative — the post-Send
	// path writes result.SessionID which can differ from sess.SessionID()
	// if the CLI handshake assigned a new id mid-turn.
	inf.setSessionID("sess-final")
	if v, _ := inf.snapshot(); v.SessionID != "sess-final" {
		t.Errorf("replacement setSessionID lost: got %q", v.SessionID)
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

// TestRunInflight_FinalizeReleaseContract pins the live terminal release
// path. R246-CR-017 (#759) once put this contract in a releaseRun method;
// R246-GO-3 (#689) superseded it with the per-run runFinalizer (see the
// anchor comment in runinflight.go), so the contract is now: after
// finalize(), the CAS gate is released and snapshot() returns ok=false
// (so list handlers stop surfacing stale RunID/Phase), and the gauge
// Add(-1) at the defer site pairs with the CAS-true Add(+1). reset
// happens BEFORE CAS-release inside finalize (R238-GO-2 ordering).
func TestRunInflight_FinalizeReleaseContract(t *testing.T) {
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

	// Get-or-New: expvar.NewInt panics on re-registration, and `go test
	// -count>1` (the RFC §3.4 Phase C race gate runs -count=10) re-enters
	// this test in the same process — the expvar registry is global and
	// survives iterations. Reuse is sound: each iteration nets the gauge
	// back to 0 via the paired Add(1)/Add(-1) below.
	gaugeName := "test_finalize_release_gauge_" + t.Name()
	gauge, _ := expvar.Get(gaugeName).(*expvar.Int)
	if gauge == nil {
		gauge = expvar.NewInt(gaugeName)
	}
	gauge.Add(1) // mirror executeOpt's CAS-true Add(+1).

	if v, ok := inf.snapshot(); !ok || v.RunID == "" {
		t.Fatalf("precondition: snapshot before release must be live: ok=%v v=%+v", ok, v)
	}

	// Live path: per-run finalizer does reset + CAS-release; the gauge
	// Add(-1) is paired at the scheduler_run.go defer site.
	finalizer := &runFinalizer{inflight: inf}
	finalizer.finalize()
	gauge.Add(-1)

	if inf.running.Load() {
		t.Errorf("finalize must Store(false) on running CAS gate")
	}
	if v, ok := inf.snapshot(); ok || v.RunID != "" {
		t.Errorf("finalize must reset view (no stale metadata): ok=%v v=%+v", ok, v)
	}
	if got := gauge.Value(); got != 0 {
		t.Errorf("gauge must decrement to 0: got=%d", got)
	}
}

// TestRunInflight_FinalizeNilSafe locks in the nil-safety of the live
// release path: a nil finalizer and a finalizer over a nil inflight must
// both no-op rather than panic (mirrors reset / populate nil-receiver).
func TestRunInflight_FinalizeNilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("finalize on nil finalizer / nil inflight panicked: %v", r)
		}
	}()
	var nilFinalizer *runFinalizer
	nilFinalizer.finalize() // nil finalizer

	(&runFinalizer{inflight: nil}).finalize() // nil inflight

	live := &runInflight{}
	live.running.Store(true)
	live.populate(runInflightView{RunID: "y"})
	(&runFinalizer{inflight: live}).finalize() // must still reset + release CAS
	if live.running.Load() {
		t.Errorf("finalize must still release CAS gate")
	}
	if _, ok := live.snapshot(); ok {
		t.Errorf("finalize must still reset view")
	}
}
