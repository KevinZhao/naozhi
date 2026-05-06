package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// passthroughShim extends shimTestServer with a recv goroutine that parses
// client-sent shim frames ("write" frames carrying NDJSON user messages) and
// routes them into a channel. Tests can listen for these writes and react
// (emit replay events, emit result, etc).
type passthroughShim struct {
	srv      *shimTestServer
	proc     *Process
	writeCh  chan shimClientMsg
	readerWG sync.WaitGroup
}

func newPassthroughShim(t *testing.T) *passthroughShim {
	t.Helper()
	p, srv := shimTestPair(&ClaudeProtocol{})
	s := &passthroughShim{
		srv:     srv,
		proc:    p,
		writeCh: make(chan shimClientMsg, 64),
	}
	s.readerWG.Add(1)
	go s.recvLoop()
	return s
}

// recvLoop reads frames the Process writes to the shim (stdin writes,
// ping/interrupt/etc) and pushes parsed frames to writeCh.
func (s *passthroughShim) recvLoop() {
	defer s.readerWG.Done()
	reader := bufio.NewReader(s.srv.conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var msg shimClientMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		select {
		case s.writeCh <- msg:
		default:
			// Drop if channel full — keeps recvLoop non-blocking
		}
	}
}

// expectWrite waits for the next "write" frame from the process and returns
// the parsed InputMessage. Fails if no write arrives within timeout.
func (s *passthroughShim) expectWrite(t *testing.T, timeout time.Duration) InputMessage {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case msg := <-s.writeCh:
			if msg.Type != "write" {
				// Skip non-write frames (ping, interrupt, shutdown, etc).
				continue
			}
			var input InputMessage
			if err := json.Unmarshal([]byte(msg.Line), &input); err != nil {
				t.Fatalf("expectWrite: failed to unmarshal user message line: %v", err)
			}
			return input
		case <-deadline:
			t.Fatalf("expectWrite: timeout after %v waiting for stdin write", timeout)
		}
	}
}

// emitReplay sends an isReplay:true user event back to the process.
func (s *passthroughShim) emitReplay(uuid string, texts ...string) {
	content := make([]map[string]any, 0, len(texts))
	for _, t := range texts {
		content = append(content, map[string]any{"type": "text", "text": t})
	}
	ev := map[string]any{
		"type":     "user",
		"uuid":     uuid,
		"isReplay": true,
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	}
	data, _ := json.Marshal(ev)
	s.srv.SendStdout(string(data))
}

// emitInit sends a system/init event (starts a new turn).
func (s *passthroughShim) emitInit(sessionID string) {
	ev := fmt.Sprintf(`{"type":"system","subtype":"init","session_id":"%s"}`, sessionID)
	s.srv.SendStdout(ev)
}

// emitResult sends a result event (turn complete) with the given text.
func (s *passthroughShim) emitResult(sessionID, text string) {
	ev := map[string]any{
		"type":           "result",
		"subtype":        "success",
		"session_id":     sessionID,
		"result":         text,
		"total_cost_usd": 0.001,
	}
	data, _ := json.Marshal(ev)
	s.srv.SendStdout(string(data))
}

func (s *passthroughShim) close() {
	s.srv.Close()
	s.readerWG.Wait()
}

// --- Tests ---

// TestPassthrough_Independent_OneMessageOneResult verifies the simplest case:
// a single SendPassthrough writes stdin, receives a replay, gets the matching
// result, and returns to caller.
func TestPassthrough_Independent_OneMessageOneResult(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	resultCh := make(chan *SendResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := sh.proc.SendPassthrough(context.Background(), "hello", nil, nil, "")
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- res
	}()

	// Wait for stdin write
	input := sh.expectWrite(t, 2*time.Second)
	if input.Type != "user" {
		t.Fatalf("stdin input.Type = %q, want user", input.Type)
	}
	if input.UUID == "" {
		t.Fatal("stdin input.UUID empty; expected passthrough to generate one")
	}
	if text, ok := input.Message.Content.(string); !ok || text != "hello" {
		t.Fatalf("stdin input.Message.Content = %v, want %q", input.Message.Content, "hello")
	}

	// Emit init → replay → result
	sh.emitInit("s1")
	sh.emitReplay(input.UUID, "hello")
	sh.emitResult("s1", "hi back")

	select {
	case res := <-resultCh:
		if res.Text != "hi back" {
			t.Errorf("result.Text = %q, want 'hi back'", res.Text)
		}
		if res.MergedCount != 1 {
			t.Errorf("result.MergedCount = %d, want 1", res.MergedCount)
		}
	case err := <-errCh:
		t.Fatalf("SendPassthrough returned error: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("SendPassthrough did not return within 3s")
	}
}

// TestPassthrough_Merged_FanoutHeadFollower verifies that two concurrent sends
// whose replays come back as a single merged replay event get result fan-out
// with head/follower semantics.
func TestPassthrough_Merged_FanoutHeadFollower(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	type sendOut struct {
		res *SendResult
		err error
	}
	outA := make(chan sendOut, 1)
	outB := make(chan sendOut, 1)

	go func() {
		res, err := sh.proc.SendPassthrough(context.Background(), "msg A", nil, nil, "")
		outA <- sendOut{res, err}
	}()
	inputA := sh.expectWrite(t, 2*time.Second)

	go func() {
		res, err := sh.proc.SendPassthrough(context.Background(), "msg B", nil, nil, "")
		outB <- sendOut{res, err}
	}()
	inputB := sh.expectWrite(t, 2*time.Second)

	// Simulate the CLI merging both messages into one turn. Live behaviour:
	// a single merged-replay event with a CLI-generated uuid and content
	// that doesn't match any naozhi uuid. The current Process sweeps every
	// unclaimed pending slot on such an event. Emit a single merged replay
	// (single text field concatenating the messages) and one result.
	sh.emitInit("s1")
	_ = inputA
	_ = inputB
	sh.emitReplay("cli-merged-uuid", "msg A msg B")
	sh.emitResult("s1", "merged reply text")

	var a, b sendOut
	select {
	case a = <-outA:
	case <-time.After(3 * time.Second):
		t.Fatal("slot A did not return")
	}
	select {
	case b = <-outB:
	case <-time.After(3 * time.Second):
		t.Fatal("slot B did not return")
	}
	if a.err != nil || b.err != nil {
		t.Fatalf("errors: a=%v b=%v", a.err, b.err)
	}
	if a.res.MergedCount != 2 || b.res.MergedCount != 2 {
		t.Errorf("MergedCount: a=%d b=%d, want 2 for both", a.res.MergedCount, b.res.MergedCount)
	}
	// Head (A — first to arrive) should have full text, follower B should
	// have empty Text and point MergedWithHead at A's slot id.
	if a.res.Text != "merged reply text" {
		t.Errorf("head A text = %q, want full", a.res.Text)
	}
	if a.res.MergedWithHead != 0 {
		t.Errorf("head A MergedWithHead = %d, want 0", a.res.MergedWithHead)
	}
	if b.res.Text != "" {
		t.Errorf("follower B text = %q, want empty", b.res.Text)
	}
	if b.res.MergedWithHead == 0 {
		t.Errorf("follower B MergedWithHead = 0, want head's slot id")
	}
	if b.res.HeadText != "merged reply text" {
		t.Errorf("follower B HeadText = %q, want head's full text", b.res.HeadText)
	}
}

// TestPassthrough_CtxCancel_TombstoneDoesNotBreakFIFO verifies that a canceled
// Send leaves its slot in place so the result of a later slot still lands on
// the correct caller.
func TestPassthrough_CtxCancel_TombstoneDoesNotBreakFIFO(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	// Slot A — will be canceled
	ctxA, cancelA := context.WithCancel(context.Background())
	type sendOut struct {
		res *SendResult
		err error
	}
	outA := make(chan sendOut, 1)
	go func() {
		res, err := sh.proc.SendPassthrough(ctxA, "msg A", nil, nil, "")
		outA <- sendOut{res, err}
	}()
	inputA := sh.expectWrite(t, 2*time.Second)

	// Slot B — stays alive, should still get its own result correctly
	outB := make(chan sendOut, 1)
	go func() {
		res, err := sh.proc.SendPassthrough(context.Background(), "msg B", nil, nil, "")
		outB <- sendOut{res, err}
	}()
	inputB := sh.expectWrite(t, 2*time.Second)

	// Cancel A before any replay/result arrives. The slot stays in
	// pendingSlots as a tombstone.
	cancelA()
	aResult := <-outA
	if !errors.Is(aResult.err, context.Canceled) {
		t.Fatalf("slot A err = %v, want context.Canceled", aResult.err)
	}

	// Now emit replay for A and replay for B as two independent events.
	// Both should try to match by uuid; A's replay finds a canceled slot
	// (its result is dropped). B's replay + result delivers to B.
	sh.emitInit("s1")
	sh.emitReplay(inputA.UUID, "msg A")
	sh.emitResult("s1", "reply to A")

	// Need a short moment for the canceled slot's result to be dropped
	// and removed from pendingSlots before B's turn arrives.
	time.Sleep(100 * time.Millisecond)

	sh.emitInit("s1")
	sh.emitReplay(inputB.UUID, "msg B")
	sh.emitResult("s1", "reply to B")

	select {
	case b := <-outB:
		if b.err != nil {
			t.Fatalf("slot B err = %v", b.err)
		}
		if b.res.Text != "reply to B" {
			t.Errorf("slot B got text = %q, want 'reply to B' (FIFO corrupted!)", b.res.Text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("slot B did not return")
	}
}

// TestPassthrough_CLIDeath_FansOutErrProcessExited verifies that when the
// CLI exits, all pending slots get ErrProcessExited.
func TestPassthrough_CLIDeath_FansOutErrProcessExited(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	type sendOut struct {
		res *SendResult
		err error
	}
	outA := make(chan sendOut, 1)
	outB := make(chan sendOut, 1)
	go func() {
		res, err := sh.proc.SendPassthrough(context.Background(), "msg A", nil, nil, "")
		outA <- sendOut{res, err}
	}()
	_ = sh.expectWrite(t, 2*time.Second)
	go func() {
		res, err := sh.proc.SendPassthrough(context.Background(), "msg B", nil, nil, "")
		outB <- sendOut{res, err}
	}()
	_ = sh.expectWrite(t, 2*time.Second)

	// CLI exits while both slots are waiting
	sh.srv.SendCLIExited(0)

	for i, ch := range []chan sendOut{outA, outB} {
		select {
		case o := <-ch:
			if !errors.Is(o.err, ErrProcessExited) {
				t.Errorf("slot %d err = %v, want ErrProcessExited", i, o.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("slot %d did not return after CLI exit", i)
		}
	}
}

// TestPassthrough_Discard_FiresErrSessionReset verifies /new semantics.
func TestPassthrough_Discard_FiresErrSessionReset(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	type sendOut struct {
		res *SendResult
		err error
	}
	out := make(chan sendOut, 1)
	go func() {
		res, err := sh.proc.SendPassthrough(context.Background(), "msg", nil, nil, "")
		out <- sendOut{res, err}
	}()
	_ = sh.expectWrite(t, 2*time.Second)

	sh.proc.DiscardPassthroughPending(ErrSessionReset)

	select {
	case o := <-out:
		if !errors.Is(o.err, ErrSessionReset) {
			t.Errorf("err = %v, want ErrSessionReset", o.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("slot did not return after Discard")
	}
}

// TestPassthrough_MaxPending_RejectsWhenFull verifies the back-pressure path.
func TestPassthrough_MaxPending_RejectsWhenFull(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	// Fill exactly to the limit. All these sends block waiting for result.
	var wg sync.WaitGroup
	for i := 0; i < maxPendingSlots; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = sh.proc.SendPassthrough(context.Background(), "filler", nil, nil, "")
		}()
		_ = sh.expectWrite(t, 2*time.Second)
	}

	// One more — should fail immediately with ErrTooManyPending.
	_, err := sh.proc.SendPassthrough(context.Background(), "overflow", nil, nil, "")
	if !errors.Is(err, ErrTooManyPending) {
		t.Errorf("overflow err = %v, want ErrTooManyPending", err)
	}

	// Tear down: CLI death unblocks the filler sends.
	sh.srv.SendCLIExited(0)
	wg.Wait()
}

// TestPassthrough_PriorityNowForwarded verifies that priority="now" is
// serialized into the stdin payload as the top-level priority field.
func TestPassthrough_PriorityNowForwarded(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	go func() {
		_, _ = sh.proc.SendPassthrough(context.Background(), "stop!", nil, nil, "now")
	}()
	input := sh.expectWrite(t, 2*time.Second)
	if input.Priority != "now" {
		t.Errorf("stdin priority = %q, want 'now'", input.Priority)
	}

	// Clean up so the blocked SendPassthrough returns
	sh.srv.SendCLIExited(0)
}

// TestPassthrough_ReplayEventNotLoggedAsUserTurn verifies that replay events
// do NOT show up as entries in EventLog (they are ack echoes, not new user
// messages). Otherwise the dashboard would display each message twice.
func TestPassthrough_ReplayEventNotLoggedAsUserTurn(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	// Send and complete one turn.
	out := make(chan error, 1)
	go func() {
		_, err := sh.proc.SendPassthrough(context.Background(), "hi", nil, nil, "")
		out <- err
	}()
	input := sh.expectWrite(t, 2*time.Second)
	sh.emitInit("s1")
	sh.emitReplay(input.UUID, "hi")
	sh.emitResult("s1", "reply")
	<-out

	// EventLog should contain the result entry but NOT a user-replay entry.
	entries := sh.proc.EventEntries()
	for _, e := range entries {
		if e.Type == "user" && strings.Contains(e.Summary, "hi") {
			t.Errorf("EventLog contains a user entry for 'hi' — replay should be filtered. got=%+v", e)
		}
	}
	// Must contain a result entry
	foundResult := false
	for _, e := range entries {
		if e.Type == "result" {
			foundResult = true
		}
	}
	if !foundResult {
		t.Error("EventLog does not contain result entry")
	}
}

// TestPassthrough_FIFOOrder_TwoIndependentSends verifies that two independent
// (non-merged) sends get their own results in FIFO order — ie. the first
// sender's uuid-match claims result #1, the second claims result #2.
func TestPassthrough_FIFOOrder_TwoIndependentSends(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	type sendOut struct {
		res *SendResult
		err error
	}
	outA := make(chan sendOut, 1)
	outB := make(chan sendOut, 1)
	go func() {
		res, err := sh.proc.SendPassthrough(context.Background(), "first", nil, nil, "")
		outA <- sendOut{res, err}
	}()
	inputA := sh.expectWrite(t, 2*time.Second)

	// Wait slightly so ordering is stable across runs
	time.Sleep(50 * time.Millisecond)

	go func() {
		res, err := sh.proc.SendPassthrough(context.Background(), "second", nil, nil, "")
		outB <- sendOut{res, err}
	}()
	inputB := sh.expectWrite(t, 2*time.Second)

	// Turn 1: A only
	sh.emitInit("s1")
	sh.emitReplay(inputA.UUID, "first")
	sh.emitResult("s1", "result-A")

	// Turn 2: B only
	sh.emitInit("s1")
	sh.emitReplay(inputB.UUID, "second")
	sh.emitResult("s1", "result-B")

	a := <-outA
	b := <-outB
	if a.err != nil || b.err != nil {
		t.Fatalf("errs: a=%v b=%v", a.err, b.err)
	}
	if a.res.Text != "result-A" {
		t.Errorf("first send got %q, want result-A", a.res.Text)
	}
	if b.res.Text != "result-B" {
		t.Errorf("second send got %q, want result-B", b.res.Text)
	}
}

// TestPassthrough_ACPProtocol_Rejected verifies that non-replay protocols
// can't be used in passthrough mode.
func TestPassthrough_ACPProtocol_Rejected(t *testing.T) {
	// Construct a Process with an ACP-like protocol that returns
	// SupportsReplay() == false.
	p := &Process{
		protocol: &ACPProtocol{},
		done:     make(chan struct{}),
	}
	_, err := p.SendPassthrough(context.Background(), "msg", nil, nil, "")
	if err == nil || !strings.Contains(err.Error(), "does not support replay") {
		t.Errorf("err = %v, want 'does not support replay'", err)
	}
}

// TestPassthrough_DeadProcess_FastReject verifies that SendPassthrough on a
// dead Process returns ErrProcessExited without blocking.
func TestPassthrough_DeadProcess_FastReject(t *testing.T) {
	p := &Process{
		protocol: &ClaudeProtocol{},
		done:     make(chan struct{}),
	}
	close(p.done)
	_, err := p.SendPassthrough(context.Background(), "msg", nil, nil, "")
	if !errors.Is(err, ErrProcessExited) {
		t.Errorf("err = %v, want ErrProcessExited", err)
	}
}

// Silence unused imports when test file compiles without exercising them.
var _ = net.Pipe
