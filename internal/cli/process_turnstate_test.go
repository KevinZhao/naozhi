package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// TestNextState is the transition table: for every state and event, where
// the process goes, and whether it moves at all.
func TestNextState(t *testing.T) {
	const (
		S = StateSpawning
		R = StateReady
		U = StateRunning
		D = StateDead
		x = ProcessState(-1) // no move
	)
	want := map[stateEvent][4]ProcessState{ // from Spawning, Ready, Running, Dead
		evReconnectMidTurn: {U, x, x, x},
		evReadLoopStart:    {R, x, x, x},
		evSendBegin:        {U, U, x, x},
		evSendEnd:          {x, x, R, x},
		evTurnStarted:      {U, U, x, x},
		evTurnEnded:        {x, x, R, x},
		evDied:             {D, D, D, x},
	}
	names := map[stateEvent]string{evReconnectMidTurn: "reconnectMidTurn", evReadLoopStart: "readLoopStart",
		evSendBegin: "sendBegin", evSendEnd: "sendEnd", evTurnStarted: "turnStarted", evTurnEnded: "turnEnded", evDied: "died"}
	for ev, row := range want {
		for i, from := range []ProcessState{S, R, U, D} {
			got, moved := nextState(from, ev)
			if row[i] == x {
				if moved {
					t.Errorf("%s from %v moved to %v, want no move", names[ev], from, got)
				}
				continue
			}
			if !moved || got != row[i] {
				t.Errorf("%s from %v = %v (moved=%v), want %v", names[ev], from, got, moved, row[i])
			}
		}
	}
}

// TestSend_OnADeadProcessFailsAndStaysDead: Send on a process whose CLI has
// exited reports the exit and leaves the process Dead. It used to claim the
// dead process as Running and hand it back as Ready, so State() reported a
// live idle process while Alive() said otherwise.
func TestSend_OnADeadProcessFailsAndStaysDead(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	p.startReadLoop()
	srv.SendCLIExited(1)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the read loop did not exit on cli_exited")
	}
	srv.Close()

	_, err := p.Send(context.Background(), "hello", nil, nil)
	if !errors.Is(err, clierr.ErrProcessExited) {
		t.Errorf("Send on a dead process = %v, want ErrProcessExited", err)
	}
	if got := p.State(); got != StateDead {
		t.Errorf("state after Send = %v, want Dead", got)
	}
}

// TestSend_WhileRunningWaits: a Send during a turn it does not own waits for
// that turn instead of refusing the message; it gives up only on its ctx,
// and leaves the running turn alone.
func TestSend_WhileRunningWaits(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	defer srv.Close()
	p.startReadLoop()
	p.transition(evTurnStarted) // the CLI's own turn (a background-task notification)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := p.Send(ctx, "hello", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Send during a turn = %v, want it to wait until its ctx ends", err)
	}
	if got := p.State(); got != StateRunning {
		t.Errorf("state after the waiting Send = %v, want Running", got)
	}
}

// TestClaimSendWait_ClaimsWhenTheTurnEnds: the wait ends with the claim as
// soon as the running turn ends, and with no claim when the process dies or
// the bound passes.
func TestClaimSendWait_ClaimsWhenTheTurnEnds(t *testing.T) {
	var ts turnState
	ts.state = StateReady
	ts.transition(evTurnStarted)
	got := make(chan bool, 1)
	go func() {
		_, claimed := ts.claimSendWait(context.Background(), "run-1", time.Minute)
		got <- claimed
	}()
	select {
	case <-got:
		t.Fatal("claimSendWait returned while the turn was still running")
	case <-time.After(50 * time.Millisecond):
	}
	ts.transition(evTurnEnded)
	select {
	case claimed := <-got:
		if !claimed || ts.currentRunID() != "run-1" {
			t.Fatalf("claimed=%v run=%q, want the claim for run-1", claimed, ts.currentRunID())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("claimSendWait did not wake when the turn ended")
	}

	// Running again (the claim above): a death ends the wait unclaimed.
	go func() {
		_, claimed := ts.claimSendWait(context.Background(), "run-2", time.Minute)
		got <- claimed
	}()
	testhelper.Eventually(t, func() bool {
		ts.mu.RLock()
		defer ts.mu.RUnlock()
		return ts.moved != nil
	}, 5*time.Second, "the second Send never started waiting")
	ts.transition(evDied)
	select {
	case claimed := <-got:
		if claimed {
			t.Fatal("claimSendWait claimed a dead process")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("claimSendWait did not wake when the process died")
	}

	var stuck turnState
	stuck.state = StateRunning
	if prev, claimed := stuck.claimSendWait(context.Background(), "run-3", 20*time.Millisecond); claimed || prev != StateRunning {
		t.Fatalf("claimSendWait past its bound = (%v, %v), want (Running, false)", prev, claimed)
	}
}

// TestSystemInit_WithoutASendMarksTheTurnRunning: on a replay backend a turn
// no Send owns (passthrough, or one queued before a reconnect) starts with
// system/init, and the process reports Running for it so Interrupt and the
// dashboard see the turn.
func TestSystemInit_WithoutASendMarksTheTurnRunning(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	defer srv.Close()
	p.startReadLoop()
	srv.SendStdout(`{"type":"system","subtype":"init","session_id":"sess-pt"}`)
	testhelper.Eventually(t, func() bool { return p.State() == StateRunning }, 5*time.Second,
		"system/init did not mark the turn Running")
}
