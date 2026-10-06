package cron

import (
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cron/runstore"
)

// TestScheduler_DisabledRunStore_AccessorsReturnEmpty verifies the external
// gate now keys off Enabled() rather than a bare nil check: a Scheduler with
// a disabled (no-persist) run store must serve empty history without touching
// disk, exactly as a nil store would have. R249-ARCH-29 (#993).
func TestScheduler_DisabledRunStore_AccessorsReturnEmpty(t *testing.T) {
	t.Parallel()

	s := &Scheduler{runs: runstore.New(runstore.Options{})}

	if got := s.ListRuns("abc", 10, time.Time{}); got != nil {
		t.Errorf("ListRuns on disabled store = %v, want nil", got)
	}
	if got := s.RecentRuns("abc", 5); got != nil {
		t.Errorf("RecentRuns on disabled store = %v, want nil", got)
	}
	if run, err := s.Run("abc", "def"); run != nil || err == nil {
		t.Errorf("GetRun on disabled store = (%v, %v), want (nil, non-nil err)", run, err)
	}
}

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
		if h := s.RunStoreHealth(); h.Enabled {
			t.Errorf("%s: RunStoreHealth = %+v, want disabled", name, h)
		}
	}
}
