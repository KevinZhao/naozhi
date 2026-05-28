package cron

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestMarshalJobs_PerSchedulerIsolation pins R239-GO-7 (#867) /
// R250-ARCH-14 / R242-CR-5 (#693) / R247-CR-19 (#599): the marshalJobs
// seam MUST be per-Scheduler, not a package-level global. The historical
// layout used a `var marshalJobs atomic.Pointer` that withFailingMarshal
// swapped wholesale; two parallel tests installing different stubs would
// clobber each other and the observed behaviour depended on goroutine
// scheduling. The current layout holds the atomic.Pointer on Scheduler so
// each test's stub stays scoped to its own instance.
//
// Test plan:
//
//   - Two Schedulers, A and B, each with a different marshalJobs stub
//     (A returns "injected-A", B returns errors).
//   - Trigger persistJobsLocked on both concurrently, repeatedly.
//   - A's mutations must always see "injected-A" output and never observe
//     B's failure (which would prove cross-Scheduler leak).
//
// This is the contract test #867 asks for to prevent a future refactor
// from reverting to a package-level atomic.
func TestMarshalJobs_PerSchedulerIsolation(t *testing.T) {
	t.Parallel()

	dirA := t.TempDir()
	dirB := t.TempDir()
	sA := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dirA, "cron.json"),
		MaxJobs:   5,
	})
	sB := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dirB, "cron.json"),
		MaxJobs:   5,
	})
	if err := sA.Start(); err != nil {
		t.Fatalf("sA.Start: %v", err)
	}
	if err := sB.Start(); err != nil {
		t.Fatalf("sB.Start: %v", err)
	}
	// Defer Stop AFTER we restore the real marshaler so shutdown persist
	// does not log noisy errors from the failing-B stub. Cleanup runs in
	// reverse-defer order: restore-then-Stop-A-then-Stop-B.
	t.Cleanup(func() {
		sA.marshalJobs.Store(&defaultMarshalJobs)
		sB.marshalJobs.Store(&defaultMarshalJobs)
		sA.Stop()
		sB.Stop()
	})

	// Add a job to each scheduler BEFORE installing stubs so AddJob's
	// initial persist uses the real marshaler.
	jobA := &Job{Schedule: "@hourly", Prompt: "a", Platform: "p", ChatID: "c", WorkDir: "/tmp"}
	if err := sA.AddJob(jobA); err != nil {
		t.Fatalf("AddJob A: %v", err)
	}
	jobB := &Job{Schedule: "@hourly", Prompt: "b", Platform: "p", ChatID: "c", WorkDir: "/tmp"}
	if err := sB.AddJob(jobB); err != nil {
		t.Fatalf("AddJob B: %v", err)
	}

	// Distinct stubs: A returns a sentinel byte slice, B returns an error.
	// If they shared a slot, A's persist would either fail (B leaked into
	// A) or B's mutation would somehow succeed (A leaked into B).
	stubA := marshalJobsFn(func(any) ([]byte, error) {
		return []byte("[]"), nil // success path; pinned bytes for assertion
	})
	stubB := marshalJobsFn(func(any) ([]byte, error) {
		return nil, fmt.Errorf("B should never poison A")
	})
	sA.marshalJobs.Store(&stubA)
	sB.marshalJobs.Store(&stubB)

	// Concurrently exercise both schedulers' persist paths.
	const iters = 50
	var wg sync.WaitGroup
	errsA := make(chan error, iters)
	errsB := make(chan error, iters)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			// PauseJobByID then ResumeJobByID — each triggers a persist.
			if _, err := sA.PauseJobByID(jobA.ID); err != nil {
				errsA <- fmt.Errorf("iter %d pause: %w", i, err)
				return
			}
			if _, err := sA.ResumeJobByID(jobA.ID); err != nil {
				errsA <- fmt.Errorf("iter %d resume: %w", i, err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			// B's stub always errors; expect ErrPersistFailed every iteration.
			_, err := sB.PauseJobByID(jobB.ID)
			if err == nil {
				errsB <- fmt.Errorf("iter %d: B pause unexpectedly succeeded — A's stub may have leaked", i)
				return
			}
			if !errors.Is(err, ErrPersistFailed) {
				errsB <- fmt.Errorf("iter %d: B pause returned non-persist err: %v", i, err)
				return
			}
			// Resume to undo if Pause somehow took effect, so the next iter
			// has a Paused→Active transition to attempt.
			_, _ = sB.ResumeJobByID(jobB.ID)
		}
	}()
	wg.Wait()
	close(errsA)
	close(errsB)
	for err := range errsA {
		t.Errorf("scheduler A: %v", err)
	}
	for err := range errsB {
		t.Errorf("scheduler B: %v", err)
	}
}
