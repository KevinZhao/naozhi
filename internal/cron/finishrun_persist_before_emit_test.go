package cron

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// observingBroadcaster invokes the supplied callback inside
// BroadcastRunEnded so the contract test below can snapshot Job state
// at the precise moment the cron run-ended event reaches the
// broadcaster — equivalent to the pre-Phase-D SetOnRunEnded hook.
type observingBroadcaster struct{ onEnded func() }

func (b observingBroadcaster) BroadcastRunStarted(ev runtelemetry.RunStartedEvent) {}
func (b observingBroadcaster) BroadcastRunEnded(ev runtelemetry.RunEndedEvent) {
	if b.onEnded != nil {
		b.onEnded()
	}
}

// TestR242ARCH10_FinishRunUpdatesJobBeforeEmit pins #731 / R242-ARCH-10:
// when finishRun fires the cron_run_ended event, a concurrent /api/cron
// list handler reading j.LastResult / j.LastSessionID MUST observe the
// freshly-recorded values, not the previous run's stale snapshot.
//
// recordResultP0WithSanitised mutates the Job under s.mu, releases the
// lock, then synchronously runs the persist save() — emitRunEnded
// fires only after both. This test asserts the contract by checking the
// in-memory Job state from inside the broadcaster callback. A regression
// that re-orders emitRunEnded ahead of the in-memory mutation (or makes
// the persist save() async without blocking the broadcast) would flip
// the observed values to the prior run's data.
func TestR242ARCH10_FinishRunUpdatesJobBeforeEmit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var (
		observedMu      sync.Mutex
		observedResult  string
		observedSession string
	)
	var sched *Scheduler
	cfg := SchedulerConfig{
		MaxJobs:   5,
		Router:    &fakeRouter{},
		StorePath: dir + "/cron_jobs.json",
		Telemetry: observingBroadcaster{onEnded: func() {
			sched.mu.RLock()
			defer sched.mu.RUnlock()
			if jj, ok := sched.jobs["job-finish-order"]; ok {
				observedMu.Lock()
				observedResult = jj.LastResult
				observedSession = jj.LastSessionID
				observedMu.Unlock()
			}
		}},
	}
	sched = NewScheduler(cfg)

	j := &Job{
		ID:            "job-finish-order",
		Schedule:      "@every 5m",
		Prompt:        "ping",
		LastResult:    "OLD-RESULT",
		LastSessionID: "OLD-SESSION",
	}
	sched.mu.Lock()
	sched.jobs[j.ID] = j
	sched.mu.Unlock()

	// Drive finishRun directly — bypassing executeOpt — so the test isolates
	// the recordResultP0WithSanitised → emitRunEnded ordering contract from
	// the spawn / send pipeline.
	inflight := sched.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	finalizer := &runFinalizer{inflight: inflight}

	sched.finishRun(finishArgs{
		job:       j,
		runID:     "r-finish-order",
		startedAt: time.Now(),
		trigger:   TriggerScheduled,
		state:     RunStateSucceeded,
		sessionID: "NEW-SESSION",
		result:    "NEW-RESULT",
		finalizer: finalizer,
	})

	observedMu.Lock()
	defer observedMu.Unlock()
	if observedResult != "NEW-RESULT" {
		t.Errorf("RunEnded observed LastResult=%q, want %q (broadcast must fire AFTER in-memory state update)",
			observedResult, "NEW-RESULT")
	}
	if observedSession != "NEW-SESSION" {
		t.Errorf("RunEnded observed LastSessionID=%q, want %q",
			observedSession, "NEW-SESSION")
	}
}

// TestR242ARCH10_FinishRunPersistsBeforeEmit complements the in-memory
// contract above with the disk side: by the time RunEnded broadcasts,
// the persist save() has already executed (and either landed on disk
// or short-circuited via the seq gate). Without the synchronous save()
// call inside recordResultP0WithSanitised, a subscriber reacting to
// the event by re-reading cron_jobs.json could see the prior run's
// payload.
func TestR242ARCH10_FinishRunPersistsBeforeEmit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var seenSeq atomic.Uint64
	var sched *Scheduler
	cfg := SchedulerConfig{
		MaxJobs:   5,
		Router:    &fakeRouter{},
		StorePath: dir + "/cron_jobs.json",
		Telemetry: observingBroadcaster{onEnded: func() {
			seenSeq.Store(sched.lastSavedSeq.Load())
		}},
	}
	sched = NewScheduler(cfg)

	j := &Job{
		ID:         "job-persist-order",
		Schedule:   "@every 5m",
		Prompt:     "ping",
		LastResult: "OLD",
	}
	sched.mu.Lock()
	sched.jobs[j.ID] = j
	sched.mu.Unlock()

	inflight := sched.jobInflight(j.ID)
	if !inflight.running.CompareAndSwap(false, true) {
		t.Fatal("initial CAS must succeed")
	}
	finalizer := &runFinalizer{inflight: inflight}

	sched.finishRun(finishArgs{
		job:       j,
		runID:     "r-persist-order",
		startedAt: time.Now(),
		trigger:   TriggerScheduled,
		state:     RunStateSucceeded,
		result:    "NEW",
		finalizer: finalizer,
	})

	if got := seenSeq.Load(); got == 0 {
		t.Errorf("RunEnded fired before saveMarshaledSeq advanced lastSavedSeq (got 0); broadcast must follow persist")
	}
}
