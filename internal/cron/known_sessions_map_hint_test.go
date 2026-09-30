// known_sessions_map_hint_test.go: buildKnownSessionsSet's output map. It is
// allocated before the registry lock window (R202606-PERF-003) — by the
// caller, since jobTable.sessionIDs only fills a map it is handed — at the
// cost of a fixed initial capacity hint.

package cron

import (
	"fmt"
	"testing"
)

// TestBuildKnownSessionsSet_MapHint_ScalesWithJobs is a behavioural test
// verifying that buildKnownSessionsSet returns all LastSessionIDs even for
// a scheduler seeded with more than 32 jobs (the former fixed hint),
// exercising the rehash path that the hint was meant to eliminate.
// Jobs are injected directly into s.tbl.jobs to bypass scheduler AddJob limits.
func TestBuildKnownSessionsSet_MapHint_ScalesWithJobs(t *testing.T) {
	t.Parallel()

	const nJobs = 40 // > the former fixed hint of 32
	s := schedulerForJobsR241GO2Test(t)

	wantSessions := make(map[string]bool, nJobs)
	for i := 0; i < nJobs; i++ {
		id := fmt.Sprintf("job%04d", i)
		sid := "sid-" + id
		s.putJobForTest(&Job{ID: id, LastSessionID: sid})
		wantSessions[sid] = true
	}

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
