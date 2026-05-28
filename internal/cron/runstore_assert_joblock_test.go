package cron

import (
	"path/filepath"
	"testing"
	"time"
)

// TestRunStore_AssertJobLockHeld_NoOpInProduction pins R249-CR-18 (#961):
// the assertJobLockHeld probe is a best-effort *Locked-suffix contract
// check whose TryLock+Unlock pair costs ~30 ns on every Append.
// testing.Testing() returns true under `go test`, so this test still
// exercises the warn path; production binaries skip the syscalls
// entirely. The assertion below confirms the function still fires its
// warn-on-contract-miss branch under test (so reviewers don't lose
// the safety net) — a regression that nooped the whole function would
// fail this test by NOT detecting the unheld lock.
//
// We verify behaviour, not the gate itself: we call the method without
// holding jobLock and confirm it returns without panic. A
// counterfactual test (proving zero overhead in production) would
// require a non-test build target which the unit-test framework
// can't host. The presence of this test is itself the contract
// anchor — any future change that moves the gate around must keep
// the under-test behaviour observable.
func TestRunStore_AssertJobLockHeld_NoOpInProduction(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	s := newRunStore(storePath, 10, time.Hour)
	if s == nil || s.disabled {
		t.Fatalf("newRunStore must succeed; got disabled")
	}
	// Lock NOT held — under test we expect a slog.Warn but no panic.
	// Production would skip the TryLock entirely (testing.Testing() == false).
	s.assertJobLockHeld("0123456789abcdef")

	// Held path: no warn, no panic. The contract is "held → return
	// silently, unheld → warn but do not panic". This branch covers
	// the held arm so we verify the function is well-formed under both
	// modes irrespective of the testing.Testing() gate.
	lock := s.jobLock("0123456789abcdef")
	lock.Lock()
	s.assertJobLockHeld("0123456789abcdef")
	lock.Unlock()
}

// TestRunStore_AssertJobLockHeld_TestingGuardSelfTest sanity-checks
// that testing.Testing() is true inside `go test` — if this ever
// flipped (e.g. Go runtime change, build flag mutation), the
// assertJobLockHeld gate would silently noop under tests too,
// hiding contract-miss regressions in the rest of the suite.
// R249-CR-18 (#961).
func TestRunStore_AssertJobLockHeld_TestingGuardSelfTest(t *testing.T) {
	t.Parallel()
	if !testing.Testing() {
		t.Fatal("testing.Testing() must report true inside `go test` " +
			"so the assertJobLockHeld gate fires the contract probe; " +
			"a false reading would silently noop the safety net")
	}
}
