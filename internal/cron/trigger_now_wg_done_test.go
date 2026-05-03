package cron

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestTriggerNow_BothBranchesReleaseWGInGoroutine is a source-level regression
// gate for CRON4 (Round 174).
//
// Prior to this round, the `entryID != 0 && entry.WrappedJob == nil` branch
// of TriggerNow called s.triggerWG.Done() synchronously on the caller's
// goroutine before returning. This was logically correct for the immediate
// caller, but asymmetric with the sibling branches (WrappedJob != nil,
// entryID == 0) which both defer Done() inside a spawned goroutine. The
// asymmetry made it possible in principle for Stop()'s triggerWG.Wait() to
// observe the counter at zero between TriggerNow's Add(1) and the spawned
// goroutine of the other branches — a concern that grows every time someone
// adds a new code path around the WG reservation.
//
// Contract: every release of the reserved WG slot MUST happen via `defer
// s.triggerWG.Done()` inside a `go func() { ... }()` spawned in the function
// body. Synchronous `s.triggerWG.Done()` calls are forbidden.
//
// This test scans the function body textually so a future refactor that
// removes one of the goroutines re-introduces the asymmetry and fails
// here explicitly, directing the reader to this godoc.
func TestTriggerNow_BothBranchesReleaseWGInGoroutine(t *testing.T) {
	t.Parallel()

	body := readTriggerNowBody(t)

	// Structural contract: exactly three `go func()` spawn sites inside
	// TriggerNow. Two in the `entryID != 0` block (WrappedJob != nil, and
	// the entry-gone fallback), one in the `entryID == 0` block. Any
	// regression that collapses one of the entryID != 0 branches into a
	// synchronous Done() will drop this count to two and fail.
	spawnCount := strings.Count(body, "go func()")
	if spawnCount != 3 {
		t.Errorf("TriggerNow must spawn exactly 3 `go func()` calls (one per WG release path), got %d.\n"+
			"See godoc on this test for the CRON4 background.\nBody:\n%s",
			spawnCount, body)
	}

	// Defense against "spawn the goroutine but call Done synchronously":
	// every release must appear as `defer s.triggerWG.Done()`, never a
	// bare `s.triggerWG.Done()` at top-level.
	deferDoneCount := strings.Count(body, "defer s.triggerWG.Done()")
	if deferDoneCount != 3 {
		t.Errorf("TriggerNow must release triggerWG via `defer s.triggerWG.Done()` in each spawned goroutine (3 expected), got %d.\nBody:\n%s",
			deferDoneCount, body)
	}

	// Explicitly reject the pre-fix shape: a bare `s.triggerWG.Done()`
	// call outside a `defer` at top level. The "defer" prefix precludes
	// matching `defer s.triggerWG.Done()`.
	if strings.Contains(body, "\n\t\ts.triggerWG.Done()") {
		t.Errorf("TriggerNow contains a synchronous `s.triggerWG.Done()` call — CRON4 regression. " +
			"Release the WG slot from a spawned goroutine instead.")
	}
}

// readTriggerNowBody locates the TriggerNow method in scheduler.go and
// returns its body text (between the function header and the matching
// closing brace). Intentionally keeps the lexer simple — brace counting
// in Go source is adequate because TriggerNow is a small top-level method
// without string literals containing unpaired braces.
func readTriggerNowBody(t *testing.T) string {
	t.Helper()

	// Locate scheduler.go relative to this test file so the test is
	// resilient to `go test` being invoked from any working directory.
	_, thisFile, _, _ := runtime.Caller(0)
	schedulerPath := filepath.Join(filepath.Dir(thisFile), "scheduler.go")
	data, err := os.ReadFile(schedulerPath)
	if err != nil {
		t.Fatalf("read scheduler.go: %v", err)
	}
	src := string(data)

	header := "func (s *Scheduler) TriggerNow(id string) error {"
	idx := strings.Index(src, header)
	if idx < 0 {
		t.Fatalf("could not find TriggerNow method signature in scheduler.go")
	}
	// Find the matching closing brace via depth counting.
	start := idx + len(header)
	depth := 1
	end := -1
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		t.Fatalf("could not find closing brace for TriggerNow")
	}
	return src[start:end]
}

// TestTriggerNow_EntryGoneReleasesWG is a behavioural test for the CRON4
// branch (entryID != 0 but entry.WrappedJob == nil). It simulates a
// "concurrent delete" by manually seeding a non-zero entryID into jobs map
// for an entry the cron engine does not know about, then calls TriggerNow
// and verifies that triggerWG is ultimately zero — the reservation made
// inside TriggerNow must be released by the fallback goroutine.
//
// Round 174 (CRON4).
func TestTriggerNow_EntryGoneReleasesWG(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := NewScheduler(SchedulerConfig{
		StorePath: filepath.Join(dir, "cron.json"),
		MaxJobs:   5,
	})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	// Construct a Job that has a non-zero entryID pointing at nothing.
	// We cannot use AddJob here because that would register the job with
	// s.cron and make entry.WrappedJob non-nil, which routes us to the
	// "normal run" arm rather than the "entry gone" arm under test.
	// Instead inject directly into the jobs map with a synthetic entryID
	// that s.cron has never seen — s.cron.Entry(entryID) returns a zero
	// Entry with nil WrappedJob, exactly the race we want to exercise.
	s.mu.Lock()
	s.jobs["orphan-job"] = &Job{
		ID:       "orphan-job",
		Schedule: "@every 1h",
		Prompt:   "stub",
		entryID:  99999, // entry that cron engine does not know about
	}
	s.mu.Unlock()

	if err := s.TriggerNow("orphan-job"); err != nil {
		t.Fatalf("TriggerNow: %v", err)
	}

	// Wait for the spawned goroutine to call Done. The work is a single
	// slog.Debug call so this is essentially immediate, but we must not
	// race against scheduling. triggerWG.Wait blocks until count is zero.
	waitDone := make(chan struct{})
	go func() {
		s.triggerWG.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		// Counter reached zero → the goroutine fired Done exactly once,
		// matching the one-to-one Add(1)/Done() pairing required by the
		// WG semantics. If CRON4 regressed and Done was called
		// synchronously AND a stray goroutine also tried to Done, the
		// second call would panic before we got here.
	case <-timeoutAfter(t):
		t.Fatal("triggerWG never reached zero after TriggerNow on orphan entry; " +
			"the fallback goroutine likely failed to call Done()")
	}
}

// timeoutAfter returns a channel that fires if Wait does not complete in a
// reasonable amount of time. Keeps the timeout explicit rather than using
// testify or an ad-hoc time.After at the call site.
func timeoutAfter(t *testing.T) <-chan struct{} {
	t.Helper()
	ch := make(chan struct{})
	go func() {
		// 3s is generous: the fallback goroutine body is a single slog.
		// Debug call. Anything slower indicates a regression.
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		<-timer.C
		close(ch)
	}()
	return ch
}
