package wireup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
)

// TestTurnFailure: a result the backend flagged as an error becomes
// cron.ErrTurnFailed with run-history detail and the cause the notice words;
// a healthy turn and an abort naozhi requested stay nil, whether claude
// reports it as an aborted_* terminal_reason or error_during_execution.
func TestTurnFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		r      clievent.SendResult
		failed bool
		detail []string
		cause  cron.TurnCause
	}{
		{name: "success", r: clievent.SendResult{Text: "done", SubType: "success"}},
		{name: "error subtype without is_error", r: clievent.SendResult{SubType: "error_max_turns"}},
		{name: "own abort", r: clievent.SendResult{SubType: "error_during_execution", IsError: true, Aborted: true}},
		{name: "own abort 2.1.288", r: clievent.SendResult{SubType: "success", Aborted: true, TerminalReason: "aborted_tools"}},
		{name: "unrequested aborted_tools", r: clievent.SendResult{SubType: "success", TerminalReason: "aborted_tools"}},
		{name: "own aborted_* flagged is_error", r: clievent.SendResult{SubType: "success", IsError: true, Aborted: true, TerminalReason: "aborted_streaming", Text: "partial"}},
		{
			name:   "unrequested aborted_* flagged is_error",
			r:      clievent.SendResult{SubType: "success", IsError: true, TerminalReason: "aborted_streaming", Text: "partial"},
			failed: true, detail: []string{"success", "partial"}, cause: cron.TurnCauseUnknown,
		},
		{
			name:   "max turns, empty text",
			r:      clievent.SendResult{SubType: "error_max_turns", IsError: true},
			failed: true, detail: []string{"error_max_turns"}, cause: cron.TurnCauseMaxTurns,
		},
		{
			name:   "budget, empty text",
			r:      clievent.SendResult{SubType: "error_max_budget_usd", IsError: true},
			failed: true, detail: []string{"error_max_budget_usd"}, cause: cron.TurnCauseBudget,
		},
		{
			name:   "unrequested error_during_execution",
			r:      clievent.SendResult{SubType: "error_during_execution", IsError: true},
			failed: true, detail: []string{"error_during_execution"}, cause: cron.TurnCauseUnknown,
		},
		{
			name:   "abort flag does not mute a different failure",
			r:      clievent.SendResult{SubType: "error_max_turns", IsError: true, Aborted: true},
			failed: true, detail: []string{"error_max_turns"}, cause: cron.TurnCauseMaxTurns,
		},
		{
			name:   "claude error text",
			r:      clievent.SendResult{SubType: "success", IsError: true, Text: "Prompt is too long"},
			failed: true, detail: []string{"Prompt is too long"}, cause: cron.TurnCauseContextTooLong,
		},
		{
			name:   "claude api envelope",
			r:      clievent.SendResult{SubType: "success", IsError: true, Text: "API Error: 529 overloaded"},
			failed: true, detail: []string{"529 overloaded"}, cause: cron.TurnCauseBackendOverloaded,
		},
		{
			name:   "claude api envelope, connection",
			r:      clievent.SendResult{SubType: "success", IsError: true, Text: "API Error: connection reset"},
			failed: true, detail: []string{"connection reset"}, cause: cron.TurnCauseBackendUnreachable,
		},
		{
			name:   "claude error text, quota",
			r:      clievent.SendResult{SubType: "success", IsError: true, Text: "Credit balance is too low"},
			failed: true, detail: []string{"Credit balance"}, cause: cron.TurnCauseQuota,
		},
		{
			name:   "claude api envelope, auth",
			r:      clievent.SendResult{SubType: "success", IsError: true, Text: "API Error: 401 authentication_error"},
			failed: true, detail: []string{"authentication_error"}, cause: cron.TurnCauseBackendAuth,
		},
		{
			name:   "claude api envelope, rate limit",
			r:      clievent.SendResult{SubType: "success", IsError: true, Text: "API Error: 429 rate_limit_error"},
			failed: true, detail: []string{"rate_limit_error"}, cause: cron.TurnCauseBackendRateLimited,
		},
		{
			name:   "claude api envelope, permission",
			r:      clievent.SendResult{SubType: "success", IsError: true, Text: "API Error: 403 permission_error"},
			failed: true, detail: []string{"permission_error"}, cause: cron.TurnCausePermission,
		},
		{
			name:   "claude api envelope, timeout",
			r:      clievent.SendResult{SubType: "success", IsError: true, Text: "API Error: request timed out"},
			failed: true, detail: []string{"timed out"}, cause: cron.TurnCauseBackendUnreachable,
		},
		{
			name:   "claude error text nobody recognises",
			r:      clievent.SendResult{SubType: "success", IsError: true, Text: "Execution error"},
			failed: true, detail: []string{"Execution error"}, cause: cron.TurnCauseUnknown,
		},
		{
			name: "backend rpc rejection",
			r: clievent.SendResult{
				SubType: "error", IsError: true, Text: "[kiro] acp rpc error -32001: overloaded",
				BackendError: &clievent.BackendError{Backend: "kiro", Code: -32001, Message: "overloaded"},
			},
			failed: true, detail: []string{"kiro", "-32001", "overloaded"}, cause: cron.TurnCauseBackendOverloaded,
		},
		{
			name: "backend invalid params",
			r: clievent.SendResult{
				SubType: "error", IsError: true, Text: "[codex] rpc error -32602: invalid params",
				BackendError: &clievent.BackendError{Backend: "codex", Code: -32602, Message: "invalid params"},
			},
			failed: true, detail: []string{"-32602"}, cause: cron.TurnCauseBackendInvalid,
		},
		{
			name: "backend rejection with an unmatched message",
			r: clievent.SendResult{
				SubType: "error", IsError: true, Text: "[kiro] acp rpc error -32000: something odd",
				BackendError: &clievent.BackendError{Backend: "kiro", Code: -32000, Message: "something odd"},
			},
			failed: true, detail: []string{"something odd"}, cause: cron.TurnCauseUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := tc.r
			err := turnFailure(&r)
			if !tc.failed {
				if err != nil {
					t.Fatalf("turnFailure = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, cron.ErrTurnFailed) {
				t.Fatalf("turnFailure = %v, want it to wrap cron.ErrTurnFailed", err)
			}
			if !strings.HasPrefix(err.Error(), "cron: turn failed (") {
				t.Errorf("error %q lost the run-history shape", err)
			}
			for _, d := range tc.detail {
				if !strings.Contains(err.Error(), d) {
					t.Errorf("error %q lacks detail %q", err, d)
				}
			}
			var tf *cron.TurnFailedError
			if !errors.As(err, &tf) {
				t.Fatalf("turnFailure = %v, want a cron.TurnFailedError in the chain", err)
			}
			if tf.Cause != tc.cause {
				t.Errorf("cause = %q, want %q", tf.Cause, tc.cause)
			}
		})
	}
}

// TestCronSessionAdapter_SendReportsFailedTurn drives the adapter over a real
// ManagedSession: an is_error result reaches cron as ErrTurnFailed with the
// text and session id still returned, and a healthy result stays error-free.
func TestCronSessionAdapter_SendReportsFailedTurn(t *testing.T) {
	t.Parallel()
	r := session.NewRouter(session.RouterConfig{})
	t.Cleanup(r.Shutdown)
	var next *clievent.SendResult
	proc := session.NewTestProcess()
	proc.SendFunc = func(context.Context, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
		res := *next
		return &res, nil
	}
	a := cronSessionAdapter{s: r.InjectSession("cron:job-turn-failed", proc)}

	next = &clievent.SendResult{SessionID: "sess-1", SubType: "error_max_turns", IsError: true}
	got, err := a.Send(context.Background(), "ping")
	if !errors.Is(err, cron.ErrTurnFailed) {
		t.Fatalf("Send err = %v, want cron.ErrTurnFailed", err)
	}
	if got.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", got.SessionID)
	}

	next = &clievent.SendResult{Text: "done", SessionID: "sess-1", SubType: "success"}
	if got, err := a.Send(context.Background(), "ping"); err != nil || got.Text != "done" {
		t.Errorf("healthy turn: got (%+v, %v), want (done, nil)", got, err)
	}
}

// TestExitFailure: a CLI exit claude made over a stale --resume becomes
// cron.ErrTurnFailed with TurnCauseResumeUnavailable, the exit still in the
// chain; every other error, including the other exit classes, passes through.
func TestExitFailure(t *testing.T) {
	t.Parallel()
	resume := fmt.Errorf("send: %w", &clierr.ProcessExitedError{Code: 1, Class: clierr.ExitResumeNotFound})
	err := exitFailure(resume)
	var tf *cron.TurnFailedError
	if !errors.As(err, &tf) || tf.Cause != cron.TurnCauseResumeUnavailable {
		t.Fatalf("exitFailure(resume) = %v, want a TurnFailedError with TurnCauseResumeUnavailable", err)
	}
	var pe *clierr.ProcessExitedError
	if !errors.Is(err, cron.ErrTurnFailed) || !errors.As(err, &pe) || pe.Code != 1 {
		t.Errorf("exitFailure(resume) = %v, want ErrTurnFailed with the exit kept in the chain", err)
	}
	if got, want := err.Error(), "cron: turn failed: send: process exited during send (code 1)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	for _, other := range []error{
		&clierr.ProcessExitedError{Code: 1, Class: clierr.ExitAuth},
		&clierr.ProcessExitedError{Code: 1, Class: clierr.ExitUnknown},
		clierr.ErrProcessExited,
		clierr.ErrNoOutputTimeout,
		context.DeadlineExceeded,
	} {
		if got := exitFailure(other); got != other {
			t.Errorf("exitFailure(%v) = %v, want it unchanged", other, got)
		}
	}
}

// TestCronSessionAdapter_SendNamesResumeUnavailable drives the adapter over a
// real ManagedSession whose Send fails with claude's stale-resume exit.
func TestCronSessionAdapter_SendNamesResumeUnavailable(t *testing.T) {
	t.Parallel()
	r := session.NewRouter(session.RouterConfig{})
	t.Cleanup(r.Shutdown)
	proc := session.NewTestProcess()
	proc.SendFunc = func(context.Context, string, []clievent.Attachment, clievent.EventCallback) (*clievent.SendResult, error) {
		return nil, &clierr.ProcessExitedError{Code: 1, Class: clierr.ExitResumeNotFound}
	}
	a := cronSessionAdapter{s: r.InjectSession("cron:job-stale-resume", proc)}
	_, err := a.Send(context.Background(), "ping")
	var tf *cron.TurnFailedError
	if !errors.As(err, &tf) || tf.Cause != cron.TurnCauseResumeUnavailable {
		t.Fatalf("Send err = %v, want a TurnFailedError with TurnCauseResumeUnavailable", err)
	}
	if !errors.Is(err, clierr.ErrProcessExited) {
		t.Errorf("Send err = %v, want the exit kept in the chain", err)
	}
}

// TestToCronAdoptedOutcome: an adopted turn that completed as a failure is
// Completed with TurnErr; a healthy one has none; an abort in either CLI's
// shape and a CLI exit stay not-completed.
func TestToCronAdoptedOutcome(t *testing.T) {
	t.Parallel()
	failed := toCronAdoptedOutcome(cli.AdoptedOutcome{End: cli.AdoptedEndResult, Result: clievent.SendResult{
		SubType: "error_max_turns", IsError: true, SessionID: "sess-a",
	}})
	if !failed.Completed || !errors.Is(failed.TurnErr, cron.ErrTurnFailed) || failed.SessionID != "sess-a" {
		t.Errorf("failed turn = %+v, want Completed with TurnErr wrapping ErrTurnFailed", failed)
	}
	var tf *cron.TurnFailedError
	if !errors.As(failed.TurnErr, &tf) || tf.Cause != cron.TurnCauseMaxTurns {
		t.Errorf("failed turn TurnErr = %v, want it to carry TurnCauseMaxTurns", failed.TurnErr)
	}
	ok := toCronAdoptedOutcome(cli.AdoptedOutcome{End: cli.AdoptedEndResult, Result: clievent.SendResult{Text: "hi", SubType: "success"}})
	if !ok.Completed || ok.TurnErr != nil || ok.Text != "hi" {
		t.Errorf("healthy turn = %+v, want Completed without TurnErr", ok)
	}
	for _, out := range []cli.AdoptedOutcome{
		{End: cli.AdoptedEndResult, Result: clievent.SendResult{SubType: "error_during_execution", IsError: true}},
		{End: cli.AdoptedEndResult, Result: clievent.SendResult{SubType: "success", TerminalReason: "aborted_tools"}},
		{End: cli.AdoptedEndResult, Result: clievent.SendResult{SubType: "success", TerminalReason: "aborted_streaming", Text: "partial essay"}},
		{End: cli.AdoptedEndResult, Result: clievent.SendResult{SubType: "success", IsError: true, TerminalReason: "aborted_tools"}},
		{End: cli.AdoptedEndCLIExited},
	} {
		if got := toCronAdoptedOutcome(out); got.Completed || got.TurnErr != nil {
			t.Errorf("%+v → %+v, want not completed", out, got)
		}
	}
}

// TestCronSessionAdapter_NoWatermarkWithoutAShimProcess: a session whose
// process is not a shim-backed *cli.Process has no stream position to offer,
// and must report none rather than a zero position every replayed result
// would sit past.
func TestCronSessionAdapter_NoWatermarkWithoutAShimProcess(t *testing.T) {
	t.Parallel()
	r := session.NewRouter(session.RouterConfig{})
	t.Cleanup(r.Shutdown)
	a := cronSessionAdapter{s: r.InjectSession("cron:job-no-watermark", session.NewTestProcess())}
	if got := a.SendWatermark(); got != "" {
		t.Errorf("SendWatermark = %q, want empty", got)
	}
	if run, v := (cronRouterAdapter{r: r}).AdoptInFlight("cron:job-no-watermark", "not-a-watermark"); v != cron.AdoptNone || run != nil {
		t.Errorf("AdoptInFlight = (%v, %v), want (nil, AdoptNone)", run, v)
	}
}
