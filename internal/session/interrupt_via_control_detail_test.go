package session

import (
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestInterruptViaControlDetail_Outcomes pins the (outcome, error) contract
// of InterruptViaControlDetail so cron / dispatch callers can errors.Is
// against transport sentinels without InterruptViaControl swallowing the
// underlying error. R249-GO-18 (#916).
//
// Each case wires a fakeProcess that returns the listed err from its
// InterruptViaControl, then asserts the ManagedSession-level outcome and
// the surfaced error. The pre-fix ManagedSession.InterruptViaControl
// dropped err on the floor for every non-Sent path, leaving operators
// unable to distinguish "shim socket dead" from "stdin write returned
// EAGAIN" — both bucketed as InterruptError.
func TestInterruptViaControlDetail_Outcomes(t *testing.T) {
	t.Parallel()

	transportErr := errors.New("shim socket closed")

	cases := []struct {
		name        string
		procErr     error
		alive       bool
		wantOutcome InterruptOutcome
		wantErrIs   error // errors.Is target; nil → no err
	}{
		{
			name:        "no_session_when_proc_dead",
			procErr:     nil,
			alive:       false,
			wantOutcome: InterruptNoSession,
			wantErrIs:   nil,
		},
		{
			name:        "sent_when_no_err",
			procErr:     nil,
			alive:       true,
			wantOutcome: InterruptSent,
			wantErrIs:   nil,
		},
		{
			name:        "no_turn_propagates_sentinel",
			procErr:     cli.ErrNoActiveTurn,
			alive:       true,
			wantOutcome: InterruptNoTurn,
			wantErrIs:   cli.ErrNoActiveTurn,
		},
		{
			name:        "unsupported_propagates_sentinel",
			procErr:     cli.ErrInterruptUnsupported,
			alive:       true,
			wantOutcome: InterruptUnsupported,
			wantErrIs:   cli.ErrInterruptUnsupported,
		},
		{
			name:        "transport_err_surfaced_for_errors_is",
			procErr:     transportErr,
			alive:       true,
			wantOutcome: InterruptError,
			wantErrIs:   transportErr,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			proc := &fakeProcess{
				isAlive:       tc.alive,
				viaControlErr: tc.procErr,
			}
			s := &ManagedSession{key: "test:c2c:u1:general"}
			s.storeProcess(proc)

			gotOutcome, gotErr := s.InterruptViaControlDetail()
			if gotOutcome != tc.wantOutcome {
				t.Fatalf("outcome = %v, want %v", gotOutcome, tc.wantOutcome)
			}
			if tc.wantErrIs == nil {
				if gotErr != nil {
					t.Fatalf("err = %v, want nil", gotErr)
				}
				return
			}
			if !errors.Is(gotErr, tc.wantErrIs) {
				t.Fatalf("errors.Is(%v, %v) = false; want true", gotErr, tc.wantErrIs)
			}
		})
	}
}

// TestInterruptViaControl_DelegatesToDetail pins that the existing
// outcome-only API still returns the same outcome bucket as
// InterruptViaControlDetail — adding the new method must not regress
// existing callers (cron scheduler_run.go, dispatch.go owner loop).
func TestInterruptViaControl_DelegatesToDetail(t *testing.T) {
	t.Parallel()
	proc := &fakeProcess{
		isAlive:       true,
		viaControlErr: errors.New("transport boom"),
	}
	s := &ManagedSession{key: "test:c2c:u1:general"}
	s.storeProcess(proc)

	if got := s.InterruptViaControl(); got != InterruptError {
		t.Fatalf("InterruptViaControl() = %v, want InterruptError", got)
	}
}
