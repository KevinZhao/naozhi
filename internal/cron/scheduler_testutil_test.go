// scheduler_testutil_test.go: test-only seams that share package state
// with production but must not occupy production binary surface area.
//
// Anything that lives here is unreachable from non-_test.go callers.
// Promoting one of these helpers back into scheduler.go requires a real
// production caller — without that, dead-code sweeps (#1216) will flag
// it as DEADCODE again.

package cron

import "time"

// WithStopBudget shortens the package-level stopBudget for the duration
// of a test and returns a restore func intended for t.Cleanup.
// Centralising the swap here keeps the racy direct-write pattern off
// the call sites and gives future maintainers a single seam to migrate
// to a Scheduler-field design (the long-term direction noted on
// gcWaitBudget) without touching every test.
//
// R247-CR-18 (original); relocated under R248-DEADCODE-24 / #1216 so
// the helper no longer ships in the production binary. Same-package
// _test.go can still reach the unexported stopBudget so call sites
// (stop_budget_test.go) need no change.
func WithStopBudget(d time.Duration) func() {
	orig := stopBudget
	stopBudget = d
	return func() { stopBudget = orig }
}

// WithStopBudgetField overrides the per-instance Scheduler.stopBudget
// directly and returns a restore func for t.Cleanup. Prefer this over
// WithStopBudget when the *Scheduler already exists — it keeps the
// budget swap local to one instance, so t.Parallel tests on separate
// Schedulers cannot race each other on the package-level var.
//
// R249-CR-3 (#947): NewScheduler now snapshots the package-level
// stopBudget into a per-instance field, completing the long-term
// direction flagged on gcWaitBudget. This seam exposes that field swap
// to tests so they no longer need to reach for the package-level
// back-channel post-construction.
func WithStopBudgetField(s *Scheduler, d time.Duration) func() {
	orig := s.stopBudget
	s.stopBudget = d
	return func() { s.stopBudget = orig }
}
