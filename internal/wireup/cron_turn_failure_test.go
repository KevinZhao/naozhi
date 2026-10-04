package wireup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/session"
)

// TestTurnFailure: a result the backend flagged as an error becomes
// cron.ErrTurnFailed with run-history detail; a healthy turn and an abort
// naozhi asked for (error_during_execution + Aborted) stay nil.
func TestTurnFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		r      clievent.SendResult
		failed bool
		detail []string
	}{
		{name: "success", r: clievent.SendResult{Text: "done", SubType: "success"}},
		{name: "error subtype without is_error", r: clievent.SendResult{SubType: "error_max_turns"}},
		{name: "own abort", r: clievent.SendResult{SubType: "error_during_execution", IsError: true, Aborted: true}},
		{
			name:   "max turns, empty text",
			r:      clievent.SendResult{SubType: "error_max_turns", IsError: true},
			failed: true, detail: []string{"error_max_turns"},
		},
		{
			name:   "unrequested error_during_execution",
			r:      clievent.SendResult{SubType: "error_during_execution", IsError: true},
			failed: true, detail: []string{"error_during_execution"},
		},
		{
			name:   "abort flag does not mute a different failure",
			r:      clievent.SendResult{SubType: "error_max_turns", IsError: true, Aborted: true},
			failed: true, detail: []string{"error_max_turns"},
		},
		{
			name:   "claude error text",
			r:      clievent.SendResult{SubType: "success", IsError: true, Text: "Prompt is too long"},
			failed: true, detail: []string{"Prompt is too long"},
		},
		{
			name: "backend rpc rejection",
			r: clievent.SendResult{
				SubType: "error", IsError: true, Text: "[kiro] acp rpc error -32001: overloaded",
				BackendError: &clievent.BackendError{Backend: "kiro", Code: -32001, Message: "overloaded"},
			},
			failed: true, detail: []string{"kiro", "-32001", "overloaded"},
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
			for _, d := range tc.detail {
				if !strings.Contains(err.Error(), d) {
					t.Errorf("error %q lacks detail %q", err, d)
				}
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

// TestToCronAdoptedOutcome: an adopted turn that completed as a failure is
// Completed with TurnErr; a healthy one has none; error_during_execution and a
// CLI exit stay not-completed.
func TestToCronAdoptedOutcome(t *testing.T) {
	t.Parallel()
	failed := toCronAdoptedOutcome(cli.AdoptedOutcome{End: cli.AdoptedEndResult, Result: clievent.SendResult{
		SubType: "error_max_turns", IsError: true, SessionID: "sess-a",
	}})
	if !failed.Completed || !errors.Is(failed.TurnErr, cron.ErrTurnFailed) || failed.SessionID != "sess-a" {
		t.Errorf("failed turn = %+v, want Completed with TurnErr wrapping ErrTurnFailed", failed)
	}
	ok := toCronAdoptedOutcome(cli.AdoptedOutcome{End: cli.AdoptedEndResult, Result: clievent.SendResult{Text: "hi", SubType: "success"}})
	if !ok.Completed || ok.TurnErr != nil || ok.Text != "hi" {
		t.Errorf("healthy turn = %+v, want Completed without TurnErr", ok)
	}
	for _, out := range []cli.AdoptedOutcome{
		{End: cli.AdoptedEndResult, Result: clievent.SendResult{SubType: "error_during_execution", IsError: true}},
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
