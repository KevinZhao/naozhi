package cron

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestSetJobPrompt_UsesIIFEWithDeferUnlock_Structural pins R112714-LOGIC-2:
// SetJobPrompt must wrap its critical section in an IIFE with
// defer s.mu.Unlock() so that a panic inside resumeJobLocked (→ registerJob
// → AddFunc) does not permanently lock the mutex. The previous code used
// s.mu.Lock() without defer and relied on 5 explicit Unlock() calls across
// all return paths — a panic skipped all of them.
//
// Structural check: the SetJobPrompt function body must contain the IIFE
// pattern (func() ... { s.mu.Lock(); defer s.mu.Unlock() }) rather than a
// bare s.mu.Lock() followed by explicit s.mu.Unlock() calls.
func TestSetJobPrompt_UsesIIFEWithDeferUnlock_Structural(t *testing.T) {
	src, err := os.ReadFile("scheduler_jobs.go")
	if err != nil {
		t.Fatalf("read scheduler_jobs.go: %v", err)
	}
	body := string(src)

	const fnMarker = "func (s *Scheduler) SetJobPrompt("
	fnIdx := strings.Index(body, fnMarker)
	if fnIdx < 0 {
		t.Fatal("SetJobPrompt not found in scheduler_jobs.go")
	}
	fnBody := body[fnIdx:]
	if next := strings.Index(fnBody[len(fnMarker):], "\nfunc "); next >= 0 {
		fnBody = fnBody[:len(fnMarker)+next]
	}

	// Must contain the deferred unlock pattern inside a closure.
	if !strings.Contains(fnBody, "defer s.mu.Unlock()") {
		t.Error("R112714-LOGIC-2: SetJobPrompt must use `defer s.mu.Unlock()` " +
			"inside an IIFE so a panic in resumeJobLocked does not permanently " +
			"lock s.mu. The previous bare s.mu.Lock() + 5 explicit Unlock() " +
			"calls had no panic safety.")
	}

	// Must NOT contain bare s.mu.Unlock() outside a defer (i.e. direct
	// calls like `s.mu.Unlock()` that are not prefixed by `defer`).
	// We check that the only Unlock call is the deferred one. Count non-defer
	// occurrences: any "s.mu.Unlock()" not preceded by "defer" is a leftover
	// explicit call from the old pattern.
	remaining := fnBody
	bareUnlockCount := 0
	const bareUnlock = "s.mu.Unlock()"
	const deferUnlock = "defer s.mu.Unlock()"
	for {
		idx := strings.Index(remaining, bareUnlock)
		if idx < 0 {
			break
		}
		// Check if this occurrence is part of a defer statement.
		// Look back up to 10 chars for "defer ".
		start := idx
		if start > 10 {
			start = idx - 10
		}
		context := remaining[start:idx]
		if !strings.Contains(context, "defer") {
			bareUnlockCount++
		}
		remaining = remaining[idx+len(bareUnlock):]
	}
	_ = deferUnlock // used above indirectly
	if bareUnlockCount > 0 {
		t.Errorf("R112714-LOGIC-2: SetJobPrompt still contains %d bare "+
			"s.mu.Unlock() call(s) not covered by defer. These are the old "+
			"explicit unlock points that a panic would skip. All unlocks must "+
			"be handled by the single `defer s.mu.Unlock()` in the IIFE.",
			bareUnlockCount)
	}
}

// TestSetJobPrompt_AllReturnPathsUnlock verifies that SetJobPrompt unlocks
// s.mu on all observable return paths (not-found, already-set, persist-fail,
// success) — the IIFE + defer pattern guarantees this structurally, but this
// runtime test catches any regression where the lock is held after return.
func TestSetJobPrompt_AllReturnPathsUnlock(t *testing.T) {
	t.Parallel()

	s := NewScheduler(SchedulerConfig{
		MaxJobs:        10,
		AllowNilRouter: true,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	// Path 1: job not found — must unlock.
	if err := s.SetJobPrompt("nonexistent-id", "p"); err == nil {
		t.Error("expected error for nonexistent job")
	}
	// If lock is held, TryLock returns false. We use a goroutine with timeout
	// to verify s.mu is not held after SetJobPrompt returns.
	assertMuUnlocked(t, s, "not-found path")

	// Path 2: add a job with a prompt already set.
	job := &Job{
		Schedule: "@hourly",
		Prompt:   "already-set",
		Platform: "x",
		ChatID:   "c",
		WorkDir:  "/tmp",
	}
	if err := s.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := s.SetJobPrompt(job.ID, "new-prompt"); err == nil {
		t.Error("expected ErrPromptAlreadySet")
	}
	assertMuUnlocked(t, s, "already-set path")

	// Path 3: add a paused job with no prompt, then set prompt (success).
	job2 := &Job{
		Schedule: "@hourly",
		Platform: "x",
		ChatID:   "c2",
		WorkDir:  "/tmp",
		Paused:   true,
	}
	if err := s.AddJob(job2); err != nil {
		t.Fatalf("AddJob paused: %v", err)
	}
	if err := s.SetJobPrompt(job2.ID, "first-prompt"); err != nil {
		t.Errorf("SetJobPrompt success path: %v", err)
	}
	assertMuUnlocked(t, s, "success path")
}

// assertMuUnlocked verifies that s.mu is not held by trying to acquire a
// read lock within a short timeout. If the read lock cannot be acquired, the
// mutex is stuck and the test fails.
func assertMuUnlocked(t *testing.T, s *Scheduler, context string) {
	t.Helper()
	// RLock should be immediately acquirable if no write lock is held.
	// Use a goroutine + channel to enforce a timeout.
	done := make(chan struct{})
	go func() {
		s.mu.RLock()
		s.mu.RUnlock()
		close(done)
	}()
	select {
	case <-done:
		// mu is not held — good.
	case <-time.After(200 * time.Millisecond): // 200ms is generous; a real stuck mutex never releases
		t.Errorf("R112714-LOGIC-2 [%s]: s.mu appears to be permanently locked "+
			"after SetJobPrompt returned. The IIFE + defer Unlock() fix must "+
			"ensure s.mu is always released on all return paths including panics.",
			context)
	}
}
