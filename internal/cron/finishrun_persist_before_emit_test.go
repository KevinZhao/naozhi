package cron

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestR242ARCH10_FinishRunUpdatesJobBeforeEmit pins #731 / R242-ARCH-10:
// when finishRun fires the cron_run_ended event, a concurrent /api/cron
// list handler reading j.LastResult / j.LastSessionID MUST observe the
// freshly-recorded values, not the previous run's stale snapshot.
//
// recordResultP0WithSanitised mutates the Job under s.mu, releases the
// lock, then synchronously runs the persist save() — emitRunEnded
// fires only after both. This test asserts the contract by checking the
// in-memory Job state from inside the OnRunEnded callback. A regression
// that re-orders emitRunEnded ahead of the in-memory mutation (or makes
// the persist save() async without blocking the broadcast) would flip
// the observed values to the prior run's data.
func TestR242ARCH10_FinishRunUpdatesJobBeforeEmit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := SchedulerConfig{
		MaxJobs:   5,
		Router:    &fakeRouter{},
		StorePath: dir + "/cron_jobs.json",
	}
	s := NewScheduler(cfg)

	j := &Job{
		ID:            "job-finish-order",
		Schedule:      "@every 5m",
		Prompt:        "ping",
		LastResult:    "OLD-RESULT",
		LastSessionID: "OLD-SESSION",
	}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	// Snapshot from inside OnRunEnded — the callback fires last in
	// finishRun, so by then in-memory state must already reflect the
	// new run.
	var observedResult atomic.Value // string
	var observedSession atomic.Value
	observedResult.Store("")
	observedSession.Store("")
	s.SetOnRunEnded(func(ev RunEndedEvent) {
		s.mu.RLock()
		jj := s.jobs[ev.JobID]
		if jj != nil {
			observedResult.Store(jj.LastResult)
			observedSession.Store(jj.LastSessionID)
		}
		s.mu.RUnlock()
	})

	// Drive finishRun directly — bypassing executeOpt — so the test isolates
	// the recordResultP0WithSanitised → emitRunEnded ordering contract from
	// the spawn / send pipeline.
	inflight := s.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	finalizer := &runFinalizer{inflight: inflight}

	s.finishRun(finishArgs{
		job:       j,
		runID:     "r-finish-order",
		startedAt: time.Now(),
		trigger:   TriggerScheduled,
		state:     RunStateSucceeded,
		sessionID: "NEW-SESSION",
		result:    "NEW-RESULT",
		finalizer: finalizer,
	})

	if got := observedResult.Load().(string); got != "NEW-RESULT" {
		t.Errorf("OnRunEnded observed LastResult=%q, want %q (broadcast must fire AFTER in-memory state update)",
			got, "NEW-RESULT")
	}
	if got := observedSession.Load().(string); got != "NEW-SESSION" {
		t.Errorf("OnRunEnded observed LastSessionID=%q, want %q",
			got, "NEW-SESSION")
	}
}

// TestR242ARCH10_FinishRunPersistsBeforeEmit complements the in-memory
// contract above with the disk side: by the time OnRunEnded fires, the
// persist save() has already executed (and either landed on disk or
// short-circuited via the seq gate). Without the synchronous save() call
// inside recordResultP0WithSanitised, a subscriber reacting to the event
// by re-reading cron_jobs.json could see the prior run's payload.
func TestR242ARCH10_FinishRunPersistsBeforeEmit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := SchedulerConfig{
		MaxJobs:   5,
		Router:    &fakeRouter{},
		StorePath: dir + "/cron_jobs.json",
	}
	s := NewScheduler(cfg)

	j := &Job{
		ID:         "job-persist-order",
		Schedule:   "@every 5m",
		Prompt:     "ping",
		LastResult: "OLD",
	}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()

	// The contract: lastSavedSeq has advanced (>= 1) by the time the
	// OnRunEnded callback runs. Pre-fix, emitRunEnded could fire while
	// save() still owned storeMu, and a fresh persist had not yet
	// updated lastSavedSeq.
	var seenSeq atomic.Uint64
	s.SetOnRunEnded(func(ev RunEndedEvent) {
		seenSeq.Store(s.lastSavedSeq.Load())
	})

	inflight := s.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	finalizer := &runFinalizer{inflight: inflight}

	s.finishRun(finishArgs{
		job:       j,
		runID:     "r-persist-order",
		startedAt: time.Now(),
		trigger:   TriggerScheduled,
		state:     RunStateSucceeded,
		result:    "NEW",
		finalizer: finalizer,
	})

	if got := seenSeq.Load(); got == 0 {
		t.Errorf("OnRunEnded fired before saveMarshaledSeq advanced lastSavedSeq (got 0); broadcast must follow persist")
	}
}
