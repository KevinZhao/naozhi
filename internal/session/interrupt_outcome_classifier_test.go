package session

import (
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestManagedSession_InterruptViaControl_OutcomeClassifier pins R249-GO-18
// (#916): the per-error-class branches in ManagedSession.InterruptViaControl
// MUST stay separate so cron / dispatch / dashboard callers can errors.Is
// against InterruptUnsupported (a static "ACP doesn't support control_request"
// signal) versus InterruptError (a transient transport failure that should
// surface at Warn).
//
// A future refactor that collapses the cli.ErrInterruptUnsupported branch
// into the default "transport / write error" arm would silently re-classify
// every kiro session into "log a Warn", which is the regression described
// in #916.
func TestManagedSession_InterruptViaControl_OutcomeClassifier(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		setProc func() *fakeProcess
		want    InterruptOutcome
	}{
		{
			name: "no_session_when_proc_nil",
			setProc: func() *fakeProcess {
				return nil // attach no process
			},
			want: InterruptNoSession,
		},
		{
			name: "no_session_when_dead",
			setProc: func() *fakeProcess {
				return newDeadProc()
			},
			want: InterruptNoSession,
		},
		{
			name: "sent_when_proc_returns_nil",
			setProc: func() *fakeProcess {
				p := newRunningProc()
				p.viaControlErr = nil
				return p
			},
			want: InterruptSent,
		},
		{
			name: "no_turn_for_ErrNoActiveTurn",
			setProc: func() *fakeProcess {
				p := newRunningProc()
				p.viaControlErr = cli.ErrNoActiveTurn
				return p
			},
			want: InterruptNoTurn,
		},
		{
			name: "unsupported_for_ErrInterruptUnsupported",
			setProc: func() *fakeProcess {
				p := newRunningProc()
				p.viaControlErr = cli.ErrInterruptUnsupported
				return p
			},
			want: InterruptUnsupported,
		},
		{
			name: "error_for_unrecognised_transport_failure",
			setProc: func() *fakeProcess {
				p := newRunningProc()
				p.viaControlErr = errors.New("shim socket dead")
				return p
			},
			want: InterruptError,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &ManagedSession{key: "im:direct:u1:general"}
			if proc := tc.setProc(); proc != nil {
				s.storeProcess(proc)
			}
			got := s.InterruptViaControl()
			if got != tc.want {
				t.Fatalf("InterruptViaControl() = %v (%s), want %v (%s)",
					int(got), got.String(), int(tc.want), tc.want.String())
			}
		})
	}
}
