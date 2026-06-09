package cron

import "testing"

// TestSessionStatusOrdinalParity pins the cron-local SessionStatus enum to its
// documented shape (#776 / RFC cron-config-and-structs Phase 1, doc-only). The
// cron-local SessionStatus mirrors session.SessionStatus value-for-value; the
// ordinals are panic-pinned against session.* at boot by
// internal/wireup/cron_router_adapter.go init() (R260528-GO-18). This test is the
// cheap in-package counterpart: it freezes the count (3) and the iota order so
// that anyone adding/reordering a value here is forced to look at the pin and
// the session-side definition.
//
// IF YOU ADD A VALUE: also add the mirror in internal/session, update the
// ordinal pin in internal/wireup/cron_router_adapter.go init(), and bump
// wantSessionStatusCount below.
func TestSessionStatusOrdinalParity(t *testing.T) {
	t.Parallel()

	const wantSessionStatusCount = 3
	got := []SessionStatus{SessionExisting, SessionResumed, SessionNew}
	if len(got) != wantSessionStatusCount {
		t.Fatalf("SessionStatus count = %d, want %d", len(got), wantSessionStatusCount)
	}
	for i, v := range got {
		if int(v) != i {
			t.Errorf("SessionStatus ordinal[%d] = %d, want %d (iota order broke; resync the adapter pin)", i, int(v), i)
		}
	}
}

// TestInterruptOutcomeOrdinalParity pins the cron-local InterruptOutcome enum.
// The adapter casts cron.InterruptOutcome(session.InterruptOutcome) numerically,
// so a divergent ordinal silently shuffles values; the count+order freeze here
// plus the adapter init() panic together guard the contract.
//
// IF YOU ADD A VALUE: mirror it in internal/session, update the ordinal pin in
// internal/wireup/cron_router_adapter.go init(), and bump wantInterruptOutcomeCount.
func TestInterruptOutcomeOrdinalParity(t *testing.T) {
	t.Parallel()

	const wantInterruptOutcomeCount = 5
	got := []InterruptOutcome{
		InterruptSent,
		InterruptNoSession,
		InterruptNoTurn,
		InterruptUnsupported,
		InterruptError,
	}
	if len(got) != wantInterruptOutcomeCount {
		t.Fatalf("InterruptOutcome count = %d, want %d", len(got), wantInterruptOutcomeCount)
	}
	for i, v := range got {
		if int(v) != i {
			t.Errorf("InterruptOutcome ordinal[%d] = %d, want %d (iota order broke; resync the adapter pin)", i, int(v), i)
		}
	}
}
