// known_sessions_map_hint_test.go: structural and behavioural pins for
// R20260603-PERF-3 — buildKnownSessionsSet map initial capacity.

package cron

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestBuildKnownSessionsSet_MapHint_R20260603PERF3 is a structural pin that
// verifies buildKnownSessionsSet allocates the output map with len(s.jobs)
// as the capacity hint instead of the fixed 32 that caused repeated rehashes
// for schedulers with many jobs.
func TestBuildKnownSessionsSet_MapHint_R20260603PERF3(t *testing.T) {
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

	// Must NOT use the fixed 32 hint.
	if strings.Contains(rest, "make(map[string]struct{}, 32)") {
		t.Error("buildKnownSessionsSet must not use make(..., 32); " +
			"use len(s.jobs) as the capacity hint (R20260603-PERF-3)")
	}

	// Must use len(s.jobs) as the hint.
	if !strings.Contains(rest, "len(s.jobs)") {
		t.Error("buildKnownSessionsSet must use len(s.jobs) as the map " +
			"capacity hint to avoid rehashes on large job sets (R20260603-PERF-3)")
	}

	// The make must appear after s.mu.RLock() so len(s.jobs) is read under
	// the read lock.
	idxRLock := strings.Index(rest, "s.mu.RLock()")
	idxMake := strings.Index(rest, "make(map[string]struct{}")
	if idxRLock < 0 {
		t.Fatal("buildKnownSessionsSet: s.mu.RLock() not found")
	}
	if idxMake < 0 {
		t.Fatal("buildKnownSessionsSet: make(map[string]struct{}) not found")
	}
	if idxMake <= idxRLock {
		t.Error("buildKnownSessionsSet: map make must appear after s.mu.RLock() " +
			"so len(s.jobs) is read under the read lock (R20260603-PERF-3)")
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
