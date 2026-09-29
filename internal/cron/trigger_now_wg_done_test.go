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
// Prior to Round 174, the `entryID != 0 && entry.WrappedJob == nil` branch
// of TriggerNow called s.triggerWG.Done() synchronously on the caller's
// goroutine before returning. This was logically correct for the immediate
// caller, but asymmetric with the sibling branches (WrappedJob != nil,
// entryID == 0) which both defer Done() inside a spawned goroutine. The
// asymmetry made it possible in principle for Stop()'s triggerWG.Wait() to
// observe the counter at zero between TriggerNow's Add(1) and the spawned
// goroutine of the other branches.
//
// R247-CR-29 (#596): the three sibling spawn sites were collapsed into one.
// The entry-gone check is now resolved to a single bool UNDER s.tbl.mu.RLock
// (preserving the single-consistent-instant race guard against a concurrent
// DeleteJob) and the function spawns exactly ONE `go func()` with one
// `defer s.triggerWG.Done()`. The contract is unchanged in spirit — every
// WG release still happens via `defer s.triggerWG.Done()` inside a spawned
// goroutine, and a synchronous `s.triggerWG.Done()` is still forbidden — but
// the expected spawn/defer count is now 1 rather than 3.
//
// This test scans the function body textually so a future refactor that
// re-introduces a synchronous Done() (or splits the single goroutine in a way
// that drops a release path) fails here explicitly, directing the reader to
// this godoc.
func TestTriggerNow_BothBranchesReleaseWGInGoroutine(t *testing.T) {
	t.Parallel()

	body := readTriggerNowBody(t)

	// Structural contract: exactly one `go func()` spawn site inside
	// TriggerNow. The entry-gone vs run decision is taken before the spawn
	// and branched inside the single goroutine. Any regression that
	// re-introduces a synchronous Done() path (dropping the spawn) or splits
	// the goroutine back into per-branch copies will move this count off 1.
	spawnCount := strings.Count(body, "go func()")
	if spawnCount != 1 {
		t.Errorf("TriggerNow must spawn exactly 1 `go func()` call (single WG release path), got %d.\n"+
			"See godoc on this test for the CRON4 / R247-CR-29 background.\nBody:\n%s",
			spawnCount, body)
	}

	// Defense against "spawn the goroutine but call Done synchronously":
	// the single release must appear as `defer s.triggerWG.Done()`, never a
	// bare `s.triggerWG.Done()` at top-level.
	deferDoneCount := strings.Count(body, "defer s.triggerWG.Done()")
	if deferDoneCount != 1 {
		t.Errorf("TriggerNow must release triggerWG via `defer s.triggerWG.Done()` in the spawned goroutine (1 expected), got %d.\nBody:\n%s",
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
