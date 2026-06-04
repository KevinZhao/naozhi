package cron

import "testing"

// TestEnsureStub_NilReceiverReturnsFalse pins R20260603-ARCH-1: a nil
// *Scheduler stored in a CronView interface is non-nil at the interface level,
// so a dashboard handler's "h.scheduler != nil" guard does not prevent a call
// on the typed-nil receiver. EnsureStub must not panic and must return false,
// mirroring the nil-safe contract of NotifyDefault / StartedAt.
func TestEnsureStub_NilReceiverReturnsFalse(t *testing.T) {
	t.Parallel()
	var s *Scheduler
	// Must not panic and must return false.
	if got := s.EnsureStub("cron:somejobid"); got {
		t.Error("nil *Scheduler EnsureStub returned true, want false")
	}
	// Also exercise with an invalid key to confirm the nil guard fires before
	// any key parsing.
	if got := s.EnsureStub(""); got {
		t.Error("nil *Scheduler EnsureStub(\"\") returned true, want false")
	}
}
