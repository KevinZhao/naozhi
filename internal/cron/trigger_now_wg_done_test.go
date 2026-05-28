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
// gate for CRON4 (Round 174) refined under R260528-BUG-17.
//
// Original CRON4 contract required all three release sites to defer Done()
// inside a spawned goroutine for "symmetry" so Stop()'s triggerWG.Wait()
// could not observe a misordered counter. R260528-BUG-17 reconsidered the
// entryGone branch: it does zero asynchronous work (one slog.Debug call),
// so spawning a goroutine just to log + Done burned ~8 KiB stack + a
// scheduler-runtime hand-off for nothing, and the inline Done runs on
// TriggerNow's own caller goroutine BEFORE TriggerNow returns — Stop() that
// sees the counter at zero already saw a fully-released TriggerNow.
//
// Updated contract: the two work-doing branches (WrappedJob != nil; entryID
// == 0) MUST defer Done() inside a spawned goroutine because executeOpt
// lives there and is intentionally async. The entryGone branch MAY release
// inline because it does no work after Done. Bare top-level
// s.triggerWG.Done() outside the entryGone branch is still rejected.
func TestTriggerNow_BothBranchesReleaseWGInGoroutine(t *testing.T) {
	t.Parallel()

	body := readTriggerNowBody(t)

	// Two `go func()` spawn sites required for the work-doing paths
	// (WrappedJob != nil + entryID == 0). The entryGone fallback no longer
	// spawns under R260528-BUG-17.
	spawnCount := strings.Count(body, "go func()")
	if spawnCount != 2 {
		t.Errorf("TriggerNow must spawn exactly 2 `go func()` calls (one per work-doing WG release path), got %d.\n"+
			"See godoc on this test for the CRON4 / R260528-BUG-17 background.\nBody:\n%s",
			spawnCount, body)
	}

	// Each work-doing branch must defer-Done inside its goroutine.
	deferDoneCount := strings.Count(body, "defer s.triggerWG.Done()")
	if deferDoneCount != 2 {
		t.Errorf("TriggerNow must release triggerWG via `defer s.triggerWG.Done()` in each spawned goroutine (2 expected), got %d.\nBody:\n%s",
			deferDoneCount, body)
	}

	// The entryGone branch is the only legal home for an inline
	// `s.triggerWG.Done()`. Confirm exactly one bare top-level call so a
	// future refactor that drops it (regressing back to a goroutine spawn
	// or removing Done entirely) trips the count and the reader is pointed
	// here. The match pattern keys on the surrounding indentation
	// produced by gofmt — three tabs of nesting (function → if entryID
	// != 0 → if entryGone) — so it cannot accidentally match a goroutine
	// body's defer Done at deeper indentation.
	bareDoneCount := strings.Count(body, "\n\t\t\ts.triggerWG.Done()")
	if bareDoneCount != 1 {
		t.Errorf("TriggerNow must contain exactly 1 inline `s.triggerWG.Done()` (entryGone branch under R260528-BUG-17), got %d.\nBody:\n%s",
			bareDoneCount, body)
	}
}

// readTriggerNowBody locates the TriggerNow method in scheduler_jobs.go and
// returns its body text (between the function header and the matching
// closing brace). Intentionally keeps the lexer simple — brace counting
// in Go source is adequate because TriggerNow is a small top-level method
// without string literals containing unpaired braces.
//
// TriggerNow moved from scheduler.go to scheduler_jobs.go in the 2026-05
// cron-package refactor. Test reads the new location while keeping the
// CRON4 regression contract intact.
func readTriggerNowBody(t *testing.T) string {
	t.Helper()

	// Locate scheduler_jobs.go relative to this test file so the test is
	// resilient to `go test` being invoked from any working directory.
	_, thisFile, _, _ := runtime.Caller(0)
	schedulerPath := filepath.Join(filepath.Dir(thisFile), "scheduler_jobs.go")
	data, err := os.ReadFile(schedulerPath)
	if err != nil {
		t.Fatalf("read scheduler_jobs.go: %v", err)
	}
	src := string(data)

	header := "func (s *Scheduler) TriggerNow(id string) error {"
	idx := strings.Index(src, header)
	if idx < 0 {
		t.Fatalf("could not find TriggerNow method signature in scheduler_jobs.go")
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
