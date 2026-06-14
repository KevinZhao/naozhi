// known_sessions_map_hint_test.go: structural and behavioural pins for
// buildKnownSessionsSet map allocation. Originally R20260603-PERF-3 sized the
// map from len(s.jobs) under the RLock; R202606-PERF-003 reverses that
// placement — the map is now allocated BEFORE the RLock to shorten the lock
// window, at the cost of a fixed initial capacity hint. The lock-hold
// reduction is the higher-value tradeoff for write-contended schedulers.

package cron

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestBuildKnownSessionsSet_MapAllocBeforeLock_R202606PERF003 is a structural
// pin that verifies buildKnownSessionsSet allocates the output map BEFORE
// taking s.mu.RLock(), so the make() does not run inside the lock window and
// block writers. This supersedes the earlier R20260603-PERF-3 pin that
// required the make to read len(s.jobs) under the lock.
func TestBuildKnownSessionsSet_MapAllocBeforeLock_R202606PERF003(t *testing.T) {
	src, err := os.ReadFile("scheduler_session.go")
	if err != nil {
		t.Fatalf("read scheduler_session.go: %v", err)
	}
	body := string(src)

	const fnMarker = "func (s *Scheduler) buildKnownSessionsSet()"
	idx := strings.Index(body, fnMarker)
	if idx < 0 {
		t.Fatalf("buildKnownSessionsSet not found in scheduler_session.go")
	}
	rest := body[idx:]
	if next := strings.Index(rest[len(fnMarker):], "\nfunc "); next >= 0 {
		rest = rest[:len(fnMarker)+next]
	}

	// The map make must appear BEFORE s.mu.RLock() so the allocation is not
	// performed while holding the read lock. R202606-PERF-003.
	idxRLock := strings.Index(rest, "s.mu.RLock()")
	idxMake := strings.Index(rest, "make(map[string]struct{}")
	if idxRLock < 0 {
		t.Fatal("buildKnownSessionsSet: s.mu.RLock() not found")
	}
	if idxMake < 0 {
		t.Fatal("buildKnownSessionsSet: make(map[string]struct{}) not found")
	}
	if idxMake >= idxRLock {
		t.Error("buildKnownSessionsSet: map make must appear before s.mu.RLock() " +
			"so the allocation does not extend the lock window (R202606-PERF-003)")
	}

	// The map alloc must not depend on len(s.jobs): that read needs the lock,
	// which is exactly what we moved the alloc out of.
	makeStmt := rest[idxMake:]
	if end := strings.Index(makeStmt, ")"); end >= 0 {
		makeStmt = makeStmt[:end+1]
	}
	if strings.Contains(makeStmt, "len(s.jobs)") {
		t.Error("buildKnownSessionsSet: map make must not size from len(s.jobs) " +
			"now that it runs before the RLock (R202606-PERF-003)")
	}
}

// TestBuildKnownSessionsSet_MapHint_ScalesWithJobs is a behavioural test
// verifying that buildKnownSessionsSet returns all LastSessionIDs even for
// a scheduler seeded with more than 32 jobs (the former fixed hint),
// exercising the rehash path that the hint was meant to eliminate.
// Jobs are injected directly into s.jobs to bypass scheduler AddJob limits.
func TestBuildKnownSessionsSet_MapHint_ScalesWithJobs(t *testing.T) {
	t.Parallel()

	const nJobs = 40 // > the former fixed hint of 32
	s := schedulerForJobsR241GO2Test(t)

	wantSessions := make(map[string]bool, nJobs)
	s.mu.Lock()
	for i := 0; i < nJobs; i++ {
		id := fmt.Sprintf("job%04d", i)
		sid := "sid-" + id
		s.jobs[id] = &Job{ID: id, LastSessionID: sid}
		wantSessions[sid] = true
	}
	s.mu.Unlock()

	got := s.buildKnownSessionsSet()
	for sid := range wantSessions {
		if _, ok := got[sid]; !ok {
			t.Errorf("session %q missing from buildKnownSessionsSet result", sid)
		}
	}
	if len(got) < nJobs {
		t.Errorf("buildKnownSessionsSet returned %d entries, want >= %d", len(got), nJobs)
	}
}
