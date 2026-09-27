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

// TestSend_WhileRunningIsBusy: a second Send during a turn is refused as
// busy and does not end the first Send's turn.
func TestSend_WhileRunningIsBusy(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	defer srv.Close()
	p.startReadLoop()
	p.transition(evSendBegin) // the first Send's claim
	_, err := p.Send(context.Background(), "hello", nil, nil)
	if !errors.Is(err, clierr.ErrProcessBusy) {
		t.Errorf("Send during a turn = %v, want ErrProcessBusy", err)
	}
	if got := p.State(); got != StateRunning {
		t.Errorf("state after the refused Send = %v, want Running", got)
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
