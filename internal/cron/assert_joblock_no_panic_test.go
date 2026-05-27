package cron

import (
	"testing"
	"time"
)

// TestAssertJobLockHeld_NoPanicOnUnheld verifies R242-CR-11 (#696) +
// R242-CR-7 (#694): assertJobLockHeld must NOT panic in production code
// paths. The historical implementation panic'd; that propagated through
// Append's deferred unlock and crashed the process when a contract bug
// (e.g. a future caller of skipAppendTrim that forgot to acquire jobLock)
// landed. The fixed implementation logs a slog.Warn with the jobID and
// returns so the cron history path stays best-effort per RFC §4.2.
func TestAssertJobLockHeld_NoPanicOnUnheld(t *testing.T) {
	s := newTestStore(t, 10, 24*time.Hour)
	jobID := mustGenerateID()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("assertJobLockHeld panicked when lock not held: %v", r)
		}
	}()
	// jobLock(jobID) is free — the historical impl panic'd here.
	s.assertJobLockHeld(jobID)
}

// TestAssertJobLockHeld_NoPanicWhenHeld also exercises the success path
// (lock currently held by some goroutine — TryLock returns false, no
// warn). Belt-and-suspenders: ensures the rewrite did not flip the
// branch direction.
func TestAssertJobLockHeld_NoPanicWhenHeld(t *testing.T) {
	s := newTestStore(t, 10, 24*time.Hour)
	jobID := mustGenerateID()
	lock := s.jobLock(jobID)
	lock.Lock()
	defer lock.Unlock()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("assertJobLockHeld panicked while lock was held: %v", r)
		}
	}()
	s.assertJobLockHeld(jobID)
}
