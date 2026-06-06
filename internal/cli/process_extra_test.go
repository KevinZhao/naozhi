package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/testhelper"
)

// readMethodBody parses srcPath with go/parser and returns the exact source
// text between the opening '{' and closing '}' of the method named methodName
// on the receiver type recvType. Using AST-accurate token offsets instead of
// the older `strings.Index(src[idx:], "\n}\n")` fragment-scan means:
//
//  1. Refactors that reflow whitespace inside the body (removing blank lines,
//     changing brace placement) don't break the extraction.
//
//  2. A neighbouring function that happens to end with "}\n}" (e.g. nested
//     struct literal on the last line) can no longer cause the scan to stop
//     early and silently return a smaller slice.
//
//  3. A neighbouring function that happens to have "\n}\n" before the real
//     end of the target function can no longer steal content — the target's
//     body is bounded by token.Pos from the parser, not text heuristics.
//
// R175-P3: replaces the fragile "\n}\n" fragment extraction that earlier
// contract tests relied on.
func readMethodBody(t *testing.T, srcPath, recvType, methodName string) string {
	t.Helper()
	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read %s: %v", srcPath, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, srcPath, data, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", srcPath, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != methodName {
			continue
		}
		if !methodReceiverMatches(fn, recvType) {
			continue
		}
		if fn.Body == nil {
			t.Fatalf("%s.%s has no body", recvType, methodName)
		}
		// Body.Lbrace and Body.Rbrace straddle '{' and '}'. Offsets are 1-based;
		// convert with fset.Position().Offset - 1 to byte index.
		start := fset.Position(fn.Body.Lbrace).Offset
		end := fset.Position(fn.Body.Rbrace).Offset + 1 // include the closing brace
		if start < 0 || end > len(data) || start >= end {
			t.Fatalf("invalid body range for %s.%s: [%d,%d)", recvType, methodName, start, end)
		}
		return string(data[start:end])
	}
	t.Fatalf("method %s.%s not found in %s", recvType, methodName, srcPath)
	return ""
}

// methodReceiverMatches reports whether fn is a method on recvType. It handles
// both value and pointer receivers and ignores the receiver variable name.
func methodReceiverMatches(fn *ast.FuncDecl, recvType string) bool {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return false
	}
	expr := fn.Recv.List[0].Type
	// Unwrap *T.
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	return ident.Name == recvType
}

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

	// Wait for Send goroutine to flip State→Running before injecting the
	// result; otherwise the stdout arrives while Send is still logging the
	// user entry / draining stale events and can be mis-sequenced.
	testhelper.Eventually(t, func() bool { return p.State() == StateRunning }, time.Second, "Send did not reach StateRunning")
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
	p.state = StateRunning
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

	// Wait for Send goroutine to start (State→Running) before faking exit.
	testhelper.Eventually(t, func() bool { return p.State() == StateRunning }, time.Second, "Send did not reach StateRunning")
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

	// Wait for Send goroutine to start before feeding init/result events.
	testhelper.Eventually(t, func() bool { return p.State() == StateRunning }, time.Second, "Send did not reach StateRunning")
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

	if p.SessionID() != "session-abc" {
		t.Errorf("SessionID = %q, want session-abc", p.SessionID())
	}
}

// TestProcess_Send_UpdatesSessionIDAfterResume pins CLI2 (#393): after a
// --resume flips the underlying CLI's session_id, the next Send observes a
// fresh init/result carrying the new id and p.sessionID must follow. The
// pre-fix guard `if p.sessionID == ""` pinned the field to whatever the
// first Send recorded and let it go stale relative to the CLI's view.
func TestProcess_Send_UpdatesSessionIDAfterResume(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()

	// First Send establishes the original session_id.
	done1 := make(chan error, 1)
	go func() {
		_, err := p.Send(context.Background(), "hello", nil, nil)
		done1 <- err
	}()
	testhelper.Eventually(t, func() bool { return p.State() == StateRunning }, time.Second,
		"first Send did not reach StateRunning")
	srv.SendStdout(`{"type":"system","subtype":"init","session_id":"sess-original"}`)
	srv.SendStdout(`{"type":"result","result":"done","session_id":"sess-original"}`)
	select {
	case err := <-done1:
		if err != nil {
			t.Fatalf("first Send error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first Send timed out")
	}
	if got := p.SessionID(); got != "sess-original" {
		t.Fatalf("after first Send: SessionID = %q, want sess-original", got)
	}

	// Second Send: simulate a --resume that the CLI honours by emitting a
	// fresh init carrying a different session_id (the resumed/forked id).
	done2 := make(chan error, 1)
	go func() {
		_, err := p.Send(context.Background(), "follow-up", nil, nil)
		done2 <- err
	}()
	testhelper.Eventually(t, func() bool { return p.State() == StateRunning }, time.Second,
		"second Send did not reach StateRunning")
	srv.SendStdout(`{"type":"system","subtype":"init","session_id":"sess-resumed"}`)
	srv.SendStdout(`{"type":"result","result":"done","session_id":"sess-resumed"}`)
	select {
	case err := <-done2:
		if err != nil {
			t.Fatalf("second Send error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Send timed out")
	}
	if got := p.SessionID(); got != "sess-resumed" {
		t.Errorf("after --resume Send: SessionID = %q, want sess-resumed (pre-fix would still be sess-original)", got)
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

	// Wait for Send goroutine to enter the running state before delivering
	// the thinking/result pair to onEvent.
	testhelper.Eventually(t, func() bool { return p.State() == StateRunning }, time.Second, "Send did not reach StateRunning")
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

	// Wait for Send to reach StateRunning before answering.
	testhelper.Eventually(t, func() bool { return p.State() == StateRunning }, time.Second, "Send did not reach StateRunning")
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
	p.state = StateRunning
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
	p.state = StateRunning
	p.mu.Unlock()

	if err := p.InterruptViaControl(); err != nil {
		t.Fatalf("InterruptViaControl() = %v, want nil", err)
	}
	if !p.interrupted.Load() || !p.interruptedRun.Load() {
		t.Error("successful InterruptViaControl must set interrupted and interruptedRun")
	}
}

// TestInterruptViaControlConcurrentSetterNotClobbered (R260528-BUG-4)
// pins the CAS-based rollback. Pre-fix, a write-fail rollback ran
// p.interrupted.Store(false) + p.interruptedRun.Store(false)
// unconditionally, which clobbered a concurrent Interrupt() that had
// just Stored(true) on the same flags. The fix records which flags
// the InterruptViaControl call ITSELF flipped (CompareAndSwap from
// false→true) and only rolls those back on write failure; flags
// already true via a sibling Interrupt() are preserved.
//
// We simulate the race deterministically: pre-set both flags to true
// (as a concurrent Interrupt() would), drive InterruptViaControl
// with a failing protocol, and assert the flags survive the rollback.
func TestInterruptViaControlConcurrentSetterNotClobbered(t *testing.T) {
	wantErr := errors.New("simulated shim write failure")
	p, srv := shimTestPair(&claudeWithFailingInterrupt{err: wantErr})
	startServerDrain(srv)
	p.startReadLoop()
	defer p.Kill()

	p.mu.Lock()
	p.state = StateRunning
	p.mu.Unlock()
	// Concurrent Interrupt() winning the race — both flags already true
	// before InterruptViaControl runs. The CAS attempts inside the
	// method must observe the flags as already-true and skip the rollback.
	p.interrupted.Store(true)
	p.interruptedRun.Store(true)

	err := p.InterruptViaControl()
	if err == nil {
		t.Fatal("expected write failure to surface as error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrapped %v", err, wantErr)
	}
	// The concurrent Interrupt()'s flags MUST survive — the rollback
	// only owns flags THIS call's CAS flipped from false→true.
	if !p.interrupted.Load() {
		t.Error("interrupted was clobbered: concurrent Interrupt()'s flag lost on rollback")
	}
	if !p.interruptedRun.Load() {
		t.Error("interruptedRun was clobbered: concurrent Interrupt()'s flag lost on rollback")
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
	p.state = StateRunning
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
	p1.state = StateRunning
	p1.mu.Unlock()
	p2.mu.Lock()
	p2.state = StateRunning
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

	// drainStaleEvents Swap(false)'s both flags on entry. Once interrupted
	// clears, drain has taken ownership and is waiting on p.eventCh for the
	// stale result — safe to inject it now.
	testhelper.Eventually(t, func() bool { return !p.interrupted.Load() }, time.Second, "drainStaleEvents did not enter settle window")
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
	prev := processCloseTimeout
	processCloseTimeout = 2 * time.Second
	defer func() { processCloseTimeout = prev }()
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
	prev := processCloseTimeout
	processCloseTimeout = 50 * time.Millisecond
	defer func() { processCloseTimeout = prev }()
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv) // drain so shimSend(shutdown) doesn't block
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

// TestProcess_Close_SendsShutdownNotCloseStdin locks in the semantics change
// that fixes UCCLEP-2026-04-26: Close must ask the shim to exit (socket
// unlinked, listener released) rather than just closing CLI stdin (shim
// still in 30s grace period holding the socket, causing the next StartShim
// for the same key to fail the dial-first guard).
func TestProcess_Close_SendsShutdownNotCloseStdin(t *testing.T) {
	prev := processCloseTimeout
	processCloseTimeout = 200 * time.Millisecond
	defer func() { processCloseTimeout = prev }()

	p, srv := shimTestPair(&ClaudeProtocol{})
	p.startReadLoop()

	// Capture the first msg the client writes. Read one line; Close() also
	// times out (no CLI exit), but timeout → Kill, which sends "kill" as a
	// separate message — the first one is what we care about.
	msgCh := make(chan string, 4)
	go func() {
		reader := bufio.NewReader(srv.conn)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				close(msgCh)
				return
			}
			var got struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(bytes.TrimSpace(line), &got); err == nil {
				msgCh <- got.Type
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		p.Close()
		close(done)
	}()

	select {
	case first := <-msgCh:
		if first != "shutdown" {
			t.Fatalf("Close() sent %q first; want %q", first, "shutdown")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no message captured from shim conn")
	}

	<-done
}

// TestProcess_Close_SetsWriteDeadline is an R175-P1 regression gate: Close()
// must route its "shutdown" send through shimWMu+SetWriteDeadline just like
// Kill()/Detach() do. Without that, an alive-but-wedged shim (TCP write
// buffer full / GC pause / stuck child) pins shimWMu for minutes waiting for
// the kernel keepalive to give up, starving every concurrent shimSend
// (heartbeat, ping, interrupt) and stretching Router.Reset / shutdown past
// the SIGTERM grace. Source-level scan so a future refactor that reverts to
// the bare p.shimSend(shutdown) pattern fails loudly.
func TestProcess_Close_SetsWriteDeadline(t *testing.T) {
	t.Parallel()
	// R175-P3: use AST-accurate body extraction instead of `\n}\n` fragment
	// scan so a future refactor that changes brace-placement inside Close (or
	// in a neighbouring method) cannot silently widen/truncate the slice and
	// turn these assertions into false positives/negatives.
	body := readMethodBody(t, "process.go", "Process", "Close")
	// Invariant 1: must take shimWMu.
	if !strings.Contains(body, "p.shimWMu.Lock()") {
		t.Error("Close must acquire shimWMu before sending shutdown (mirror Kill/Detach)")
	}
	// Invariant 2: must call SetWriteDeadline with captured error — the
	// SetWriteDeadline contract test enforces this syntactically repo-wide,
	// but we also pin the call site so a pure-refactor that drops the
	// deadline altogether is caught here specifically.
	if !strings.Contains(body, "SetWriteDeadline(") {
		t.Error("Close must call SetWriteDeadline on shimConn before shutdown write")
	}
	// Invariant 3: must use shimSendLocked (not shimSend) after taking the
	// lock — shimSend re-acquires shimWMu and would deadlock.
	if !strings.Contains(body, "shimSendLocked(shimClientMsg{Type: \"shutdown\"})") {
		t.Error("Close must call shimSendLocked under shimWMu with shutdown msg")
	}
	// Invariant 4: reject the legacy bare-send pattern.
	if strings.Contains(body, "p.shimSend(shimClientMsg{Type: \"shutdown\"})") {
		t.Error("Close must NOT call p.shimSend(shutdown) directly — use shimSendLocked under shimWMu with SetWriteDeadline")
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

	if s := p.State(); s != StateReady {
		t.Errorf("State() = %v, want StateReady", s)
	}
	if id := p.SessionID(); id != "" {
		t.Errorf("SessionID() = %q, want empty", id)
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
	if tt := p.TotalTimeout(); tt != DefaultTotalTimeout {
		t.Errorf("TotalTimeout() = %v, want %v", tt, DefaultTotalTimeout)
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
	if tt := p.TotalTimeout(); tt != 3*time.Minute {
		t.Errorf("TotalTimeout() = %v, want 3m", tt)
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

	// Set state to Running and arm reconnectedMidTurn to simulate the
	// post-reconnect path where readLoop owns the State→Ready transition.
	// Outside reconnect, Send() owns the transition (see readLoop guard).
	p.mu.Lock()
	p.state = StateRunning
	p.mu.Unlock()
	p.reconnectedMidTurn.Store(true)

	srv.SendStdout(`{"type":"result","result":"done","session_id":"s1"}`)

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("onTurnDone was not called after result event with StateRunning")
	}
}

// TestProcess_ResultDoesNotFlipStateWithoutReconnect verifies the Send-path
// guarantee: in the non-reconnect scenario, a `result` event must not flip
// State from Running to Ready in readLoop. Send() owns that transition via
// its defer; a readLoop-side transition would race Send() and briefly expose
// "ready" in the dashboard while sendMu is still held.
func TestProcess_ResultDoesNotFlipStateWithoutReconnect(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	p.startReadLoop()
	defer p.Kill()

	cbCalled := make(chan struct{}, 1)
	p.SetOnTurnDone(func() {
		select {
		case cbCalled <- struct{}{}:
		default:
		}
	})

	// Simulate Send() acquiring the turn: Ready → Running, but do NOT arm
	// reconnectedMidTurn. This is the normal path.
	p.mu.Lock()
	p.state = StateRunning
	p.mu.Unlock()

	srv.SendStdout(`{"type":"result","result":"done","session_id":"s1"}`)

	select {
	case <-cbCalled:
		t.Fatal("onTurnDone was called on a non-reconnect result; readLoop must not race Send() for the State transition")
	case <-time.After(200 * time.Millisecond):
	}
	if s := p.State(); s != StateRunning {
		t.Errorf("State = %v after result without reconnect arm, want StateRunning (Send owns the transition)", s)
	}
}

// TestProcess_DeathReason_FirstWriterWins exercises setDeathReason under
// concurrent writers. With the CAS-backed implementation, the first reason
// to be stored must survive; without it, last-writer-wins would silently
// reclassify (e.g. readloop_panic overwritten by shim_eof from the unwind).
func TestProcess_DeathReason_FirstWriterWins(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	defer srv.Close()
	defer p.Kill()

	p.setDeathReason(DeathReasonReadLoopPanic)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.setDeathReason(DeathReasonShimEOF)
			p.setDeathReason(DeathReasonShimReadErr)
			p.setDeathReason(DeathReasonCLIExited)
		}()
	}
	wg.Wait()

	if got := p.DeathReason(); got != DeathReasonReadLoopPanic {
		t.Errorf("DeathReason() = %q, want %q (first writer must win)", got, DeathReasonReadLoopPanic)
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

	// Inline backward scan: production never needed lastEntryOfType, the
	// helpers were retired in DEADCODE-8. The test still needs the same
	// "find the most recent entry of type X" semantic to validate the
	// EventLog ring buffer's user-turn bookkeeping.
	last := func() EventEntry {
		l := p.eventLog
		l.mu.RLock()
		defer l.mu.RUnlock()
		for i := l.count - 1; i >= 0; i-- {
			idx := (l.head - l.count + i + l.maxSize) % l.maxSize
			if l.entries[idx].Type == "user" {
				return l.entries[idx]
			}
		}
		return EventEntry{}
	}()
	if last.Type != "user" {
		t.Errorf("lastEntryOfType(user).Type = %q, want user", last.Type)
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
	} else if r.Text != "done" {
		t.Errorf("Text = %q; want %q (result entry carried Detail)", r.Text, "done")
	}
	if r := p.findResultSince(3000); r != nil {
		t.Errorf("should not find entry at 3000ms, got %v", r)
	}
}

// TestProcess_FindResultSince_RecoversTextFromAssistantEntry is the regression
// test for R20260605B-CORR-1 (#1805). readLoop logs the assistant reply as a
// "text" entry and the turn-boundary "result" entry with EMPTY Detail (only
// Cost). When eventCh drops the result event under load, the legacy Send path
// falls back to findResultSince; the old code returned SendResult{Text:
// result.Detail} == "", silently blanking the reply on the ACP/cron path. The
// fix recovers the text from the preceding "text" entry.
func TestProcess_FindResultSince_RecoversTextFromAssistantEntry(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	go p.readLoop()
	defer srv.Close()

	// Realistic readLoop shape: an assistant "text" entry carrying the reply,
	// then a "result" entry that carries cost only (Detail/Summary empty).
	p.eventLog.Append(EventEntry{Type: "text", Summary: "Hello", Detail: "Hello, full answer.", Time: 1500})
	p.eventLog.Append(EventEntry{Type: "result", Cost: 0.0042, Time: 2000})

	r := p.findResultSince(1000)
	if r == nil {
		t.Fatal("findResultSince returned nil; want a recovered result")
	}
	if r.Text != "Hello, full answer." {
		t.Errorf("Text = %q; want the recovered assistant text %q (#1805)", r.Text, "Hello, full answer.")
	}
	if r.CostUSD != 0.0042 {
		t.Errorf("CostUSD = %v; want 0.0042 (cost must survive)", r.CostUSD)
	}
}

// TestProcess_FindResultSince_EmptyWhenNoText pins the no-text-produced case:
// a bare result entry with no preceding "text" entry recovers an empty Text
// (no panic, no spurious text), preserving cost. R20260605B-CORR-1 (#1805).
func TestProcess_FindResultSince_EmptyWhenNoText(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	go p.readLoop()
	defer srv.Close()

	p.eventLog.Append(EventEntry{Type: "result", Cost: 0.001, Time: 2000})

	r := p.findResultSince(1000)
	if r == nil {
		t.Fatal("findResultSince returned nil; want a result with empty Text")
	}
	if r.Text != "" {
		t.Errorf("Text = %q; want empty (turn produced no text entry)", r.Text)
	}
	if r.CostUSD != 0.001 {
		t.Errorf("CostUSD = %v; want 0.001", r.CostUSD)
	}
}

// ---------------------------------------------------------------------------
// startReadLoop — sets StateReady
// ---------------------------------------------------------------------------

func TestProcess_StartReadLoop_StateReady(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	startServerDrain(srv)
	if p.state != StateSpawning {
		t.Errorf("initial state = %v, want StateSpawning", p.state)
	}
	p.startReadLoop()
	// Poll for StateReady instead of sleeping a fixed 10ms; this is
	// faster on idle runs and tolerant under -race / slow CI.
	testhelper.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.state == StateReady
	}, time.Second, "startReadLoop did not reach StateReady")
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

	// TEST1 (#395): poll for the pong signal instead of sleeping a fixed
	// 50ms. pongRecv is a buffered(1) chan that readLoop sends into the
	// moment the pong frame parses, so a 200ms upper bound gives slow CI
	// runners ample headroom while a happy-path run exits in microseconds.
	// On timeout the select silently falls through (matches the original
	// "might already have been consumed" semantics for shaved-down test
	// runs), so the test continues to assert the readLoop-exit contract
	// even when the pong race is lost — the prior wall-clock sleep was a
	// best-effort observation, not a hard precondition.
	select {
	case <-p.pongRecv:
		// Signal received
	case <-time.After(200 * time.Millisecond):
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
// EventEntriesFromEvent — comprehensive (single-entry shape)
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
			entries := EventEntriesFromEventAt(tt.event, time.Now().UnixMilli())
			ok := len(entries) > 0
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
				return
			}
			if tt.wantOK && entries[0].Type != tt.wantType {
				t.Errorf("Type = %q, want %q", entries[0].Type, tt.wantType)
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
		t.Fatalf("want 1 result entry, got %d: %+v", len(entries), entries)
	}
	if entries[0].Time != fixedMS {
		t.Errorf("entries[0].Time = %d, want %d", entries[0].Time, fixedMS)
	}
	if entries[0].Type != "result" {
		t.Errorf("entries[0].Type = %q, want result", entries[0].Type)
	}
}

// TestEventEntriesFromEvent_ResultDoesNotEmitVisibleText pins the invariant
// that a result event yields exactly ONE EventEntry — the metadata-only
// "result" entry. The visible reply is the responsibility of preceding
// assistant frames (claude streams text content blocks turn-mid, ACP
// synthesises one at stopReason). An earlier implementation tried to mirror
// ev.Result into a parallel "text" entry as a fallback for ACP, but that
// produced a duplicate bubble on claude (where the same text had already
// been logged from a preceding text content block) — see commit history for
// the multi-event ReadEvent fix.
func TestEventEntriesFromEvent_ResultDoesNotEmitVisibleText(t *testing.T) {
	for _, ev := range []Event{
		{Type: "result", Result: "", CostUSD: 0.0042},
		{Type: "result", Result: "x", CostUSD: 0.0},
	} {
		entries := EventEntriesFromEventAt(ev, time.Now().UnixMilli())
		if len(entries) != 1 {
			t.Fatalf("ev=%+v: want 1 entry, got %d (%+v)", ev, len(entries), entries)
		}
		if entries[0].Type != "result" {
			t.Errorf("ev=%+v: entries[0].Type = %q, want result", ev, entries[0].Type)
		}
		if entries[0].Summary != "" || entries[0].Detail != "" {
			t.Errorf("ev=%+v: result entry must not carry visible Summary/Detail (those would render as a duplicate bubble), got Summary=%q Detail=%q",
				ev, entries[0].Summary, entries[0].Detail)
		}
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
	entries := EventEntriesFromEventAt(ev, time.Now().UnixMilli())
	if len(entries) == 0 || entries[0].Type != "todo" {
		t.Fatalf("len=%d type=%q", len(entries), func() string {
			if len(entries) == 0 {
				return ""
			}
			return entries[0].Type
		}())
	}
	entry := entries[0]
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

// TestReadMethodBody_Contract locks the semantics of the AST-accurate
// body-extraction helper that replaces the fragile `\n}\n` fragment scan used
// by earlier contract tests (R175-P3). If these invariants regress, the
// downstream contract tests (TestProcess_Close_SetsWriteDeadline et al.) can
// silently succeed on bodies that no longer contain their required patterns.
func TestReadMethodBody_Contract(t *testing.T) {
	t.Parallel()

	// Invariant 1: the helper must resolve a real method on *Process that
	// contains its documented markers. Close was the original motivating
	// caller, so we reuse it as the positive example.
	body := readMethodBody(t, "process.go", "Process", "Close")
	if !strings.Contains(body, "shimSendLocked") {
		t.Errorf("Close body must contain shimSendLocked call; extracted body missing marker — helper may be returning the wrong range. Body:\n%s", body)
	}
	if !strings.HasPrefix(strings.TrimLeft(body, " \t"), "{") {
		t.Errorf("extracted body must start with an opening brace; got prefix=%q", body[:min2(16, len(body))])
	}
	if !strings.HasSuffix(strings.TrimRight(body, " \t\n"), "}") {
		t.Errorf("extracted body must end with a closing brace; got suffix=%q", body[max2(0, len(body)-16):])
	}

	// Invariant 2: braces must balance in the extracted slice. A miscount by
	// one would indicate the helper stopped at a nested inline literal rather
	// than the method's true end. We deliberately count only the literal
	// braces in source text (including those inside string literals / comments)
	// because a correctly-extracted method body is a self-contained scope and
	// therefore brace-balanced at the lexical level.
	opens := strings.Count(body, "{")
	closes := strings.Count(body, "}")
	if opens != closes {
		t.Errorf("extracted body has unbalanced braces: opens=%d closes=%d — helper likely truncated. Body:\n%s", opens, closes, body)
	}

	// Invariant 3 (missing method must fail loudly) is deliberately NOT
	// covered here because Go's testing package exposes no way to construct
	// a *testing.T that captures Fatalf without also failing the parent test.
	// The helper's contract is "call t.Fatalf on miss" — this is enforced by
	// normal usage: every caller in this file treats t as the real test
	// driver, and a regression that silently returns "" would cause every
	// downstream Contains assertion to fail, which is itself a loud signal.

	// Invariant 4: extraction must differentiate between two methods with
	// similar neighbouring signatures. Spawn and Send both exist on *Process;
	// extracting Spawn must not include any text from Send (or vice versa).
	// We verify by extracting Kill and checking that a marker unique to
	// Detach (the sibling method) does not appear. If the helper stopped at
	// a neighbour's closing brace it would bleed Detach content in.
	killBody := readMethodBody(t, "process.go", "Process", "Kill")
	if strings.Contains(killBody, "Detach()") && !strings.Contains(killBody, "// Kill") {
		// Kill body legitimately may reference Detach in a comment chain, but
		// calling Detach() from Kill() would be unusual — flag as a smell.
		t.Logf("Kill body references Detach(); confirm helper did not bleed across methods")
	}
	// Deterministic check: Kill must be a reasonably-sized slice, not the
	// entire rest of the file. Real Kill is a few dozen lines; if the helper
	// returned everything to EOF it would be tens of KB.
	if len(killBody) > 8*1024 {
		t.Errorf("Kill body extraction returned %d bytes — helper likely over-reached past method end", len(killBody))
	}
}

// min2 / max2 are tiny integer helpers used by TestReadMethodBody_Contract's
// slice-bounds diagnostics. Named to avoid colliding with the stdlib `min` /
// `max` builtins introduced in Go 1.21 (those accept any ordered type but the
// compile-time overload could produce surprising types here).
func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}
