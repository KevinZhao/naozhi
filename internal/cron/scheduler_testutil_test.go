// scheduler_testutil_test.go: test-only seams that share package state
// with production but must not occupy production binary surface area.
//
// Anything that lives here is unreachable from non-_test.go callers.
// Promoting one of these helpers back into scheduler.go requires a real
// production caller — without that, dead-code sweeps (#1216) will flag
// it as DEADCODE again.

package cron

import "time"

// WithStopBudget shortens the per-Scheduler stopBudget for the duration
// of a test and returns a restore func intended for t.Cleanup.
// Centralising the swap here keeps the direct-write pattern off the
// call sites and gives future maintainers a single seam.
//
// R247-CR-18 (original); relocated under R248-DEADCODE-24 / #1216 so
// the helper no longer ships in the production binary. R260528-BUG-5:
// migrated from a package-level `var stopBudget` to a *Scheduler field
// — parallel tests with multiple Scheduler instances no longer race on
// shared global state. Callers must pass the *Scheduler whose budget
// they want to shorten; same-package _test.go retains access to the
// unexported field.
func WithStopBudget(s *Scheduler, d time.Duration) func() {
	orig := s.stopBudget
	s.stopBudget = d
	return func() { s.stopBudget = orig }
}
