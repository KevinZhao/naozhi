package cli

import (
	"bufio"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// panicOnReadProtocol embeds ClaudeProtocol (so it advertises Replay/Priority
// and drives passthrough mode) but panics inside ReadEvent for any stdout
// frame. It deliberately does NOT implement the eventReaderInto optional
// interface, forcing readLoop down the p.protocol.ReadEvent(...) branch which
// then panics — exercising the readLoop panic-recover defer for real.
type panicOnReadProtocol struct {
	ClaudeProtocol
}

func (p *panicOnReadProtocol) ReadEvent(string) ([]Event, bool, error) {
	panic("injected readLoop panic")
}

// ReadEventInto must also panic: ClaudeProtocol provides a promoted
// ReadEventInto via embedding, and readLoop prefers it through the
// eventReaderInto assertion — so without this override the embedded
// (non-panicking) implementation would run instead.
func (p *panicOnReadProtocol) ReadEventInto(string, []Event) ([]Event, bool, error) {
	panic("injected readLoop panic")
}

// TestReadLoopPanic_DiscardsPendingSlots locks R202606f-GO-008: when readLoop
// panics mid-frame, its recover defer must call discardAllPending so any
// SendPassthrough caller parked on slot.resultCh/errCh unblocks immediately
// with ErrProcessExited instead of waiting out the totalTimeout+30s bail timer.
func TestReadLoopPanic_DiscardsPendingSlots(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	proto := &panicOnReadProtocol{}
	p := newShimProcess(
		clientConn,
		bufio.NewReader(clientConn),
		bufio.NewWriter(clientConn),
		proto, 0, 0,
		0, 0,
	)

	go p.readLoop()

	// Drain whatever the process writes to stdin (the user-message frame) so
	// the net.Pipe write doesn't block SendPassthrough.
	go func() {
		r := bufio.NewReader(serverConn)
		for {
			if _, err := r.ReadBytes('\n'); err != nil {
				return
			}
		}
	}()

	sendErr := make(chan error, 1)
	go func() {
		_, err := p.SendPassthrough(context.Background(), "hello", nil, nil, "")
		sendErr <- err
	}()

	// Wait until the slot is actually pending before triggering the panic so
	// the discard has something to fan out to.
	deadline := time.Now().Add(2 * time.Second)
	for p.PassthroughDepth() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("slot never became pending")
		}
		time.Sleep(time.Millisecond)
	}

	// Send a stdout frame from the shim side; readLoop will parse it as a
	// "stdout" shim message and call protocol.ReadEvent, which panics.
	srv := &shimTestServer{conn: serverConn, writer: bufio.NewWriter(serverConn)}
	srv.SendStdout(`{"type":"assistant"}`)

	select {
	case err := <-sendErr:
		if !errors.Is(err, ErrProcessExited) {
			t.Fatalf("SendPassthrough err = %v, want ErrProcessExited", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SendPassthrough did not unblock after readLoop panic; " +
			"discardAllPending missing from panic recover")
	}

	// readLoop must have transitioned the process to dead.
	if p.IsRunning() {
		t.Error("process still reports running after readLoop panic")
	}
}
