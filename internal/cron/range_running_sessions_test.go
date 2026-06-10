package cron

import (
	"sort"
	"testing"
)

// newRunningInflight builds a runInflight that snapshot() reports as running
// with the given SessionID. An empty sessionID models a started-but-not-yet-
// minted run.
func newRunningInflight(sessionID string) *runInflight {
	inf := &runInflight{}
	inf.running.Store(true)
	inf.view.Store(&runInflightView{SessionID: sessionID})
	return inf
}

// TestRangeRunningSessionIDs verifies the helper introduced by R249-CR-4 /
// R260528-ARCH-7 (#948 / #1368): it visits only running runs with a non-empty
// SessionID, skips not-running guards, and honours an early-stop fn.
func TestRangeRunningSessionIDs(t *testing.T) {
	s := NewScheduler(SchedulerConfig{MaxJobs: 10, AllowNilRouter: true}, SchedulerDeps{})

	// running + session id -> visited
	s.runningJobs.Store("a", newRunningInflight("sess-a"))
	s.runningJobs.Store("b", newRunningInflight("sess-b"))
	// running but no session id yet -> skipped
	s.runningJobs.Store("c", newRunningInflight(""))
	// not running (bare guard) -> skipped
	s.runningJobs.Store("d", &runInflight{})

	var got []string
	s.rangeRunningSessionIDs(func(sid string) bool {
		got = append(got, sid)
		return true
	})
	sort.Strings(got)
	want := []string{"sess-a", "sess-b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("rangeRunningSessionIDs visited %v, want %v", got, want)
	}

	// Early stop: returning false after the first hit ends iteration.
	count := 0
	s.rangeRunningSessionIDs(func(string) bool {
		count++
		return false
	})
	if count != 1 {
		t.Fatalf("early-stop fn invoked %d times, want 1", count)
	}
}
