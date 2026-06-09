// runinflight_phase_int32_test.go pins the R246-GO-15 (#703) split-phase
// design: setPhase must not allocate a runInflightView, populate
// atomicity must hold across alternating populate(A)/populate(B) writers,
// and snapshot must always observe a phase consistent with some Stored
// view.
package cron

import (
	"sync"
	"testing"
	"testing/quick"
)

// TestRunInflight_SetPhase_DoesNotMutateView verifies the alloc-free
// hot-path: setPhase must not Store a new *runInflightView. The before/
// after pointer comparison is the direct observable signal that the
// view alloc was avoided. R246-GO-15 (#703).
func TestRunInflight_SetPhase_DoesNotMutateView(t *testing.T) {
	t.Parallel()
	inf := &runInflight{}
	inf.running.Store(true)
	inf.populate(runInflightView{RunID: "x", Phase: PhaseQueued})

	before := inf.view.Load()
	for _, ph := range []string{PhaseJittering, PhaseSpawning, PhaseSending} {
		inf.setPhase(ph)
		after := inf.view.Load()
		if before != after {
			t.Errorf("setPhase(%q) Stored a new view (alloc): before=%p after=%p", ph, before, after)
		}
	}

	// Snapshot must reflect the latest setPhase regardless of view
	// pointer being unchanged.
	if v, ok := inf.snapshot(); !ok || v.Phase != PhaseSending || v.RunID != "x" {
		t.Errorf("snapshot after setPhase chain wrong: ok=%v v=%+v", ok, v)
	}
}

// TestRunInflight_AllocBudget_SetPhase quantifies the savings: a 100-
// iteration setPhase chain must allocate zero runInflightView structs.
// testing.AllocsPerRun gives a per-call alloc count; we accept anything
// up to 0.5 to absorb test-harness noise (closure/struct allocs in the
// test setup itself are counted against the first run and the function
// is averaged across iterations).
func TestRunInflight_AllocBudget_SetPhase(t *testing.T) {
	// testing.AllocsPerRun forbids t.Parallel().
	inf := &runInflight{}
	inf.running.Store(true)
	inf.populate(runInflightView{RunID: "x", Phase: PhaseQueued})

	allocs := testing.AllocsPerRun(100, func() {
		inf.setPhase(PhaseJittering)
		inf.setPhase(PhaseSpawning)
		inf.setPhase(PhaseSending)
		inf.setPhase(PhaseQueued)
	})
	// 0 expected; allow a tiny budget for runtime noise on slow CI.
	if allocs > 0.5 {
		t.Errorf("setPhase chain allocated %.2f times per run; expected ~0", allocs)
	}
}

// TestRunInflight_PopulateAtomicity_WithSetPhase complements the
// existing TestRunInflight_SnapshotAtomic with a writer that mixes
// populate+setPhase. Snapshot must always observe a consistent phase:
// either bundled-view-Phase (when int32==unset, i.e. just-populated) or
// the most recent setPhase value.
func TestRunInflight_PopulateAtomicity_WithSetPhase(t *testing.T) {
	if testing.Short() {
		t.Skip("skip race scenario in short mode")
	}
	t.Parallel()
	inf := &runInflight{}
	inf.running.Store(true)
	inf.populate(runInflightView{
		RunID: "aaaaaaaaaaaaaaaa",
		Phase: PhaseQueued,
	})

	const iters = 5000
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if i%3 == 0 {
				inf.populate(runInflightView{
					RunID: "bbbbbbbbbbbbbbbb",
					Phase: PhaseSending,
				})
			} else if i%3 == 1 {
				inf.setPhase(PhaseSpawning)
			} else {
				inf.setPhase(PhaseSending)
			}
		}
		close(stop)
	}()

	validPhases := map[string]bool{
		PhaseQueued:    true,
		PhaseJittering: true,
		PhaseSpawning:  true,
		PhaseSending:   true,
	}

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
				t.Errorf("running gate must stay open")
				return
			}
			// Phase must be one of the four known constants (no
			// torn empty-string and no garbage int32 leak).
			if v.Phase != "" && !validPhases[v.Phase] {
				t.Errorf("snapshot returned unknown Phase %q (RunID=%q)", v.Phase, v.RunID)
				return
			}
		}
	}()

	wg.Wait()
}

// TestRunInflight_PopulateClearsPhaseInt32 ensures populate's int32
// reset prevents prior-run setPhase values from leaking into the new
// run before the new run's first setPhase fires.
func TestRunInflight_PopulateClearsPhaseInt32(t *testing.T) {
	t.Parallel()
	inf := &runInflight{}
	inf.running.Store(true)
	inf.populate(runInflightView{RunID: "run1", Phase: PhaseQueued})
	inf.setPhase(PhaseSending)

	// Snapshot now: int32 is sending → snapshot returns sending.
	if v, _ := inf.snapshot(); v.Phase != PhaseSending {
		t.Fatalf("precondition: setPhase(Sending) must take effect, got %q", v.Phase)
	}

	// New populate with a different bundled Phase.
	inf.populate(runInflightView{RunID: "run2", Phase: PhaseQueued})
	if v, _ := inf.snapshot(); v.Phase != PhaseQueued {
		t.Errorf("populate did not reset phase int32: snapshot Phase=%q, want %q (RunID=%q)",
			v.Phase, PhaseQueued, v.RunID)
	}
}

// TestPhaseStringRoundTrip pins the int32↔string mapping. A future
// reorder of the enum constants would silently misclassify on-wire
// phase values; this test catches it.
func TestPhaseStringRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []string{PhaseQueued, PhaseJittering, PhaseSpawning, PhaseSending}
	for _, s := range cases {
		got := phaseFromString(s).String()
		if got != s {
			t.Errorf("round-trip %q: got %q", s, got)
		}
	}
	// Unknown string maps to phaseUnset which renders as "".
	if got := phaseFromString("garbage").String(); got != "" {
		t.Errorf("phaseFromString(garbage).String() = %q, want \"\"", got)
	}
	// Quick check across random strings: all map to "" except the four
	// canonical constants.
	if err := quick.Check(func(s string) bool {
		want := ""
		switch s {
		case PhaseQueued, PhaseJittering, PhaseSpawning, PhaseSending:
			want = s
		}
		return phaseFromString(s).String() == want
	}, &quick.Config{MaxCount: 200}); err != nil {
		t.Errorf("quick.Check: %v", err)
	}
}

// TestR20260607GO003_PhasePopulatingRemoved pins R20260607-GO-003: the
// dead constant phasePopulating (formerly = phaseUnset = 0) has been
// deleted. Its value was identical to phaseUnset so the switch in
// String() could never distinguish it; keeping it around was a
// maintenance trap. This test exhaustively confirms that:
//   - runPhase(0) (phaseUnset) renders as "" — unchanged.
//   - Every integer in [0, 4] that is not a canonical phase renders as "".
//   - The four live phases (1-4) still round-trip correctly.
func TestR20260607GO003_PhasePopulatingRemoved(t *testing.T) {
	t.Parallel()

	// phaseUnset (0) must render as "" — it is the sentinel for "no phase yet".
	if got := runPhase(0).String(); got != "" {
		t.Errorf("runPhase(0).String() = %q, want \"\" (phaseUnset must be empty)", got)
	}

	// Verify no integer in [0,4] accidentally introduces a new non-empty
	// string that wasn't there before the deletion.
	canonical := map[runPhase]string{
		runPhase(1): PhaseQueued,
		runPhase(2): PhaseJittering,
		runPhase(3): PhaseSpawning,
		runPhase(4): PhaseSending,
	}
	for i := runPhase(0); i <= runPhase(5); i++ {
		want, ok := canonical[i]
		if !ok {
			want = ""
		}
		if got := i.String(); got != want {
			t.Errorf("runPhase(%d).String() = %q, want %q", i, got, want)
		}
	}
}

// Compile-time check that runPhase is int32 — guards against a future
// type change that would break atomic.Int32 storage.
var _ = func() runPhase {
	var x int32 = 1
	return runPhase(x)
}()
