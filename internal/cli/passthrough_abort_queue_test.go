package cli

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/testhelper"
)

// pendingCount is the number of slots the CLI still owes a turn.
func (s *passthroughShim) pendingCount() int {
	s.proc.slots.mu.Lock()
	defer s.proc.slots.mu.Unlock()
	return len(s.proc.slots.pending)
}

// An aborted turn that no priority:"now" message caused leaves the CLI's queue
// intact, so the message queued behind it stays pending and gets its own turn's
// result.
func TestSendPassthrough_AbortWithoutUrgentKeepsQueuedSlots(t *testing.T) {
	for _, tc := range []struct {
		name        string
		abort       func(*testing.T, *passthroughShim)
		wantAborted bool
	}{
		{"InterruptViaControl", func(t *testing.T, sh *passthroughShim) {
			if err := sh.proc.InterruptViaControl(); err != nil {
				t.Fatalf("InterruptViaControl: %v", err)
			}
			if in := sh.expectWrite(t, 2*time.Second); in.Type != "control_request" {
				t.Fatalf("stdin write = %q, want the control_request", in.Type)
			}
		}, true},
		{"Interrupt", func(_ *testing.T, sh *passthroughShim) { sh.proc.Interrupt() }, true},
		{"mid-turn CLI error", func(*testing.T, *passthroughShim) {}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh := startWatchdogShim(t, time.Minute, time.Minute, parkedWatchdog)

			uuidA, outA := sh.sendAsync(t, context.Background(), "A")
			playTurnStart(t, sh, uuidA, "A")
			uuidB, outB := sh.sendAsync(t, context.Background(), "B")
			tc.abort(t, sh)
			sh.srv.SendStdout(abortedResult)

			a := waitOut(t, "A", outA, 3*time.Second)
			if a.err != nil || a.res.SubType != "error_during_execution" || a.res.Aborted != tc.wantAborted {
				t.Fatalf("A = %+v, %v; want an error_during_execution result with Aborted=%v", a.res, a.err, tc.wantAborted)
			}
			// A's fan-out is done, so a dropped B would already be gone.
			if n := sh.pendingCount(); n != 1 {
				t.Fatalf("pending slots after the aborted turn = %d, want B still queued", n)
			}
			if st := sh.proc.State(); st != StateRunning {
				t.Errorf("State = %v with B queued, want Running", st)
			}

			playTurnStart(t, sh, uuidB, "B")
			sh.emitResult("s1", "B done")
			if b := waitOut(t, "B", outB, 3*time.Second); b.err != nil || b.res.Text != "B done" {
				t.Fatalf("B = %+v, %v; want its own result", b.res, b.err)
			}
			testhelper.Eventually(t, func() bool { return sh.proc.State() == StateReady }, 2*time.Second,
				"State never returned to Ready after B's turn")
		})
	}
}

// aborted288Result is the frame claude 2.1.288 emits for a turn a
// priority:"now" message preempted mid-generation.
const aborted288Result = `{"type":"result","subtype":"success","is_error":false,"terminal_reason":"aborted_streaming","result":"partial","session_id":"s1"}`

// A priority:"now" preemption keeps the CLI's queue too: it runs the urgent
// message next and the one queued before it afterwards, each with its own
// replay (docs/rfc/passthrough-mode-validation.md V10).
func TestSendPassthrough_UrgentKeepsQueuedSlots(t *testing.T) {
	for _, tc := range []struct{ name, aborted, wantText string }{
		{"error_during_execution", abortedResult, ""},
		{"terminal_reason aborted_streaming", aborted288Result, "partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh := startWatchdogShim(t, time.Minute, time.Minute, parkedWatchdog)

			uuidA, outA := sh.sendAsync(t, context.Background(), "A")
			playTurnStart(t, sh, uuidA, "A")
			uuidB, outB := sh.sendAsync(t, context.Background(), "B")
			outC := make(chan passthroughOut, 1)
			go func() {
				res, err := sh.proc.SendPassthrough(context.Background(), "C", nil, nil, "now")
				outC <- passthroughOut{res, err}
			}()
			uuidC := sh.expectWrite(t, 2*time.Second).UUID
			sh.srv.SendStdout(tc.aborted)

			if a := waitOut(t, "A", outA, 3*time.Second); a.err != nil || !a.res.Aborted || a.res.Text != tc.wantText {
				t.Fatalf("A = %+v, %v; want the preempted turn's aborted result", a.res, a.err)
			}
			if n := sh.pendingCount(); n != 2 {
				t.Fatalf("pending slots after the preempted turn = %d, want B and C still queued", n)
			}
			if st := sh.proc.State(); st != StateRunning {
				t.Errorf("State = %v with B and C queued, want Running", st)
			}

			playTurnStart(t, sh, uuidC, "C")
			sh.emitResult("s1", "C done")
			if c := waitOut(t, "C", outC, 3*time.Second); c.err != nil || c.res.Text != "C done" {
				t.Fatalf("C = %+v, %v; want its own result", c.res, c.err)
			}
			select {
			case b := <-outB:
				t.Fatalf("B returned %+v, %v before its own turn", b.res, b.err)
			default:
			}

			playTurnStart(t, sh, uuidB, "B")
			sh.emitResult("s1", "B done")
			if b := waitOut(t, "B", outB, 3*time.Second); b.err != nil || b.res.Text != "B done" {
				t.Fatalf("B = %+v, %v; want its own result", b.res, b.err)
			}
			testhelper.Eventually(t, func() bool { return sh.proc.State() == StateReady }, 2*time.Second,
				"State never returned to Ready after B's turn")
		})
	}
}
