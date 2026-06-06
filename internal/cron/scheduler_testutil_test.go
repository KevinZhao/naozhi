// scheduler_testutil_test.go: test-only seams that share package state
// with production but must not occupy production binary surface area.
//
// Anything that lives here is unreachable from non-_test.go callers.
// Promoting one of these helpers back into scheduler.go requires a real
// production caller — without that, dead-code sweeps (#1216) will flag
// it as DEADCODE again.

package cron

import "time"

// WithStopBudgetField overrides the per-instance Scheduler.stopBudget
// directly and returns a restore func for t.Cleanup. Call it after the
// *Scheduler is constructed — it keeps the budget swap local to one
// instance, so t.Parallel tests on separate Schedulers cannot race each
// other.
//
// R249-CR-3 (#947): NewScheduler seeds the per-instance stopBudget
// field, completing the long-term direction flagged on gcWaitBudget.
// R20260603150052-GO-2 (#1712): the package-level back-channel var
// (and its WithStopBudget seam) are removed; this per-instance field
// swap is now the ONLY way to inject a short budget for a test.
func WithStopBudgetField(s *Scheduler, d time.Duration) func() {
	orig := s.stopBudget
	s.stopBudget = d
	return func() { s.stopBudget = orig }
}
