package cron

import (
	"errors"
	"io/fs"
	"testing"
	"time"
)

// TestScheduler_RunHistoryNilSafe: the exported history reads stay safe on a
// nil Scheduler and on one built without a store, which is what a
// pre-wireup dashboard render and the zero-value test fixtures hand them.
func TestScheduler_RunHistoryNilSafe(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]*Scheduler{"nil scheduler": nil, "no store": {}} {
		if got := s.ListRuns("abc", 10, time.Time{}); got != nil {
			t.Errorf("%s: ListRuns = %v, want nil", name, got)
		}
		if got := s.RecentRuns("abc", 5); got != nil {
			t.Errorf("%s: RecentRuns = %v, want nil", name, got)
		}
		if run, err := s.Run("abc", "def"); run != nil || !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: Run = (%v, %v), want (nil, fs.ErrNotExist)", name, run, err)
		}
	}
}
