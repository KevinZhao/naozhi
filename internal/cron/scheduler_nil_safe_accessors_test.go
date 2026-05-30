package cron

import (
	"testing"
	"time"
)

// TestNilSchedulerDashboardAccessors pins R249-CR-11 (#955): the three
// dashboard-facing read accessors the bootstrap window relies on
// (Location / NotifyDefault / StartedAt) must all be nil-safe so a
// pre-wireup dashboard render does not panic on any one of them. Before
// the fix StartedAt() dereferenced s.startedAtNanos on a nil receiver even
// though NotifyDefault()'s godoc advertised it as nil-safe.
func TestNilSchedulerDashboardAccessors(t *testing.T) {
	t.Parallel()
	var s *Scheduler

	if got := s.StartedAt(); !got.IsZero() {
		t.Errorf("nil Scheduler StartedAt() = %v, want zero time", got)
	}
	if got := s.Location(); got != time.UTC {
		t.Errorf("nil Scheduler Location() = %v, want UTC", got)
	}
	if got := s.NotifyDefault(); got.IsSet() {
		t.Errorf("nil Scheduler NotifyDefault() = %+v, want zero (unset) target", got)
	}
}
