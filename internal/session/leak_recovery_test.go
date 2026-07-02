package session

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/leakguard"
)

// approxEq compares two float64 cost values with a tolerance that swallows IEEE
// 754 addition rounding (0.10 + 0.05 == 0.15000000000000002).
func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

const leakSample = "先跑一下。\n\ncall\n<invoke name=\"Bash\">\n<parameter name=\"command\">echo hi</parameter>\n</invoke>"

// newLeakSession builds a bare ManagedSession bound to a TestProcess whose Send
// is scripted by sendFunc. No run-history store, so finishRun is a no-op and
// the tests isolate the recovery behaviour.
func newLeakSession(sendFunc func(context.Context, string, []cli.ImageData, cli.EventCallback) (*cli.SendResult, error)) (*ManagedSession, *TestProcess) {
	proc := &TestProcess{AliveVal: true, SendFunc: sendFunc}
	s := &ManagedSession{key: "feishu:p2p:leak"}
	s.storeProcess(proc)
	return s, proc
}

// resendOnce returns a resend closure that records how many times it fired and
// what nudge text it was handed, replaying scripted results in order.
func resendOnce(calls *int, gotNudge *string, results []*cli.SendResult, errs []error) func(context.Context, string) (*cli.SendResult, error) {
	return func(_ context.Context, nudge string) (*cli.SendResult, error) {
		i := *calls
		*calls++
		*gotNudge = nudge
		var r *cli.SendResult
		var e error
		if i < len(results) {
			r = results[i]
		}
		if i < len(errs) {
			e = errs[i]
		}
		return r, e
	}
}

func TestRecover_LeakThenClean_FiresOnce(t *testing.T) {
	t.Setenv(leakRecoveryEnvVar, "1")
	s, proc := newLeakSession(nil)
	orig := &cli.SendResult{Text: leakSample, SessionID: "sess-1", CostUSD: 0.10}

	var calls int
	var nudge string
	resend := resendOnce(&calls, &nudge,
		[]*cli.SendResult{{Text: "已执行完成。", SessionID: "sess-1", CostUSD: 0.05}}, nil)

	got := s.recoverLeakedToolcall(context.Background(), proc, orig, resend)
	if calls != 1 {
		t.Fatalf("resend fired %d times, want exactly 1", calls)
	}
	if got.Text != "已执行完成。" {
		t.Errorf("Text = %q, want recovered text", got.Text)
	}
	if !approxEq(got.CostUSD, 0.15) {
		t.Errorf("CostUSD = %v, want 0.15 (summed)", got.CostUSD)
	}
	if got.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", got.SessionID)
	}
}

func TestRecover_LeakThenLeak_NoLoop(t *testing.T) {
	t.Setenv(leakRecoveryEnvVar, "1")
	s, proc := newLeakSession(nil)
	orig := &cli.SendResult{Text: leakSample, CostUSD: 0.10}

	var calls int
	var nudge string
	// The retry ALSO leaks — cap=1 must stop, not retry again.
	resend := resendOnce(&calls, &nudge,
		[]*cli.SendResult{{Text: leakSample, CostUSD: 0.05}}, nil)

	got := s.recoverLeakedToolcall(context.Background(), proc, orig, resend)
	if calls != 1 {
		t.Fatalf("resend fired %d times, want exactly 1 (cap=1, no loop)", calls)
	}
	// Returned text must have the leaked XML stripped (no <invoke> wall to IM).
	if got.Text == "" || got.Text != "先跑一下。" {
		t.Errorf("Text = %q, want stripped prose %q", got.Text, "先跑一下。")
	}
	if !approxEq(got.CostUSD, 0.15) {
		t.Errorf("CostUSD = %v, want 0.15 (summed even on failure)", got.CostUSD)
	}
}

func TestRecover_CleanResult_NoResend(t *testing.T) {
	t.Setenv(leakRecoveryEnvVar, "1")
	s, proc := newLeakSession(nil)
	orig := &cli.SendResult{Text: "完全正常的回复，没有工具调用。", CostUSD: 0.10}

	var calls int
	var nudge string
	got := s.recoverLeakedToolcall(context.Background(), proc, orig,
		resendOnce(&calls, &nudge, nil, nil))
	if calls != 0 {
		t.Fatalf("resend fired %d times on clean result, want 0", calls)
	}
	if got != orig {
		t.Error("clean result must be returned unchanged (same pointer)")
	}
}

func TestRecover_KillSwitchOff_NoResend(t *testing.T) {
	t.Setenv(leakRecoveryEnvVar, "off")
	s, proc := newLeakSession(nil)
	orig := &cli.SendResult{Text: leakSample, CostUSD: 0.10}

	var calls int
	var nudge string
	got := s.recoverLeakedToolcall(context.Background(), proc, orig,
		resendOnce(&calls, &nudge, nil, nil))
	if calls != 0 {
		t.Fatalf("resend fired %d times with flag off, want 0", calls)
	}
	// Flag off: return the leaked result untouched (dashboard still folds it).
	if got != orig {
		t.Error("with flag off the original result must pass through unchanged")
	}
}

func TestRecover_ProseQuotedInvoke_NoFalsePositive(t *testing.T) {
	t.Setenv(leakRecoveryEnvVar, "1")
	s, proc := newLeakSession(nil)
	// Legitimate technical prose quoting invoke syntax in backticks.
	orig := &cli.SendResult{Text: "语法是 `<invoke name=\"X\">` 配对 `</invoke>`，别当真执行。"}

	var calls int
	var nudge string
	s.recoverLeakedToolcall(context.Background(), proc, orig,
		resendOnce(&calls, &nudge, nil, nil))
	if calls != 0 {
		t.Fatalf("resend fired %d times on quoted-syntax prose, want 0 (false positive)", calls)
	}
}

func TestRecover_PassthroughFollower_MergedCount_NoResend(t *testing.T) {
	t.Setenv(leakRecoveryEnvVar, "1")
	s, proc := newLeakSession(nil)
	// A follower slot: MergedCount>1 with empty Text. Even though a sibling's
	// head text leaked, the follower must not run its own recovery.
	orig := &cli.SendResult{Text: "", MergedCount: 2}

	var calls int
	var nudge string
	s.recoverLeakedToolcall(context.Background(), proc, orig,
		resendOnce(&calls, &nudge, nil, nil))
	if calls != 0 {
		t.Fatalf("resend fired %d times on merged follower, want 0", calls)
	}
}

func TestRecover_ResendError_ReturnsStrippedOriginal(t *testing.T) {
	t.Setenv(leakRecoveryEnvVar, "1")
	s, proc := newLeakSession(nil)
	orig := &cli.SendResult{Text: leakSample, CostUSD: 0.10}

	var calls int
	var nudge string
	resend := resendOnce(&calls, &nudge, nil, []error{errors.New("process exited")})

	got := s.recoverLeakedToolcall(context.Background(), proc, orig, resend)
	if calls != 1 {
		t.Fatalf("resend fired %d times, want 1", calls)
	}
	if got.Text != "先跑一下。" {
		t.Errorf("Text = %q, want stripped prose on re-send error", got.Text)
	}
}

func TestRecover_CtxCancelled_NoResend(t *testing.T) {
	t.Setenv(leakRecoveryEnvVar, "1")
	s, proc := newLeakSession(nil)
	orig := &cli.SendResult{Text: leakSample}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls int
	var nudge string
	s.recoverLeakedToolcall(ctx, proc, orig,
		resendOnce(&calls, &nudge, nil, nil))
	if calls != 0 {
		t.Fatalf("resend fired %d times with cancelled ctx, want 0", calls)
	}
}

func TestRecover_ProcDead_NoResend(t *testing.T) {
	t.Setenv(leakRecoveryEnvVar, "1")
	s, proc := newLeakSession(nil)
	proc.AliveVal = false
	orig := &cli.SendResult{Text: leakSample}

	var calls int
	var nudge string
	s.recoverLeakedToolcall(context.Background(), proc, orig,
		resendOnce(&calls, &nudge, nil, nil))
	if calls != 0 {
		t.Fatalf("resend fired %d times on dead process, want 0", calls)
	}
}

func TestRecover_PromptStringExact(t *testing.T) {
	t.Setenv(leakRecoveryEnvVar, "1")
	s, proc := newLeakSession(nil)
	orig := &cli.SendResult{Text: leakSample}

	var calls int
	var nudge string
	s.recoverLeakedToolcall(context.Background(), proc, orig,
		resendOnce(&calls, &nudge, []*cli.SendResult{{Text: "done"}}, nil))
	if nudge != leakContinuePrompt {
		t.Errorf("nudge text drifted:\n got: %q\nwant: %q", nudge, leakContinuePrompt)
	}
	// The nudge itself must not be detectable as a leak (else echoing it back
	// could re-trip the detector on the next turn).
	if leakguard.Detect(leakContinuePrompt) {
		t.Error("leakContinuePrompt must not itself match the leak detector")
	}
}

// TestSend_LeakRecovery_EndToEnd drives the public legacy Send path: first turn
// leaks, the auto-injected continue turn returns clean, and Send hands back the
// recovered text — no manual "continue" needed.
func TestSend_LeakRecovery_EndToEnd(t *testing.T) {
	t.Setenv(leakRecoveryEnvVar, "1")
	var turn int
	s, _ := newLeakSession(func(_ context.Context, text string, _ []cli.ImageData, _ cli.EventCallback) (*cli.SendResult, error) {
		turn++
		if turn == 1 {
			return &cli.SendResult{Text: leakSample, CostUSD: 0.10}, nil
		}
		if text != leakContinuePrompt {
			t.Errorf("turn 2 got text %q, want the continue nudge", text)
		}
		return &cli.SendResult{Text: "工具已执行，任务完成。", CostUSD: 0.05}, nil
	})

	got, err := s.Send(context.Background(), "do the thing", nil, nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if turn != 2 {
		t.Fatalf("Send drove %d turns, want 2 (original + recovery)", turn)
	}
	if got.Text != "工具已执行，任务完成。" {
		t.Errorf("Send returned %q, want recovered text", got.Text)
	}
	if !approxEq(got.CostUSD, 0.15) {
		t.Errorf("CostUSD = %v, want 0.15 summed", got.CostUSD)
	}
}
