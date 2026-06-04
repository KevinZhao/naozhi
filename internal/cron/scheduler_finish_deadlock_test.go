package cron

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestRecordTerminalResult_PanicReleasesLock pins R20260604-GO-001: when the
// in-lock persist path panics (here via an injected marshalJobs stub that
// panics), recordTerminalResult must NOT leave s.mu held. robfig's Recover
// wrapper only catches the panic above this frame, so a hand-written Unlock
// that the panic skips would deadlock every subsequent tick. The single
// `defer s.mu.Unlock()` guarantees release on the panic path.
//
// Test plan:
//   - Add a job (real marshaler), then install a marshalJobs stub that panics.
//   - Call recordTerminalResult and recover the propagated panic.
//   - Assert s.mu is immediately re-acquirable from another goroutine within a
//     short deadline; a held lock would block forever and fail the deadline.
func TestRecordTerminalResult_PanicReleasesLock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		s.marshalJobs.Store(&defaultMarshalJobs)
		s.Stop()
	})

	job := &Job{Schedule: "@hourly", Prompt: "p", Platform: "p", ChatID: "c", WorkDir: "/tmp"}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Stub that panics inside persistJobsLocked → marshalJobsLocked, mimicking
	// a future Job-field type bug or a buggy custom marshaler.
	panicStub := marshalJobsFn(func(any) ([]byte, error) {
		panic("injected marshal panic")
	})
	s.marshalJobs.Store(&panicStub)

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("expected recordTerminalResult to propagate the marshal panic")
			}
		}()
		s.recordTerminalResult(job, "result", "", "", ErrClassNone, RunStateSucceeded, time.Now())
	}()

	// Restore a sane marshaler so the lock-acquisition probe's own persist (if
	// any) does not re-panic; the probe below only needs s.mu, not persist.
	s.marshalJobs.Store(&defaultMarshalJobs)

	locked := make(chan struct{})
	go func() {
		s.mu.Lock()
		s.mu.Unlock()
		close(locked)
	}()

	select {
	case <-locked:
		// Lock was free — the defer released it across the panic.
	case <-time.After(2 * time.Second):
		t.Fatal("s.mu still held after recordTerminalResult panic — deadlock (R20260604-GO-001 regression)")
	}

	// Sanity: scheduler still functions after recovery.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := s.PauseJobByID(job.ID); err != nil {
			t.Errorf("PauseJobByID after recovery: %v", err)
		}
	}()
	wg.Wait()
}
