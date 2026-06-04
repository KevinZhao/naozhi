package cron

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// concurrencySession records the peak number of Send calls in flight at once.
// executeOpt is supposed to admit at most one run per jobID at a time (the
// inflight CAS gate); a value >1 is the observable signature of the
// double-execution race R20260603140013-GO-2 (#1706) closes.
type concurrencySession struct {
	cur  *atomic.Int32
	peak *atomic.Int32
	hold time.Duration
}

func (s concurrencySession) Send(ctx context.Context, text string) (SendResult, error) {
	n := s.cur.Add(1)
	for { // track peak under concurrency
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if s.hold > 0 {
		time.Sleep(s.hold)
	}
	s.cur.Add(-1)
	return SendResult{Text: "ok", SessionID: "sess"}, nil
}
func (s concurrencySession) SessionID() string                     { return "sess" }
func (s concurrencySession) InterruptViaControl() InterruptOutcome { return InterruptUnsupported }

type concurrencyRouter struct{ sess concurrencySession }

func (r concurrencyRouter) RegisterCronStubWithChain(key, workspace, lastPrompt string, chain []string) {
}
func (r concurrencyRouter) Reset(key string) {}
func (r concurrencyRouter) GetOrCreate(ctx context.Context, key string, opts AgentOpts) (Session, SessionStatus, error) {
	return r.sess, SessionExisting, nil
}

// TestJobGateLock_SameJobIDSharesMutex pins the precondition the fix rests on:
// executeOpt and cleanupRunningJobIfIdle must take the SAME mutex for a given
// jobID (otherwise serialising them is meaningless). Sharding by hash means
// the same id always maps to the same shard.
func TestJobGateLock_SameJobIDSharesMutex(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: &fakeRouter{}})
	a := s.jobGateLock("job-x")
	b := s.jobGateLock("job-x")
	if a != b {
		t.Fatalf("jobGateLock returned distinct mutexes for the same jobID: %p vs %p", a, b)
	}
	// Index must be in range and stable.
	if idx := jobGateShardIndex("job-x"); idx >= jobGateShards {
		t.Fatalf("shard index %d out of range [0,%d)", idx, jobGateShards)
	}
	if jobGateShardIndex("job-x") != jobGateShardIndex("job-x") {
		t.Fatal("jobGateShardIndex not deterministic")
	}
}

// TestJobGate_CleanupSerialisedAgainstGate is the deterministic core of the
// #1706 fix: while a goroutine holds the per-jobID gate (mimicking executeOpt
// mid jobInflight-load→CAS), cleanupRunningJobIfIdle MUST block — it cannot
// observe-and-delete the runningJobs entry in that window. Before the fix
// cleanup took no gate, so it would race straight through and delete the entry
// an in-flight executeOpt was about to CAS-win on, orphaning the gate and
// permitting double execution. A regression that drops either gate acquisition
// makes cleanup complete immediately and fails this test.
func TestJobGate_CleanupSerialisedAgainstGate(t *testing.T) {
	t.Parallel()
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: &fakeRouter{}})

	const jobID = "job-serialise"
	// Seed an idle inflight entry so cleanup has something to delete once it
	// acquires the gate (running=false → deletable).
	_ = s.jobInflight(jobID)

	gate := s.jobGateLock(jobID)
	gate.Lock()

	done := make(chan bool, 1)
	go func() { done <- s.cleanupRunningJobIfIdle(jobID) }()

	// Cleanup must be blocked on the gate — it should NOT complete while held.
	select {
	case <-done:
		gate.Unlock()
		t.Fatal("cleanupRunningJobIfIdle completed while the per-jobID gate was held; " +
			"it is not taking the gate (the #1706 serialisation is broken)")
	case <-time.After(100 * time.Millisecond):
		// Expected: blocked.
	}

	// Release the gate; cleanup must now proceed and delete the idle entry.
	gate.Unlock()
	select {
	case deleted := <-done:
		if !deleted {
			t.Fatal("cleanup returned false for an idle seeded entry; want delete=true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup never completed after gate release")
	}
	if _, present := s.runningJobs.Load(jobID); present {
		t.Fatal("runningJobs entry should be gone after cleanup of an idle job")
	}
}

// TestJobGate_NoDoubleExecutionUnderDeleteTriggerRace is the concurrency
// companion: many executeOpt runs for ONE jobID interleaved with cleanup must
// never run two bodies at once, and must stay data-race clean under -race.
// Probabilistic (the bug window is one instruction wide), so its primary value
// is the -race data-race check on the shared runningJobs entry; the
// deterministic serialisation guarantee lives in the test above.
func TestJobGate_NoDoubleExecutionUnderDeleteTriggerRace(t *testing.T) {
	t.Parallel()

	var cur, peak atomic.Int32
	router := concurrencyRouter{sess: concurrencySession{cur: &cur, peak: &peak, hold: 100 * time.Microsecond}}
	rec := &recordingBroadcaster{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: router, Telemetry: rec})

	const jobID = "job-race"
	j := &Job{ID: jobID, Schedule: "@every 5m", Prompt: "ping", Platform: "feishu", ChatID: "X"}
	s.mu.Lock()
	s.jobs[jobID] = j
	s.mu.Unlock()

	const rounds = 500
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); s.executeOpt(j, true) }()
		go func() { defer wg.Done(); s.cleanupRunningJobIfIdle(jobID) }()
	}
	wg.Wait()

	if got := peak.Load(); got > 1 {
		t.Fatalf("peak concurrent run bodies for one jobID = %d, want ≤1 (double execution)", got)
	}
}
