package cli

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/shim"
)

// ---------------------------------------------------------------------------
// #1779: drainStaleEvents re-enqueue must not panic when readLoop has closed
// eventCh between the isChanAlive(done) guard and the actual send.
// ---------------------------------------------------------------------------

// TestSafeReenqueue_SendOnClosedDoesNotPanic exercises safeReenqueue directly
// against an already-closed eventCh. readLoop closes done strictly before
// eventCh, but a producer that passed the isChanAlive(done) guard can still
// race the eventCh close. A bare `select { case ch<-ev: default: }` panics on
// a closed channel (the send case is always ready-to-run, so select picks it).
// safeReenqueue must recover and drop silently.
func TestSafeReenqueue_SendOnClosedDoesNotPanic(t *testing.T) {
	t.Parallel()

	p := &Process{
		eventCh: make(chan Event, 1),
		done:    make(chan struct{}),
	}
	close(p.eventCh)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("safeReenqueue panicked on closed eventCh: %v", r)
		}
	}()

	p.safeReenqueue(Event{Type: "result", SessionID: "race"})
}

// TestDrainStaleEvents_CtxDoneReenqueueOnClosedChan pins #1779 at the
// drainStaleEvents level via the ctx.Done re-enqueue arm. Setup: the previous
// turn was interrupted-while-running, so drain enters the settle window; the
// channel is closed there (CLI exited) which short-circuits to `drain`. We
// then drive the ctx.Done re-enqueue with done still OPEN (isChanAlive lets the
// re-enqueue proceed) but eventCh CLOSED — the exact race the recover guards.
// Without safeReenqueue's recover this panics; with it the drain returns the
// ctx error cleanly.
func TestDrainStaleEvents_CtxDoneReenqueueOnClosedChan(t *testing.T) {
	t.Parallel()

	p := &Process{
		eventCh: make(chan Event, 4),
		done:    make(chan struct{}), // OPEN: isChanAlive reports alive
	}
	p.interrupted.Store(true)
	p.interruptedRun.Store(true)

	// One post-cutoff event so holdback is non-empty when we reach the
	// re-enqueue, then close the channel so the settle-window read observes
	// ok==false and jumps to drain, and any re-enqueue targets a dead channel.
	future := time.Now().Add(time.Hour)
	p.eventCh <- Event{Type: "assistant", SessionID: "fresh", recvAt: future}
	close(p.eventCh)

	// Pre-cancel so the drain loop's ctx.Done arm fires and exercises the
	// holdback re-enqueue path onto the now-closed eventCh.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("drainStaleEvents panicked on closed eventCh: %v", r)
		}
	}()

	// Either the ctx error or nil is acceptable here; the assertion under test
	// is "does not panic". The send-on-closed would crash the goroutine.
	_ = p.drainStaleEvents(ctx)
}

// ---------------------------------------------------------------------------
// #1778: SpawnReconnect must arm reconnectedMidTurn + StateRunning BEFORE the
// readLoop starts, so a result that arrives immediately after reconnect does
// not get processed by the stray-result handler while the flag is still unset
// (which would strand the session in StateRunning).
// ---------------------------------------------------------------------------

// TestReconnectMidTurn_ResultTransitionsToReady simulates the post-reconnect
// invariant the #1778 ordering fix protects: once reconnectedMidTurn is armed
// and state is Running, a `result` event delivered by readLoop transitions the
// state back to Ready (no Send() ever runs). With the arm happening before
// startReadLoop, even a result arriving in the first read iteration is handled.
func TestReconnectMidTurn_ResultTransitionsToReady(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	defer p.Kill()

	// Mirror SpawnReconnect's ordering: arm BEFORE the read loop starts.
	p.mu.Lock()
	p.state = StateRunning
	p.mu.Unlock()
	p.reconnectedMidTurn.Store(true)

	done := make(chan struct{}, 1)
	p.SetOnTurnDone(func() {
		select {
		case done <- struct{}{}:
		default:
		}
	})

	p.startReadLoop()

	// Result arrives with no active Send(): the stray-result handler must
	// consume the armed flag and flip Running → Ready.
	srv.SendStdout(`{"type":"result","result":"done","session_id":"s1"}`)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("onTurnDone not called: armed reconnect result did not settle the turn")
	}

	// Allow the readLoop state transition to be observed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.GetState() == StateReady {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("State = %v after reconnect result, want StateReady (session stuck Running)", p.GetState())
}

// TestIsMidTurn covers the helper SpawnReconnect uses to decide whether to arm
// reconnectedMidTurn. The arm decision depends ONLY on the already-drained
// replays, which is why #1778 can safely evaluate it before startReadLoop.
func TestIsMidTurn(t *testing.T) {
	proto := &ClaudeProtocol{}

	tests := []struct {
		name    string
		replays []shim.ServerMsg
		want    bool
	}{
		{
			name:    "no replays is not mid-turn",
			replays: nil,
			want:    false,
		},
		{
			name: "last event is result -> turn complete",
			replays: []shim.ServerMsg{
				{Type: "replay", Line: `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]},"session_id":"s1"}`},
				{Type: "replay", Line: `{"type":"result","result":"ok","session_id":"s1"}`},
			},
			want: false,
		},
		{
			name: "last event is assistant -> mid turn",
			replays: []shim.ServerMsg{
				{Type: "replay", Line: `{"type":"result","result":"ok","session_id":"s1"}`},
				{Type: "replay", Line: `{"type":"assistant","message":{"content":[{"type":"text","text":"still going"}]},"session_id":"s1"}`},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMidTurn(tt.replays, proto); got != tt.want {
				t.Errorf("isMidTurn() = %v, want %v", got, tt.want)
			}
		})
	}
}
