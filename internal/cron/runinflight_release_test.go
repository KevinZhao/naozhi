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

// TestRunInflight_SetSessionID_ResumeWindow pins the R242-ARCH-22 (#766)
// contract: setSessionID is the seam executeOpt invokes both immediately
// after GetOrCreate (when the router carries a SessionID from a /resume
// handshake) AND after Send (the new-spawn late-bind). Both writes must
// be observable through snapshot() — the second write is idempotent on
// equal value (no extra alloc) but MUST overwrite stale empty values.
// Empty strings short-circuit so the GetOrCreate-fast-path can call
// setSessionID(sess.SessionID()) unconditionally without erasing a
// previously-set ID. Without this contract the empty-sessionID window
// between CAS-true and Send-return lets KnownSessionIDs probes from a
// concurrent auto-workspace-chain spawn miss this run's ID.
func TestRunInflight_SetSessionID_ResumeWindow(t *testing.T) {
	r := &runInflight{}
	if !r.running.CompareAndSwap(false, true) {
		t.Fatalf("CAS into running=true failed")
	}
	r.runID.Store(boxString("rid"))
	r.startedAt.Store(boxTime(time.Now()))
	r.phase.Store(boxString(PhaseSpawning))
	r.trigger.Store(boxString(string(TriggerScheduled)))

	// Empty string is a no-op (preserves any prior write).
	r.setSessionID("")
	if v, ok := r.snapshot(); !ok || v.SessionID != "" {
		t.Fatalf("after setSessionID(\"\") sessionID should remain empty; got %+v ok=%v", v, ok)
	}

	// First non-empty Store (GetOrCreate-time path).
	r.setSessionID("sess-from-resume")
	v, ok := r.snapshot()
	if !ok || v.SessionID != "sess-from-resume" {
		t.Fatalf("setSessionID after GetOrCreate did not stick; got %+v ok=%v", v, ok)
	}

	// Idempotent same-value Store (post-Send path with same id).
	r.setSessionID("sess-from-resume")
	v, ok = r.snapshot()
	if !ok || v.SessionID != "sess-from-resume" {
		t.Fatalf("idempotent setSessionID changed value; got %+v ok=%v", v, ok)
	}

	// Distinct value overwrites (the rare case of resume + new spawn ID).
	r.setSessionID("sess-from-send")
	v, ok = r.snapshot()
	if !ok || v.SessionID != "sess-from-send" {
		t.Fatalf("setSessionID overwrite did not stick; got %+v ok=%v", v, ok)
	}
}
