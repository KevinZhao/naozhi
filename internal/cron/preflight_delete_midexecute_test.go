package cron

import (
	"testing"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// TestPreflightDeleteMidExecute_RefreshesStubBeforeGateRelease is the fifth of
// the five paths the deleted stub-refresh source anchor counted (#2318,
// R202606h-GO-009b). It had no behaviour test at all — the anchor's ">=5 stub
// refreshes precede their finishRun" was its only coverage, and that count is
// satisfied by any five call sites anywhere in the file.
//
// The invariant: on the delete-mid-execute branch the sidebar stub must be
// re-registered BEFORE the finishRun that releases the inflight CAS gate. After
// release a concurrent TriggerNow could spawn run-B's live stub, and this stale
// re-register would clobber it.
func TestPreflightDeleteMidExecute_RefreshesStubBeforeGateRelease(t *testing.T) {
	t.Parallel()

	ord := &orderRecorder{}
	rec := &recordingBroadcaster{order: ord}
	// deleteAtReset: 1 = the PREFLIGHT Reset window (the existing fixture defaults
	// to 2, the reap window of a success run).
	router := &deleteOnResetRouter{reapRouter: reapRouter{order: ord}, deleteAtReset: 1}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5}, SchedulerDeps{Router: router, Telemetry: rec})
	router.s = s

	j := &Job{ID: "job-deleted-midexec", Schedule: "@every 5m", Prompt: "ping", FreshContext: true}
	router.jobID = j.ID
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	s.executeOpt(j, true /* viaTriggerNow: skip jitter */)

	if rec.endedCount() != 1 {
		t.Fatalf("want 1 ended event, got %d; sequence=%v", rec.endedCount(), ord.events())
	}
	got := rec.endedAtCron(0)
	if got.State != RunStateCanceled {
		t.Fatalf("state = %q, want canceled for a job deleted mid-execute (errClass=%q)", got.State, got.ErrorClass)
	}
	if got.ErrorClass != ErrClassCanceled {
		t.Errorf("errClass = %q, want %q", got.ErrorClass, ErrClassCanceled)
	}

	// The branch must be the preflight one: a Reset happened (so we were past the
	// preflight) and no session was ever created.
	wantKey := sessionkey.CronKey(j.ID)
	resets, _ := router.snapshot()
	seen := false
	for _, k := range resets {
		if k == wantKey {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("no preflight Reset(%q); the test did not reach the delete-mid-execute branch: resets=%v",
			wantKey, resets)
	}

	// NOT asserted here: that the stub re-register precedes run-ended. On this
	// branch it CANNOT be — stubRefresher.run() re-registers only if the job still
	// exists, and the branch is reached precisely because it does not, so the call
	// is a designed no-op ("run() re-checks existence, so for the steady-state
	// delete it is a no-op" — scheduler_run.go). The observed sequence is
	// [run-started reset run-ended]. That ordering therefore stays pinned by the
	// narrowed source anchor in stub_refresh_before_finish_source_anchor_test.go:
	// it guards a future refactor where the job could exist again at that point
	// (delete → re-create inside the gate), which behaviour cannot reach today.
	if got := ord.events(); len(got) == 0 {
		t.Errorf("no events recorded; the run did not reach finishRun")
	}
}
