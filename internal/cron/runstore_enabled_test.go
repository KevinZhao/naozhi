package cron

import (
	"testing"
	"time"
)

// TestRunStoreEnabled_FoldsNilAndDisabled pins R249-ARCH-29 (#993): the
// enabled() predicate must collapse the two historically-separate "off"
// signals — a nil *runStore receiver and the disabled flag — into one
// gate so external callers stop mixing `s.runStore != nil` with the
// method-internal `s.disabled` guard.
func TestRunStoreEnabled_FoldsNilAndDisabled(t *testing.T) {
	t.Parallel()

	var nilStore *runStore
	if nilStore.enabled() {
		t.Fatal("nil *runStore must report enabled()==false")
	}

	disabled := &runStore{disabled: true}
	if disabled.enabled() {
		t.Fatal("disabled runStore must report enabled()==false")
	}

	live := &runStore{disabled: false}
	if !live.enabled() {
		t.Fatal("non-nil, non-disabled runStore must report enabled()==true")
	}
}

// TestScheduler_DisabledRunStore_AccessorsReturnEmpty verifies the external
// gate now keys off enabled() rather than a bare nil check: a Scheduler with
// a disabled (no-persist) runStore must serve empty history without touching
// disk, exactly as a nil store would have. R249-ARCH-29 (#993).
func TestScheduler_DisabledRunStore_AccessorsReturnEmpty(t *testing.T) {
	t.Parallel()

	s := &Scheduler{runStore: &runStore{disabled: true}}

	if got := s.ListRuns("abc", 10, time.Time{}); got != nil {
		t.Errorf("ListRuns on disabled store = %v, want nil", got)
	}
	if got := s.RecentRuns("abc", 5); got != nil {
		t.Errorf("RecentRuns on disabled store = %v, want nil", got)
	}
	if run, err := s.GetRun("abc", "def"); run != nil || err == nil {
		t.Errorf("GetRun on disabled store = (%v, %v), want (nil, non-nil err)", run, err)
	}
}
