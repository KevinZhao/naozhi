package cron

import (
	"testing"
)

// TestOverlapSkip_EmitsPairedStartedEnded freezes the #870 OverlapPolicy
// contract: when executeOpt loses the per-jobID inflight CAS (a second run for
// the same job while the first is still in flight), it must take the
// overlap-skip path and emit a PAIRED cron_run_started + cron_run_ended frame
// (via emitOverlapSkipped -> emitSyntheticSkipped) with the terminal state
// RunStateSkipped + ErrClassOverlapSkipped. The pairing is load-bearing for
// dashboard subscriber state machines (scheduler_finish.go ~:504-517): an
// ended-without-started frame would orphan the dashboard "running" badge.
//
// A future #870 interface-extraction PR that hoists this path behind an
// OverlapPolicy abstraction must preserve the paired-frame + skipped-state
// behaviour. Deterministic, no wall-clock: we pre-take the CAS gate so the
// executeOpt call observes the gate as already-running and short-circuits down
// the overlap path immediately.
func TestOverlapSkip_EmitsPairedStartedEnded(t *testing.T) {
	t.Parallel()

	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &fakeRouter{}, Telemetry: rec})

	j := &Job{ID: "job-overlap", Schedule: "@every 5m", Prompt: "ping", Platform: "feishu", ChatID: "X"}
	s.putJobForTest(j)

	// Pre-take the inflight gate so executeOpt's CompareAndSwap(false, true)
	// loses — exactly the state a concurrent in-flight run would leave.
	inflight := s.gateForTest().jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("precondition: initial CAS on a fresh inflight gate must succeed")
	}

	s.executeOpt(j.ID, true /* viaTriggerNow */)

	// The gate must STILL be held (overlap path must not release the
	// concurrent run's gate — R246-GO-3 / #689: emitOverlapSkipped passes a
	// nil finalizer precisely so it does not clear the in-flight run's CAS).
	if !inflight.running.Load() {
		t.Fatal("overlap-skip path released the in-flight run's CAS gate; it must leave it held (finalizer=nil)")
	}

	// Exactly one started + one ended frame, paired.
	rec.mu.Lock()
	startedN := len(rec.started)
	rec.mu.Unlock()
	if startedN != 1 {
		t.Fatalf("cron_run_started frames = %d, want 1 (overlap skip must emit a started frame)", startedN)
	}
	if got := rec.endedCount(); got != 1 {
		t.Fatalf("cron_run_ended frames = %d, want 1 (started/ended must stay paired on overlap-skip)", got)
	}

	ended := rec.endedAtCron(0)
	if ended.OwnerID != j.ID {
		t.Errorf("ended.OwnerID = %q, want %q", ended.OwnerID, j.ID)
	}
	if ended.State != RunStateSkipped {
		t.Errorf("ended.State = %v, want RunStateSkipped", ended.State)
	}
	if ended.ErrorClass != ErrClassOverlapSkipped {
		t.Errorf("ended.ErrorClass = %v, want ErrClassOverlapSkipped", ended.ErrorClass)
	}
	if ended.Trigger != TriggerManual {
		t.Errorf("ended.Trigger = %v, want TriggerManual (viaTriggerNow=true)", ended.Trigger)
	}

	// Match the started/ended frame's RunID so the dashboard can correlate
	// the pair — they must share the synthesised RunID.
	rec.mu.Lock()
	startedRunID := rec.started[0].RunID
	rec.mu.Unlock()
	if startedRunID == "" || startedRunID != ended.RunID {
		t.Errorf("started.RunID %q must equal ended.RunID %q (paired frames share a RunID)", startedRunID, ended.RunID)
	}
}
