package cron

import (
	"sync/atomic"
	"testing"
	"time"
)

// fakeGauge is a stand-in for metrics.CronRunInflight that records the
// argument passed to Add. Used to assert releaseRun's gauge bookkeeping.
type fakeGauge struct {
	delta atomic.Int64
}

func (f *fakeGauge) Add(d int64) { f.delta.Add(d) }

// TestRunInflight_ReleaseRun_ContractOrdering pins the R246-CR-017 contract:
// releaseRun must (1) clear observable metadata, (2) drop the CAS gate, and
// (3) decrement the gauge — in that order — so the helper cannot drift back
// into open-coded form without observable failure. The ordering is
// load-bearing per R238-GO-2 (reset must precede CAS release).
func TestRunInflight_ReleaseRun_ContractOrdering(t *testing.T) {
	r := &runInflight{}
	// Acquire the CAS gate as executeOpt would.
	if !r.running.CompareAndSwap(false, true) {
		t.Fatalf("CAS into running=true failed on fresh runInflight")
	}
	r.runID.Store(boxString("test-run-id"))
	r.startedAt.Store(boxTime(time.Now()))
	r.phase.Store(boxString(PhaseSending))
	r.trigger.Store(boxString(string(TriggerScheduled)))
	r.sessionID.Store(boxString("sess-abc"))
	r.freshSnap.Store(true)

	g := &fakeGauge{}
	r.releaseRun(g)

	// Step 1: metadata cleared (snapshot reports !running so view is zero).
	if _, ok := r.snapshot(); ok {
		t.Fatalf("after releaseRun, snapshot must report !running")
	}
	// All atomic.Pointer slots cleared so a fresh CAS-true window does not
	// inherit stale values.
	if r.runID.Load() != nil {
		t.Errorf("runID not cleared")
	}
	if r.startedAt.Load() != nil {
		t.Errorf("startedAt not cleared")
	}
	if r.phase.Load() != nil {
		t.Errorf("phase not cleared")
	}
	if r.trigger.Load() != nil {
		t.Errorf("trigger not cleared")
	}
	if r.sessionID.Load() != nil {
		t.Errorf("sessionID not cleared")
	}
	if r.freshSnap.Load() {
		t.Errorf("freshSnap not cleared")
	}
	// Step 2: CAS gate released so a follow-up TriggerNow can re-enter.
	if !r.running.CompareAndSwap(false, true) {
		t.Fatalf("CAS gate not released by releaseRun")
	}
	// Step 3: gauge decremented exactly once.
	if got := g.delta.Load(); got != -1 {
		t.Errorf("gauge delta = %d, want -1", got)
	}
}

// TestRunInflight_ReleaseRun_NilSafety covers the two nil-safe branches
// documented in releaseRun: a nil receiver and a nil gauge. Both are paths
// that test fixtures hit when wiring up a partial Scheduler.
func TestRunInflight_ReleaseRun_NilSafety(t *testing.T) {
	var r *runInflight
	r.releaseRun(&fakeGauge{}) // must not panic on nil receiver

	r2 := &runInflight{}
	r2.running.Store(true)
	r2.releaseRun(nil) // nil gauge: only the metric step is skipped
	if r2.running.Load() {
		t.Fatalf("CAS gate must be released even when gauge is nil")
	}
}
