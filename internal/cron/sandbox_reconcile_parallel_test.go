package cron

// Tests for R20260616-PERF-006 (#2142): reconcileSandboxPending fans the
// per-orphan StopSession out across a bounded worker pool. The correctness
// invariant is unchanged — every valid orphan is Stopped and its pending file
// removed — regardless of how many orphans share the dir.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// concurrencyProbeRunner records the max number of StopSession calls in flight
// at once so the test can assert the reconcile actually parallelises.
type concurrencyProbeRunner struct {
	mu       sync.Mutex
	inFlight int
	maxSeen  int
	total    atomic.Int64
	delay    time.Duration
}

func (r *concurrencyProbeRunner) RunJob(context.Context, SandboxJob, func([]byte) error) (SandboxOutcome, error) {
	return SandboxOutcome{State: SandboxStateSuccess}, nil
}

func (r *concurrencyProbeRunner) StopSession(_ context.Context, _ string) error {
	r.mu.Lock()
	r.inFlight++
	if r.inFlight > r.maxSeen {
		r.maxSeen = r.inFlight
	}
	r.mu.Unlock()
	r.total.Add(1)
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	r.mu.Lock()
	r.inFlight--
	r.mu.Unlock()
	return nil
}

// R202606e-GO-002: when stopCtx is already cancelled, the multi-orphan feed
// loop must bail via its select on stopCtx instead of handing off every orphan
// on the unbuffered jobs channel. Workers skip Stops while shutting down, so a
// regression here would still iterate the whole orphan slice and hold up
// close(jobs)/wg.Wait() against the gcWaitBudget. We assert reconcile returns
// promptly and never Stops anything once shutdown has begun.
func TestReconcileSandboxPending_BailsSendSideOnStopCtx(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "cron_jobs.json")
	runner := &concurrencyProbeRunner{delay: 50 * time.Millisecond}
	s, _ := sandboxTestScheduler(t, runner, storePath)

	const nOrphans = 50
	for i := 0; i < nOrphans; i++ {
		runID := fmt.Sprintf("feedface0001%04d", i)
		writePendingFixture(t, storePath, sandboxPending{
			JobID:            fmt.Sprintf("0123456789cd%04d", i),
			RunID:            runID,
			RuntimeSessionID: "run-" + runID + "-1234567890123456789",
			StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
		})
	}

	// Cancel before the pass so both the worker-side gate and the send-side
	// select see a done stopCtx.
	s.Stop()

	done := make(chan struct{})
	go func() {
		s.reconcileSandboxPending()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcileSandboxPending did not return promptly after stopCtx cancel; send side likely blocked feeding all orphans")
	}

	if got := runner.total.Load(); got != 0 {
		t.Fatalf("StopSession called %d time(s) after stopCtx cancel; want 0", got)
	}
}

func TestReconcileSandboxPending_ParallelStopsAllOrphans(t *testing.T) {
	tests := []struct {
		name       string
		nOrphans   int
		wantMaxGt1 bool // expect observed concurrency > 1 (parallel path)
	}{
		{name: "single orphan stays serial", nOrphans: 1, wantMaxGt1: false},
		{name: "two orphans parallelise", nOrphans: 2, wantMaxGt1: true},
		{name: "many orphans parallelise within worker bound", nOrphans: 10, wantMaxGt1: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			storePath := filepath.Join(dir, "cron_jobs.json")
			runner := &concurrencyProbeRunner{delay: 20 * time.Millisecond}
			s, _ := sandboxTestScheduler(t, runner, storePath)

			paths := make([]string, 0, tc.nOrphans)
			for i := 0; i < tc.nOrphans; i++ {
				runID := fmt.Sprintf("feedface0000%04d", i)
				p := sandboxPending{
					JobID:            fmt.Sprintf("0123456789ab%04d", i),
					RunID:            runID,
					RuntimeSessionID: "run-" + runID + "-1234567890123456789",
					StartedAtMS:      time.Now().Add(-2 * time.Minute).UnixMilli(),
				}
				paths = append(paths, writePendingFixture(t, storePath, p))
			}

			s.reconcileSandboxPending()

			if got := runner.total.Load(); got != int64(tc.nOrphans) {
				t.Fatalf("StopSession total = %d, want %d (every orphan must be Stopped)", got, tc.nOrphans)
			}
			for _, p := range paths {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Fatalf("orphan pending file %s must be removed after reconcile", p)
				}
			}

			runner.mu.Lock()
			maxSeen := runner.maxSeen
			runner.mu.Unlock()
			if tc.wantMaxGt1 && maxSeen <= 1 {
				t.Fatalf("observed max in-flight Stops = %d, want > 1 (reconcile must parallelise)", maxSeen)
			}
			if maxSeen > sandboxReconcileWorkers {
				t.Fatalf("observed max in-flight Stops = %d, exceeds worker bound %d", maxSeen, sandboxReconcileWorkers)
			}
		})
	}
}
