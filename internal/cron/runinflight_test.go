package cron

import (
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
// Send window).
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

	// Replacing with a different id IS authoritative — the post-Send
	// path writes result.SessionID which can differ from sess.SessionID()
	// if the CLI handshake assigned a new id mid-turn.
	inf.setSessionID("sess-final")
	if v, _ := inf.snapshot(); v.SessionID != "sess-final" {
		t.Errorf("replacement setSessionID lost: got %q", v.SessionID)
	}
}

// TestRunInflight_PopulateClearsPhaseInt32 ensures populate's int32 reset
// prevents prior-run setPhase values from leaking into the new run before
// the new run's first setPhase fires.
func TestRunInflight_PopulateClearsPhaseInt32(t *testing.T) {
	t.Parallel()
	inf := &runInflight{}
	inf.running.Store(true)
	inf.populate(runInflightView{RunID: "run1", Phase: PhaseQueued})
	inf.setPhase(PhaseSending)
	if v, _ := inf.snapshot(); v.Phase != PhaseSending {
		t.Fatalf("precondition: setPhase(Sending) must take effect, got %q", v.Phase)
	}

	inf.populate(runInflightView{RunID: "run2", Phase: PhaseQueued})
	if v, _ := inf.snapshot(); v.Phase != PhaseQueued {
		t.Errorf("populate did not reset phase int32: snapshot Phase=%q, want %q (RunID=%q)",
			v.Phase, PhaseQueued, v.RunID)
	}
}

// TestPhaseStringRoundTrip pins the int32<->string mapping of the on-wire
// phase values (the dashboard badge reads them); unknown strings and
// non-canonical ints render as "".
func TestPhaseStringRoundTrip(t *testing.T) {
	t.Parallel()
	for _, s := range []string{PhaseQueued, PhaseJittering, PhaseSpawning, PhaseSending} {
		if got := phaseFromString(s).String(); got != s {
			t.Errorf("round-trip %q: got %q", s, got)
		}
	}
	if got := phaseFromString("garbage").String(); got != "" {
		t.Errorf("phaseFromString(garbage).String() = %q, want \"\"", got)
	}
	for _, i := range []runPhase{0, 5} {
		if got := i.String(); got != "" {
			t.Errorf("runPhase(%d).String() = %q, want \"\"", i, got)
		}
	}
}
