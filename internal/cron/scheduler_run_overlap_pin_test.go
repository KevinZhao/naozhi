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
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	// Pre-take the inflight gate so executeOpt's CompareAndSwap(false, true)
	// loses — exactly the state a concurrent in-flight run would leave.
	inflight := s.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("precondition: initial CAS on a fresh inflight gate must succeed")
	}

	s.executeOpt(j, true /* viaTriggerNow */)

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
	if ended.JobID != j.ID {
		t.Errorf("ended.JobID = %q, want %q", ended.JobID, j.ID)
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

	// Note: CronRunStartedTotal / CronRunSkippedTotal are process-global
	// expvars; asserting an absolute delta here would race the other parallel
	// cron tests. The per-test recordingBroadcaster frame counts above are the
	// isolated, deterministic signal for this pin.

	// Match the started/ended frame's RunID so the dashboard can correlate
	// the pair — they must share the synthesised RunID.
	rec.mu.Lock()
	startedRunID := rec.started[0].RunID
	rec.mu.Unlock()
	if startedRunID == "" || startedRunID != ended.RunID {
		t.Errorf("started.RunID %q must equal ended.RunID %q (paired frames share a RunID)", startedRunID, ended.RunID)
	}
}

// TestPerJobIDGate_RejectsConcurrentSameJob freezes the #1706
// (R20260603140013-GO-2) per-jobID gate that the #870 OverlapPolicy contract
// rests on: executeOpt admits at most ONE run body per jobID at a time. This
// mirrors the existing job_gate_double_exec_test peak-tracking pin but states
// it explicitly as part of the frozen #870 contract so a future extraction PR
// keeps the single-execution guarantee.
//
// Deterministic core: pre-take the gate (first run in flight); a second
// executeOpt for the same jobID must NOT enter the run body — it skips via the
// overlap path, leaving the first run's gate untouched.
func TestPerJobIDGate_RejectsConcurrentSameJob(t *testing.T) {
	t.Parallel()

	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: &fakeRouter{}, Telemetry: rec})

	j := &Job{ID: "job-gate", Schedule: "@every 5m", Prompt: "ping", Platform: "feishu", ChatID: "X"}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	// jobGateLock must return the SAME mutex for the same jobID — the gate is
	// what serialises the load→CAS against cleanup (#1706 precondition).
	if a, b := s.jobGateLock(j.ID), s.jobGateLock(j.ID); a != b {
		t.Fatalf("jobGateLock returned distinct mutexes for the same jobID: %p vs %p", a, b)
	}

	// Simulate an in-flight run holding the gate.
	inflight := s.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("precondition CAS must succeed")
	}

	// A concurrent executeOpt for the same jobID must be rejected (overlap
	// skip), NOT run a second body. Observable signature: it emits exactly one
	// skipped pair and leaves the gate held for the in-flight run.
	s.executeOpt(j, false)

	if !inflight.running.Load() {
		t.Fatal("second executeOpt cleared the in-flight gate — concurrent same-jobID run was NOT rejected (#1706 regression)")
	}
	if got := rec.endedCount(); got != 1 {
		t.Fatalf("expected exactly one skipped ended-frame from the rejected run, got %d", got)
	}
	if ended := rec.endedAtCron(0); ended.State != RunStateSkipped || ended.ErrorClass != ErrClassOverlapSkipped {
		t.Errorf("rejected run frame = {state:%v class:%v}, want {Skipped, overlap_skipped}", ended.State, ended.ErrorClass)
	}

	// Sanity: releasing the gate lets a subsequent CAS win again (gate is not
	// permanently wedged by the overlap path).
	inflight.running.Store(false)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("gate must be re-acquirable after the in-flight run releases it")
	}
	inflight.running.Store(false)
}
