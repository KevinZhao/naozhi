package session

import (
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestManagedSession_InterruptViaControl_OutcomeClassifier pins R249-GO-18
// (#916): the 5 outcome branches of ManagedSession.InterruptViaControl
// (NoSession / Sent / NoTurn / Unsupported / Error) MUST stay separate so
// cron / dispatch / dashboard can errors.Is against InterruptUnsupported
// (static "ACP doesn't support control_request") versus InterruptError
// (transient transport failure that surfaces at Warn). A refactor that
// collapses ErrInterruptUnsupported into the default arm would silently
// re-classify every kiro/ACP session.
func TestManagedSession_InterruptViaControl_OutcomeClassifier(t *testing.T) {
	t.Parallel()
	mk := func(err error, alive, running bool) *fakeProcess {
		return &fakeProcess{isAlive: alive, isRunning: running, viaControlErr: err}
	}
	cases := []struct {
		name string
		proc *fakeProcess
		want InterruptOutcome
	}{
		{"no_session_proc_nil", nil, InterruptNoSession},
		{"no_session_dead", mk(nil, false, false), InterruptNoSession},
		{"sent_nil_err", mk(nil, true, true), InterruptSent},
		{"no_turn_ErrNoActiveTurn", mk(cli.ErrNoActiveTurn, true, true), InterruptNoTurn},
		{"unsupported_ErrInterruptUnsupported", mk(cli.ErrInterruptUnsupported, true, true), InterruptUnsupported},
		{"error_unrecognised_transport", mk(errors.New("shim socket dead"), true, true), InterruptError},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &ManagedSession{key: "im:direct:u1:general"}
			if tc.proc != nil {
				s.storeProcess(tc.proc)
			}
			if got := s.InterruptViaControl(); got != tc.want {
				t.Fatalf("got %s, want %s", got.String(), tc.want.String())
			}
		})
	}
}
