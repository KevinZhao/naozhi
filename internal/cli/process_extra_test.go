package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// startServerDrain starts a goroutine that reads and discards all data the
// client sends to the server side of the shim connection. This is required
// for shimWriter tests because net.Pipe blocks writes until the peer reads.
func startServerDrain(srv *shimTestServer) {
	go func() {
		buf := make([]byte, 4096)
		for {
			_, err := srv.conn.Read(buf)
			if err != nil {
				return
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// shimWriter.Write — fast path: single complete line, empty buffer
// ---------------------------------------------------------------------------

func TestShimWriter_FastPath_SingleLine(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	go p.readLoop()
	defer p.Kill()

	w := p.shimStdinWriter()
	data := []byte(`{"type":"user_message"}` + "\n")
	n, err := w.Write(data)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len(data) {
		t.Errorf("Write() n = %d, want %d", n, len(data))
	}
}

// ---------------------------------------------------------------------------
// shimWriter.Write — fast path: ErrMessageTooLarge (line too big)
// ---------------------------------------------------------------------------

func TestShimWriter_FastPath_TooLarge(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	go p.readLoop()
	defer p.Kill()

	w := p.shimStdinWriter()
	// maxStdinLineBytes = 12 MB; craft exactly maxStdinLineBytes+1 bytes of payload
	// plus a trailing newline (total = maxStdinLineBytes+2).
	bigLine := make([]byte, maxStdinLineBytes+2)
	bigLine[len(bigLine)-1] = '\n'
	_, err := w.Write(bigLine)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("Write() error = %v, want ErrMessageTooLarge", err)
	}
}

// ---------------------------------------------------------------------------
// shimWriter.Write — slow path: fragmented writes
// ---------------------------------------------------------------------------

func TestShimWriter_SlowPath_Fragmented(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	go p.readLoop()
	defer p.Kill()

	w := p.shimStdinWriter()
	// Write part1 without newline → goes through slow path (buffer)
	part1 := []byte(`{"type":"msg"`)
	part2 := []byte(`}` + "\n") // completes the line

	if n, err := w.Write(part1); err != nil || n != len(part1) {
		t.Fatalf("Write(part1) n=%d err=%v", n, err)
	}
	if n, err := w.Write(part2); err != nil || n != len(part2) {
		t.Fatalf("Write(part2) n=%d err=%v", n, err)
	}
}

// ---------------------------------------------------------------------------
// shimWriter.Write — slow path: multi-line in single write
// ---------------------------------------------------------------------------

func TestShimWriter_SlowPath_MultiLine(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	go p.readLoop()
	defer p.Kill()

	w := p.shimStdinWriter()
	// Two complete lines containing a mid-content newline → slow path
	twoLines := `{"type":"a"}` + "\n" + `{"type":"b"}` + "\n"
	n, err := w.Write([]byte(twoLines))
	if err != nil {
		t.Fatalf("Write(twoLines) error = %v", err)
	}
	if n != len(twoLines) {
		t.Errorf("n = %d, want %d", n, len(twoLines))
	}
}

// ---------------------------------------------------------------------------
// shimWriter.Write — slow path: ErrMessageTooLarge
// ---------------------------------------------------------------------------

func TestShimWriter_SlowPath_TooLarge(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	go p.readLoop()
	defer p.Kill()

	w := p.shimStdinWriter()
	// Write a partial line to force slow path, then a huge line terminator
	partial := []byte(`{"a":`)
	if _, err := w.Write(partial); err != nil {
		t.Fatalf("Write(partial) error = %v", err)
	}

	// Now append data that makes total line length exceed maxStdinLineBytes
	huge := make([]byte, maxStdinLineBytes+1)
	huge[len(huge)-1] = '\n'
	_, err := w.Write(huge)
	if err == nil {
		t.Error("expected ErrMessageTooLarge from slow path")
		return
	}
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("error = %v, want ErrMessageTooLarge", err)
	}
}

// ---------------------------------------------------------------------------
// Send — normal flow: result event received
// ---------------------------------------------------------------------------

func TestProcess_Send_ResultEvent(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()

	done := make(chan *SendResult, 1)
	sendErr := make(chan error, 1)
	go func() {
		r, err := p.Send(context.Background(), "hello", nil, nil)
		done <- r
		sendErr <- err
	}()

	time.Sleep(30 * time.Millisecond)
	srv.SendStdout(`{"type":"result","result":"answer","session_id":"sess1","total_cost_usd":0.01}`)

	select {
	case result := <-done:
		if err := <-sendErr; err != nil {
			t.Fatalf("Send() error = %v", err)
		}
		if result.Text != "answer" {
			t.Errorf("Text = %q, want answer", result.Text)
		}
		if result.SessionID != "sess1" {
			t.Errorf("SessionID = %q, want sess1", result.SessionID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send() timed out")
	}
}

// ---------------------------------------------------------------------------
// Send — context cancelled
// ---------------------------------------------------------------------------

func TestProcess_Send_CtxCancel(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Send(ctx, "hello", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// Send — process busy (StateRunning)
// ---------------------------------------------------------------------------

func TestProcess_Send_Busy(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()

	p.mu.Lock()
	p.State = StateRunning
	p.mu.Unlock()

	_, err := p.Send(context.Background(), "hello", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("expected busy error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Send — process exits during send
// ---------------------------------------------------------------------------

func TestProcess_Send_ProcessExits(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()

	errCh := make(chan error, 1)
	go func() {
		_, err := p.Send(context.Background(), "hello", nil, nil)
		errCh <- err
	}()

	time.Sleep(30 * time.Millisecond)
	srv.SendCLIExited(1) // exit without result

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error when process exits during send")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send() timed out")
	}
}

// ---------------------------------------------------------------------------
// Send — captures session ID from init event
// ---------------------------------------------------------------------------

func TestProcess_Send_CapturesSessionID(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()

	done := make(chan error, 1)
	go func() {
		_, err := p.Send(context.Background(), "hello", nil, nil)
		done <- err
	}()

	time.Sleep(30 * time.Millisecond)
	srv.SendStdout(`{"type":"system","subtype":"init","session_id":"session-abc"}`)
	srv.SendStdout(`{"type":"result","result":"done","session_id":"session-abc"}`)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Send error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send timed out")
	}

	if p.GetSessionID() != "session-abc" {
		t.Errorf("SessionID = %q, want session-abc", p.GetSessionID())
	}
}

// ---------------------------------------------------------------------------
// Send — onEvent callback for thinking block
// ---------------------------------------------------------------------------

func TestProcess_Send_OnEventCallback(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()

	var mu sync.Mutex
	var events []Event
	done := make(chan error, 1)
	go func() {
		_, err := p.Send(context.Background(), "hello", nil, func(ev Event) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		})
		done <- err
	}()

	time.Sleep(30 * time.Millisecond)
	srv.SendStdout(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","text":"analyzing"}]}}`)
	srv.SendStdout(`{"type":"result","result":"done"}`)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Send error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send timed out")
	}

	mu.Lock()
	evCount := len(events)
	mu.Unlock()
	if evCount == 0 {
		t.Error("expected onEvent to be called at least once")
	}
}

// ---------------------------------------------------------------------------
// Send — with images
// ---------------------------------------------------------------------------

func TestProcess_Send_WithImages(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()

	images := []ImageData{{Data: []byte("fake-png"), MimeType: "image/png"}}
	done := make(chan error, 1)
	go func() {
		_, err := p.Send(context.Background(), "describe", images, nil)
		done <- err
	}()

	time.Sleep(30 * time.Millisecond)
	srv.SendStdout(`{"type":"result","result":"it is an image"}`)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Send with images error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send timed out")
	}
}

// ---------------------------------------------------------------------------
// Interrupt
// ---------------------------------------------------------------------------

func TestProcess_Interrupt_WhenReady(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()

	p.Interrupt()
	if !p.interrupted.Load() {
		t.Error("interrupted should be true after Interrupt()")
	}
	// interruptedRun should be false (not in Running state)
	if p.interruptedRun.Load() {
		t.Error("interruptedRun should be false when not running")
	}
}

func TestProcess_Interrupt_WhenRunning(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()

	p.mu.Lock()
	p.State = StateRunning
	p.mu.Unlock()

	p.Interrupt()
	if !p.interruptedRun.Load() {
		t.Error("interruptedRun should be true when interrupted during Running state")
	}
}

func TestProcess_Interrupt_AfterDead(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	go p.readLoop()

	srv.SendCLIExited(0)
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("process did not die")
	}

	p.Interrupt() // should be no-op on dead process
}

// ---------------------------------------------------------------------------
// InterruptViaControl
// ---------------------------------------------------------------------------

// claudeWithFailingInterrupt wraps ClaudeProtocol and forces WriteInterrupt
// to return a transport-like error so we can exercise the rollback path.
type claudeWithFailingInterrupt struct {
	ClaudeProtocol
	err error
}

func (p *claudeWithFailingInterrupt) WriteInterrupt(_ io.Writer, _ string) error {
	return p.err
}

func (p *claudeWithFailingInterrupt) Clone() Protocol {
	return &claudeWithFailingInterrupt{err: p.err}
}

func TestProcess_InterruptViaControl_NoActiveTurn(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()
	defer p.Kill()

	// State == Ready: no turn in flight.
	err := p.InterruptViaControl()
	if !errors.Is(err, ErrNoActiveTurn) {
		t.Fatalf("InterruptViaControl on idle = %v, want ErrNoActiveTurn", err)
	}
	if p.interrupted.Load() || p.interruptedRun.Load() {
		t.Error("idle InterruptViaControl must not set settle flags")
	}
}

func TestProcess_InterruptViaControl_Running_SetsFlagsAndWrites(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()
	defer p.Kill()

	p.mu.Lock()
	p.State = StateRunning
	p.mu.Unlock()

	if err := p.InterruptViaControl(); err != nil {
		t.Fatalf("InterruptViaControl() = %v, want nil", err)
	}
	if !p.interrupted.Load() || !p.interruptedRun.Load() {
		t.Error("successful InterruptViaControl must set interrupted and interruptedRun")
	}
}

// Regression test for P0-1: a failed WriteInterrupt must roll the settle
// flags back so the next Send()'s drainStaleEvents does not burn the 500ms
// settle budget waiting for a result that will never arrive.
func TestProcess_InterruptViaControl_WriteFailure_RollsBackFlags(t *testing.T) {
	wantErr := errors.New("simulated shim write failure")
	p, srv := shimTestPair(&claudeWithFailingInterrupt{err: wantErr})
	startServerDrain(srv)
	p.startReadLoop()
	defer p.Kill()

	p.mu.Lock()
	p.State = StateRunning
	p.mu.Unlock()

	err := p.InterruptViaControl()
	if err == nil {
		t.Fatal("expected write failure to surface as error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrapped %v", err, wantErr)
	}
	if p.interrupted.Load() {
		t.Error("interrupted must be rolled back after write failure")
	}
	if p.interruptedRun.Load() {
		t.Error("interruptedRun must be rolled back after write failure")
	}
}

func TestProcess_InterruptViaControl_DeadProcess(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	go p.readLoop()

	srv.SendCLIExited(0)
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("process did not die")
	}

	err := p.InterruptViaControl()
	if !errors.Is(err, ErrNoActiveTurn) {
		t.Errorf("dead InterruptViaControl = %v, want ErrNoActiveTurn", err)
	}
}

func TestProcess_InterruptViaControl_RequestIDIsPerProcess(t *testing.T) {
	// P1-4 regression: the seq counter must be per-Process, not package-level.
	// Two independent processes both issue their first interrupt and should
	// both land on naozhi-int-1 (not share a global counter).
	p1, srv1 := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv1)
	p1.startReadLoop()
	defer p1.Kill()

	p2, srv2 := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv2)
	p2.startReadLoop()
	defer p2.Kill()

	p1.mu.Lock()
	p1.State = StateRunning
	p1.mu.Unlock()
	p2.mu.Lock()
	p2.State = StateRunning
	p2.mu.Unlock()

	if err := p1.InterruptViaControl(); err != nil {
		t.Fatalf("p1 InterruptViaControl: %v", err)
	}
	if err := p2.InterruptViaControl(); err != nil {
		t.Fatalf("p2 InterruptViaControl: %v", err)
	}
	if got := p1.interruptSeq.Load(); got != 1 {
		t.Errorf("p1.interruptSeq = %d, want 1 (per-process counter)", got)
	}
	if got := p2.interruptSeq.Load(); got != 1 {
		t.Errorf("p2.interruptSeq = %d, want 1 (per-process counter)", got)
	}
}

// ---------------------------------------------------------------------------
// drainStaleEvents
// ---------------------------------------------------------------------------

func TestProcess_DrainStaleEvents_NotInterrupted(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()

	if err := p.drainStaleEvents(context.Background()); err != nil {
		t.Errorf("drainStaleEvents() error = %v", err)
	}
}

func TestProcess_DrainStaleEvents_InterruptedIdle(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()

	p.interrupted.Store(true)
	p.interruptedRun.Store(false)

	if err := p.drainStaleEvents(context.Background()); err != nil {
		t.Errorf("drainStaleEvents() error = %v", err)
	}
	if p.interrupted.Load() {
		t.Error("interrupted should be cleared by drainStaleEvents")
	}
}

func TestProcess_DrainStaleEvents_InterruptedRunning_WithResult(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()

	p.interrupted.Store(true)
	p.interruptedRun.Store(true)

	// Send result event to simulate the interrupted turn completing
	done := make(chan error, 1)
	go func() {
		done <- p.drainStaleEvents(context.Background())
	}()

	time.Sleep(20 * time.Millisecond)
	srv.SendStdout(`{"type":"result","result":"interrupted_result"}`)

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("drainStaleEvents() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drainStaleEvents timed out")
	}
}

// ---------------------------------------------------------------------------
// Close
// ---------------------------------------------------------------------------

func TestProcess_Close_WithExit(t *testing.T) {
	processCloseTimeout = 2 * time.Second
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()

	done := make(chan struct{})
	go func() {
		p.Close()
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	srv.SendCLIExited(0)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close() timed out")
	}
}

func TestProcess_Close_Timeout(t *testing.T) {
	processCloseTimeout = 50 * time.Millisecond
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv) // drain so shimSend(close_stdin) doesn't block
	p.startReadLoop()
	// Close without server side sending cli_exited → timeout → Kill
	done := make(chan struct{})
	go func() {
		p.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close() did not complete after timeout")
	}
}

// ---------------------------------------------------------------------------
// Detach
// ---------------------------------------------------------------------------

func TestProcess_Detach(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()
	p.Detach() // should not panic
}

// ---------------------------------------------------------------------------
// Accessor methods
// ---------------------------------------------------------------------------

func TestProcess_Accessors(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()
	defer p.Kill()

	if s := p.GetState(); s != StateReady {
		t.Errorf("GetState() = %v, want StateReady", s)
	}
	if id := p.GetSessionID(); id != "" {
		t.Errorf("GetSessionID() = %q, want empty", id)
	}
	if c := p.TotalCost(); c != 0.0 {
		t.Errorf("TotalCost() = %f, want 0", c)
	}
	if name := p.ProtocolName(); name != "stream-json" {
		t.Errorf("ProtocolName() = %q, want stream-json", name)
	}
	if pid := p.PID(); pid != 0 {
		t.Errorf("PID() = %d, want 0", pid)
	}
	if tt := p.GetTotalTimeout(); tt != DefaultTotalTimeout {
		t.Errorf("GetTotalTimeout() = %v, want %v", tt, DefaultTotalTimeout)
	}
	if seq := p.LastSeq(); seq != 0 {
		t.Errorf("LastSeq() = %d, want 0", seq)
	}
}

func TestProcess_GetTotalTimeout_Custom(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.totalTimeout = 3 * time.Minute
	p.startReadLoop()
	defer p.Kill()
	if tt := p.GetTotalTimeout(); tt != 3*time.Minute {
		t.Errorf("GetTotalTimeout() = %v, want 3m", tt)
	}
}

// ---------------------------------------------------------------------------
// SetOnTurnDone
// ---------------------------------------------------------------------------

func TestProcess_SetOnTurnDone(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()

	called := make(chan struct{}, 1)
	p.SetOnTurnDone(func() {
		select {
		case called <- struct{}{}:
		default:
		}
	})

	// Set state to Running so result event triggers callback
	p.mu.Lock()
	p.State = StateRunning
	p.mu.Unlock()

	srv.SendStdout(`{"type":"result","result":"done","session_id":"s1"}`)

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("onTurnDone was not called after result event with StateRunning")
	}
}

// ---------------------------------------------------------------------------
// InjectHistory / EventEntries / EventLastN / EventEntriesSince
// ---------------------------------------------------------------------------

func TestProcess_InjectHistory(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	go p.readLoop()
	defer srv.Close()

	entries := []EventEntry{
		{Type: "user", Summary: "msg1", Time: 1000},
		{Type: "result", Summary: "result1", Time: 2000},
	}
	p.InjectHistory(entries)

	all := p.EventEntries()
	if len(all) < 2 {
		t.Errorf("EventEntries() = %d, want >= 2", len(all))
	}

	lastN := p.EventLastN(1)
	if len(lastN) != 1 {
		t.Errorf("EventLastN(1) = %d, want 1", len(lastN))
	}

	since := p.EventEntriesSince(1500)
	if len(since) == 0 {
		t.Errorf("EventEntriesSince(1500) = 0, want >= 1")
	}

	last := p.LastEntryOfType("user")
	if last.Type != "user" {
		t.Errorf("LastEntryOfType(user).Type = %q, want user", last.Type)
	}

	_ = p.LastActivitySummary()
	_ = p.TurnAgents()
}

// ---------------------------------------------------------------------------
// SubscribeEvents
// ---------------------------------------------------------------------------

func TestProcess_SubscribeEvents(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	go p.readLoop()
	defer srv.Close()

	ch, unsub := p.SubscribeEvents()
	defer unsub()

	p.InjectHistory([]EventEntry{{Type: "user", Summary: "hi", Time: 1000}})

	// May or may not notify depending on timing; just verify no panic
	select {
	case <-ch:
	case <-time.After(100 * time.Millisecond):
	}
}

// ---------------------------------------------------------------------------
// findResultSince
// ---------------------------------------------------------------------------

func TestProcess_FindResultSince(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	go p.readLoop()
	defer srv.Close()

	if r := p.findResultSince(0); r != nil {
		t.Errorf("empty log: want nil, got %v", r)
	}

	p.eventLog.Append(EventEntry{Type: "result", Detail: "done", Time: 2000})

	if r := p.findResultSince(1000); r == nil {
		t.Error("should find result entry after 1000ms")
	}
	if r := p.findResultSince(3000); r != nil {
		t.Errorf("should not find entry at 3000ms, got %v", r)
	}
}

// ---------------------------------------------------------------------------
// startReadLoop — sets StateReady
// ---------------------------------------------------------------------------

func TestProcess_StartReadLoop_StateReady(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	if p.State != StateSpawning {
		t.Errorf("initial State = %v, want StateSpawning", p.State)
	}
	p.startReadLoop()
	time.Sleep(10 * time.Millisecond)
	p.mu.Lock()
	s := p.State
	p.mu.Unlock()
	if s != StateReady {
		t.Errorf("State after startReadLoop = %v, want StateReady", s)
	}
	p.Kill()
}

// ---------------------------------------------------------------------------
// readLoop — pong message
// ---------------------------------------------------------------------------

func TestProcess_ReadLoop_Pong(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	go p.readLoop()

	srv.mu.Lock()
	pongMsg, _ := json.Marshal(map[string]string{"type": "pong"})
	srv.writer.Write(pongMsg)      //nolint:errcheck
	srv.writer.Write([]byte{'\n'}) //nolint:errcheck
	srv.writer.Flush()             //nolint:errcheck
	srv.mu.Unlock()

	time.Sleep(50 * time.Millisecond)
	// Verify pongRecv received the signal
	select {
	case <-p.pongRecv:
		// Signal received
	case <-time.After(100 * time.Millisecond):
		// pongRecv is buffered(1) — might already have been consumed
	}
	srv.SendCLIExited(0)
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("readLoop did not exit")
	}
}

// ---------------------------------------------------------------------------
// readLoop — stderr message (logged, not forwarded)
// ---------------------------------------------------------------------------

func TestProcess_ReadLoop_Stderr(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	go p.readLoop()

	srv.mu.Lock()
	msg, _ := json.Marshal(map[string]string{"type": "stderr", "line": "some error"})
	srv.writer.Write(msg)          //nolint:errcheck
	srv.writer.Write([]byte{'\n'}) //nolint:errcheck
	srv.writer.Flush()             //nolint:errcheck
	srv.mu.Unlock()

	srv.SendCLIExited(0)
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("readLoop did not exit")
	}
}

// ---------------------------------------------------------------------------
// readLoop — shim error message
// ---------------------------------------------------------------------------

func TestProcess_ReadLoop_ShimError(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	go p.readLoop()

	srv.mu.Lock()
	msg, _ := json.Marshal(map[string]string{"type": "error", "line": "internal error"})
	srv.writer.Write(msg)          //nolint:errcheck
	srv.writer.Write([]byte{'\n'}) //nolint:errcheck
	srv.writer.Flush()             //nolint:errcheck
	srv.mu.Unlock()

	srv.SendCLIExited(0)
	select {
	case <-p.done:
	case <-time.After(time.Second):
		t.Fatal("readLoop did not exit")
	}
}

// ---------------------------------------------------------------------------
// EventEntryFromEvent — comprehensive
// ---------------------------------------------------------------------------

func TestEventEntryFromEvent(t *testing.T) {
	toolInput, _ := json.Marshal(map[string]string{"file_path": "/a/b/c.go"})
	agentInput, _ := json.Marshal(map[string]interface{}{
		"subagent_type": "Explore",
		"description":   "explore codebase",
	})

	tests := []struct {
		name     string
		event    Event
		wantOK   bool
		wantType string
	}{
		{
			name:     "result",
			event:    Event{Type: "result", Result: "ans", CostUSD: 0.01},
			wantOK:   true,
			wantType: "result",
		},
		{
			name:   "system init skipped",
			event:  Event{Type: "system", SubType: "init"},
			wantOK: false,
		},
		{
			name:     "system task_started",
			event:    Event{Type: "system", SubType: "task_started", Description: "run tests"},
			wantOK:   true,
			wantType: "task_start",
		},
		{
			name:     "system task_progress",
			event:    Event{Type: "system", SubType: "task_progress"},
			wantOK:   true,
			wantType: "task_progress",
		},
		{
			name: "system task_progress with usage",
			event: Event{
				Type: "system", SubType: "task_progress",
				Usage: &TaskUsage{TotalTokens: 100, ToolUses: 5, DurationMS: 1000},
			},
			wantOK:   true,
			wantType: "task_progress",
		},
		{
			name:     "system task_notification",
			event:    Event{Type: "system", SubType: "task_notification", Status: "success"},
			wantOK:   true,
			wantType: "task_done",
		},
		{
			name:   "system stop_hook_summary skipped",
			event:  Event{Type: "system", SubType: "stop_hook_summary"},
			wantOK: false,
		},
		{
			name:   "system turn_duration skipped",
			event:  Event{Type: "system", SubType: "turn_duration"},
			wantOK: false,
		},
		{
			name: "assistant thinking",
			event: Event{Type: "assistant", Message: &AssistantMessage{
				Content: []ContentBlock{{Type: "thinking", Text: "analyzing"}},
			}},
			wantOK:   true,
			wantType: "thinking",
		},
		{
			name: "assistant tool_use",
			event: Event{Type: "assistant", Message: &AssistantMessage{
				Content: []ContentBlock{{Type: "tool_use", Name: "Read", Input: toolInput}},
			}},
			wantOK:   true,
			wantType: "tool_use",
		},
		{
			name: "assistant agent tool_use",
			event: Event{Type: "assistant", Message: &AssistantMessage{
				Content: []ContentBlock{{Type: "tool_use", Name: "Agent", Input: agentInput, ID: "tu-1"}},
			}},
			wantOK:   true,
			wantType: "agent",
		},
		{
			name: "assistant text",
			event: Event{Type: "assistant", Message: &AssistantMessage{
				Content: []ContentBlock{{Type: "text", Text: "hello"}},
			}},
			wantOK:   true,
			wantType: "text",
		},
		{
			name:   "assistant no message skipped",
			event:  Event{Type: "assistant"},
			wantOK: false,
		},
		{
			name:   "unknown type skipped",
			event:  Event{Type: "unknown"},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry, ok := EventEntryFromEvent(tt.event)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
				return
			}
			if tt.wantOK && entry.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", entry.Type, tt.wantType)
			}
		})
	}
}

// TestEventEntriesFromEventAt_UsesExternalTime locks R67-PERF-9: the At
// variant must stamp entries with the caller-supplied millisecond timestamp
// rather than calling time.Now() internally. readLoop relies on this to
// share a single wall-clock read between ev.recvAt and EventEntry.Time.
func TestEventEntriesFromEventAt_UsesExternalTime(t *testing.T) {
	const fixedMS int64 = 1711111111111
	ev := Event{Type: "result", Result: "x", CostUSD: 0.0}
	entries := EventEntriesFromEventAt(ev, fixedMS)
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].Time != fixedMS {
		t.Errorf("EventEntry.Time = %d, want %d", entries[0].Time, fixedMS)
	}
}

// TestEventEntryFromEvent_TodoWriteDetailIsArray locks the on-wire shape of
// entry.Detail for TodoWrite events. The dashboard's renderTodoList calls
// JSON.parse and then Array.isArray on this string; it silently falls back
// to the one-line summary if the parsed value is an object (e.g. the raw
// `{"todos":[...]}` envelope). Historically this bug surfaced as a bubble
// showing "1项·▶1" without the checklist body.
func TestEventEntryFromEvent_TodoWriteDetailIsArray(t *testing.T) {
	raw := json.RawMessage(`{"todos":[{"content":"改 Lane E 到同样口径","status":"in_progress","activeForm":"正在改 Lane E"}]}`)
	ev := Event{Type: "assistant", Message: &AssistantMessage{
		Content: []ContentBlock{{Type: "tool_use", Name: "TodoWrite", Input: raw}},
	}}
	entry, ok := EventEntryFromEvent(ev)
	if !ok || entry.Type != "todo" {
		t.Fatalf("ok=%v type=%q", ok, entry.Type)
	}
	var parsed []TodoItem
	if err := json.Unmarshal([]byte(entry.Detail), &parsed); err != nil {
		t.Fatalf("Detail must be a JSON array of TodoItem, got %q: %v", entry.Detail, err)
	}
	if len(parsed) != 1 || parsed[0].Status != "in_progress" {
		t.Fatalf("unexpected parsed todos: %+v", parsed)
	}
}

// ---------------------------------------------------------------------------
// FormatToolInput
// ---------------------------------------------------------------------------

func TestFormatToolInput(t *testing.T) {
	tests := []struct {
		tool, input, want string
	}{
		{"Read", `{"file_path":"/home/user/proj/main.go"}`, "Read ~/proj/main.go"},
		{"Write", `{"file_path":"/a/b/c.go"}`, "Write /a/b/c.go"},
		{"Edit", `{"file_path":"/a/b/c.go"}`, "Edit /a/b/c.go"},
		{"Glob", `{"pattern":"*.go"}`, "Glob *.go"},
		{"Grep", `{"pattern":"TODO","path":"/src"}`, "Grep TODO in /src"},
		{"Bash", `{"description":"run tests"}`, "Bash run tests"},
		{"Bash", `{"command":"go test ./..."}`, "Bash go test ./..."},
		{"Agent", `{"description":"review changes"}`, "Agent review changes"},
		{"UnknownTool", `{"description":"do it"}`, "UnknownTool do it"},
		// No matching key in known tool → fallback with truncated input
		{"Read", `{}`, "Read: {}"},
		// Empty input
		{"Read", `null`, "Read: null"},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			got := FormatToolInput(tt.tool, json.RawMessage(tt.input))
			if got != tt.want {
				t.Errorf("FormatToolInput(%q, %q) = %q, want %q", tt.tool, tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// shortPath
// ---------------------------------------------------------------------------

func TestShortPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/home/alice/project/file.go", "~/project/file.go"},
		{"/home/alice/file.go", "~/file.go"},
		{"/var/log/app.log", "/var/log/app.log"},
		{strings.Repeat("a", 51), "..." + strings.Repeat("a", 47)},
	}
	for _, tt := range tests {
		if got := shortPath(tt.in); got != tt.want {
			t.Errorf("shortPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// getStr
// ---------------------------------------------------------------------------

func TestGetStr(t *testing.T) {
	m := map[string]json.RawMessage{
		"key": json.RawMessage(`"value"`),
		"num": json.RawMessage(`42`),
	}
	if got := getStr(m, "key"); got != "value" {
		t.Errorf("getStr key = %q, want value", got)
	}
	if got := getStr(m, "missing"); got != "" {
		t.Errorf("getStr missing = %q, want empty", got)
	}
	if got := getStr(m, "num"); got != "" {
		t.Errorf("getStr num = %q, want empty (not a string)", got)
	}
}

// ---------------------------------------------------------------------------
// formatToolDetail
// ---------------------------------------------------------------------------

func TestFormatToolDetail(t *testing.T) {
	block := ContentBlock{
		Type: "tool_use", Name: "Read",
		Input: json.RawMessage(`{"file_path":"/a/b/c.go"}`),
	}
	got := formatToolDetail(block)
	if !strings.Contains(got, "Read") {
		t.Errorf("formatToolDetail = %q, expected Read", got)
	}

	emptyBlock := ContentBlock{Type: "tool_use", Name: "Read"}
	if got2 := formatToolDetail(emptyBlock); got2 != "Read" {
		t.Errorf("formatToolDetail (empty input) = %q, want Read", got2)
	}
}

// Suppressed lint: ensure unused imports compile cleanly.
var _ = bufio.NewReader
var _ = bytes.Buffer{}
var _ io.Writer = nil
var _ = fmt.Sprintf

func TestSanitizeStderrLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"empty", "", ""},
		{"sgr color", "\x1b[31mred\x1b[0m text", "red text"},
		{"cursor move", "abc\x1b[2J\x1b[Hdef", "abcdef"},
		{"osc bel", "\x1b]0;title\x07payload", "payload"},
		{"osc st", "\x1b]8;;https://x\x1b\\link", "link"},
		{"bare ctrl drops", "a\x00b\x07c", "abc"},
		{"preserves tab", "a\tb", "a\tb"},
		{"keeps utf8", "你好世界", "你好世界"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sanitizeStderrLine(tc.in); got != tc.want {
				t.Errorf("sanitizeStderrLine(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeStderrLine_Truncate(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", maxStderrLogLineBytes+200)
	got := sanitizeStderrLine(long)
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("expected truncation suffix, got suffix %q", got[len(got)-20:])
	}
	if !strings.HasPrefix(got, strings.Repeat("x", maxStderrLogLineBytes)) {
		t.Errorf("expected %d x prefix, got len=%d", maxStderrLogLineBytes, len(got))
	}
}
