package cron

import (
	"testing"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// TestCRON1_FreshResetSerializedByInflightCAS pins the in-package half of
// the #401 (CRON1) invariant documented at freshContextPreflightP0's
// s.router.Reset call: cron↔cron concurrency on a single jobID cannot
// produce two interleaved Reset/GetOrCreate pairs on the same cron:<id>
// session key, because executeOpt is serialized per job by the inflight
// CAS gate.
//
// Setup mirrors TestP0_OverlapSkippedEmitsTerminalEvent: we manually hold
// the inflight gate (as if a run were in flight) and then drive a second
// executeOpt for the SAME fresh-mode job. The second call must bail at the
// CAS gate (overlap-skip) BEFORE reaching freshContextPreflightP0, so the
// router observes zero Reset calls from the loser. If a future refactor
// moved the fresh Reset ahead of the CAS gate, this test fails — which is
// exactly the regression that would reopen the Reset/GetOrCreate race the
// invariant comment relies on being closed for the cron↔cron case.
func TestCRON1_FreshResetSerializedByInflightCAS(t *testing.T) {
	t.Parallel()
	fake := &fakeSessionRouter{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: fake})

	const id = "job-fresh-overlap"
	j := &Job{
		ID:           id,
		Schedule:     "@every 5m",
		FreshContext: true,
		WorkDir:      "/tmp",
		Prompt:       "x",
	}
	s.mu.Lock()
	s.jobs[id] = j
	s.mu.Unlock()

	// Hold the inflight gate as if run #1 is mid-flight (between its own
	// Reset and GetOrCreate, say). Do NOT release until the assertion.
	inf := s.jobInflight(id)
	if !inf.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	defer inf.running.Store(false)

	// Run #2 (a scheduled tick or TriggerNow) races the in-flight run.
	// It must be overlap-skipped at the CAS gate and never reach the fresh
	// preflight's Reset.
	s.executeOpt(j, true)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, k := range fake.resetCalls {
		if k == sessionkey.CronKey(id) {
			t.Fatalf("loser run reached s.router.Reset(%q); inflight CAS must serialize fresh Reset/GetOrCreate per job (CRON1/#401)", k)
		}
	}
	if len(fake.getCreateKeys) != 0 {
		t.Fatalf("loser run reached GetOrCreate %v; expected overlap-skip before router calls", fake.getCreateKeys)
	}
}
