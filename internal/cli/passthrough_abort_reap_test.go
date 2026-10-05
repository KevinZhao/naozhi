package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
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
// result instead of ErrAbortedByUrgent.
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
			// The reap runs before A's fan-out, so B is already gone if it ran.
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

// A priority:"now" preemption still fails the messages queued behind the
// aborted turn; the urgent message itself runs next.
func TestSendPassthrough_UrgentReapsQueuedSlots(t *testing.T) {
	sh := startWatchdogShim(t, time.Minute, time.Minute, parkedWatchdog)

	uuidA, outA := sh.sendAsync(t, context.Background(), "A")
	playTurnStart(t, sh, uuidA, "A")
	_, outB := sh.sendAsync(t, context.Background(), "B")
	outC := make(chan passthroughOut, 1)
	go func() {
		res, err := sh.proc.SendPassthrough(context.Background(), "C", nil, nil, "now")
		outC <- passthroughOut{res, err}
	}()
	uuidC := sh.expectWrite(t, 2*time.Second).UUID
	sh.srv.SendStdout(abortedResult)

	if a := waitOut(t, "A", outA, 3*time.Second); a.err != nil || !a.res.Aborted {
		t.Fatalf("A = %+v, %v; want the preempted turn's aborted result", a.res, a.err)
	}
	if b := waitOut(t, "B", outB, 3*time.Second); !errors.Is(b.err, clierr.ErrAbortedByUrgent) {
		t.Fatalf("B err = %v, want ErrAbortedByUrgent", b.err)
	}
	playTurnStart(t, sh, uuidC, "C")
	sh.emitResult("s1", "C done")
	if c := waitOut(t, "C", outC, 3*time.Second); c.err != nil || c.res.Text != "C done" {
		t.Fatalf("C = %+v, %v; want its own result", c.res, c.err)
	}
}

// The reap keys on an un-replayed priority:"now" slot; a canceled one still
// counts because the CLI has the message either way.
func TestReapAbortedPreempted_NeedsPendingUrgentSlot(t *testing.T) {
	for _, tc := range []struct {
		name        string
		urgent      *sendSlot
		wantVictims int
	}{
		{"no urgent slot", nil, 0},
		{"urgent slot pending", &sendSlot{id: 9, priority: "now"}, 2},
		{"urgent slot canceled", func() *sendSlot {
			s := &sendSlot{id: 9, priority: "now"}
			s.canceled.Store(true)
			return s
		}(), 2},
		{"urgent slot already replayed", &sendSlot{id: 9, priority: "now", replayed: true}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pending := []*sendSlot{{id: 1, replayed: true}, {id: 2}, {id: 3, priority: "next"}}
			if tc.urgent != nil {
				pending = append(pending, tc.urgent)
			}
			p := &Process{slots: sendSlots{pending: pending}}
			before := len(pending)

			victims := p.reapAbortedPreempted()
			if len(victims) != tc.wantVictims {
				t.Fatalf("victims = %d, want %d", len(victims), tc.wantVictims)
			}
			if got := len(p.slots.pending); got != before-tc.wantVictims {
				t.Errorf("pending after reap = %d, want %d", got, before-tc.wantVictims)
			}
		})
	}
}
