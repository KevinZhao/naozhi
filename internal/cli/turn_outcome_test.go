package cli

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// A claude result frame's is_error survives decoding and reaches SendResult,
// with the subtype, so a consumer can tell a failed turn from an empty answer.
func TestClaudeResult_IsErrorReachesSendResult(t *testing.T) {
	t.Parallel()
	evs, _, err := (&ClaudeProtocol{}).ReadEvent(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"","session_id":"s1"}`)
	if err != nil || len(evs) != 1 {
		t.Fatalf("ReadEvent = %v, %v; want one event", evs, err)
	}
	got := resultFromEvent(evs[0])
	if !got.IsError || got.SubType != "error_max_turns" {
		t.Errorf("IsError, SubType = %v, %q; want true, error_max_turns", got.IsError, got.SubType)
	}
}

// Every SendResult field except the merge metadata comes from the result
// frame, so resultFromEvent must fill each one from a fully populated Event:
// a field added to SendResult but not copied here is lost by all three owners.
func TestResultFromEvent_CopiesEveryFrameField(t *testing.T) {
	t.Parallel()
	var ev clievent.Event
	v := reflect.ValueOf(&ev).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("x")
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Float64:
			f.SetFloat(1)
		case reflect.Int, reflect.Int64:
			f.SetInt(1)
		case reflect.Map:
			m := reflect.MakeMap(f.Type())
			m.SetMapIndex(reflect.New(f.Type().Key()).Elem(), reflect.New(f.Type().Elem()).Elem())
			f.Set(m)
		case reflect.Pointer:
			f.Set(reflect.New(f.Type().Elem()))
		}
	}
	merge := map[string]bool{"MergedCount": true, "MergedWithHead": true, "HeadText": true}
	got := resultFromEvent(ev)
	rv := reflect.ValueOf(got)
	for i := 0; i < rv.NumField(); i++ {
		name := rv.Type().Field(i).Name
		if zero := rv.Field(i).IsZero(); zero != merge[name] {
			t.Errorf("SendResult.%s zero = %v, want %v", name, zero, merge[name])
		}
	}
}

// A codex turn that completed as failed is a structured failure, not just text.
func TestCodexFailedTurn_IsAStructuredFailure(t *testing.T) {
	t.Parallel()
	p := &CodexProtocol{BackendID: "codex"}
	evs, _, err := p.ReadEvent(`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"t1","turn":{"status":"failed","error":{"message":"stream disconnected"}}}}`)
	if err != nil || len(evs) == 0 {
		t.Fatalf("ReadEvent = %v, %v", evs, err)
	}
	res := evs[len(evs)-1]
	if res.Type != "result" || !res.IsError || res.SubType != "error" {
		t.Fatalf("result = %+v, want an IsError result with subtype error", res)
	}
	want := clievent.BackendError{Backend: "codex", Message: "stream disconnected"}
	if res.BackendError == nil || *res.BackendError != want {
		t.Errorf("BackendError = %+v, want %+v", res.BackendError, want)
	}
	if res.Result != "stream disconnected" {
		t.Errorf("Result = %q, want the failure text unchanged", res.Result)
	}
}

// abortedResult is the frame claude emits when a turn is aborted.
const abortedResult = `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","session_id":"s1"}`

// startTurn sends text over passthrough and, unless it is a priority send
// joining a turn already in flight, plays the CLI starting that turn. It
// returns the slot's result channel and the uuid the message was written with.
func startTurn(t *testing.T, sh *passthroughShim, text, priority string) (<-chan *clievent.SendResult, string) {
	t.Helper()
	out := make(chan *clievent.SendResult, 1)
	go func() {
		res, err := sh.proc.SendPassthrough(context.Background(), text, nil, nil, priority)
		if err != nil {
			t.Errorf("SendPassthrough(%q): %v", text, err)
		}
		out <- res
	}()
	in := sh.expectWrite(t, 2*time.Second)
	if priority == "" {
		playTurnStart(t, sh, in.UUID, text)
	}
	return out, in.UUID
}

// playTurnStart is the CLI starting the turn for the message written as uuid.
func playTurnStart(t *testing.T, sh *passthroughShim, uuid, text string) {
	t.Helper()
	sh.emitInit("s1")
	sh.emitReplay(uuid, text)
	testhelper.Eventually(t, func() bool { return sh.proc.State() == StateRunning }, 2*time.Second,
		"turn never reached Running")
}

func recvResult(t *testing.T, ch <-chan *clievent.SendResult) *clievent.SendResult {
	t.Helper()
	select {
	case r := <-ch:
		if r == nil {
			t.Fatal("slot returned no result")
		}
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("slot did not return")
		return nil
	}
}

// An abort naozhi requested marks the result it produces, and only that one:
// the next turn's error_during_execution, which nobody asked for, is a failure.
func TestAbortMarker_StampsTheAbortedTurnOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		abort func(*testing.T, *passthroughShim) error
	}{
		{"Interrupt", func(_ *testing.T, sh *passthroughShim) error { sh.proc.Interrupt(); return nil }},
		// A second Stop on the same turn still yields one result.
		{"Interrupt twice", func(_ *testing.T, sh *passthroughShim) error {
			sh.proc.Interrupt()
			sh.proc.Interrupt()
			return nil
		}},
		{"InterruptViaControl", func(t *testing.T, sh *passthroughShim) error {
			err := sh.proc.InterruptViaControl()
			// The control_request is a stdin write; take it so the next
			// turn's expectWrite gets that turn's user message.
			if in := sh.expectWrite(t, 2*time.Second); in.Type != "control_request" {
				t.Fatalf("stdin write = %q, want the control_request", in.Type)
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh := newPassthroughShim(t)
			defer sh.close()
			go sh.proc.readLoop()

			first, _ := startTurn(t, sh, "long job", "")
			if err := tc.abort(t, sh); err != nil {
				t.Fatalf("abort: %v", err)
			}
			sh.srv.SendStdout(abortedResult)
			if r := recvResult(t, first); !r.Aborted || r.SubType != "error_during_execution" || !r.IsError {
				t.Errorf("aborted turn: Aborted, SubType, IsError = %v, %q, %v; want true, error_during_execution, true",
					r.Aborted, r.SubType, r.IsError)
			}

			second, _ := startTurn(t, sh, "next", "")
			sh.srv.SendStdout(abortedResult)
			if r := recvResult(t, second); r.Aborted {
				t.Error("an error_during_execution nobody asked for is marked Aborted: the abort flag outlived its result")
			}
		})
	}
}

// A priority:"now" send aborts the turn in flight: its head gets the marker,
// the urgent message's own turn does not.
func TestAbortMarker_PriorityNowMarksThePreemptedHead(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	head, _ := startTurn(t, sh, "long job", "")
	urgent, uuid := startTurn(t, sh, "stop that", "now")
	sh.srv.SendStdout(abortedResult)
	if r := recvResult(t, head); !r.Aborted {
		t.Error("the turn a priority:\"now\" send preempted is not marked Aborted")
	}

	playTurnStart(t, sh, uuid, "stop that")
	sh.emitResult("s1", "stopped")
	if r := recvResult(t, urgent); r.Aborted || r.Text != "stopped" {
		t.Errorf("urgent turn: Aborted, Text = %v, %q; want false, stopped", r.Aborted, r.Text)
	}
}

// A priority:"now" send with nothing in flight aborts nothing, so it must not
// arm the marker for whatever result comes next.
func TestAbortMarker_PriorityNowWhenIdleArmsNothing(t *testing.T) {
	sh := newPassthroughShim(t)
	defer sh.close()
	go sh.proc.readLoop()

	urgent, uuid := startTurn(t, sh, "hello", "now")
	if sh.proc.turn.abortRequested.armed() {
		t.Error("an idle priority:\"now\" send armed the abort marker")
	}
	playTurnStart(t, sh, uuid, "hello")
	sh.srv.SendStdout(abortedResult)
	if r := recvResult(t, urgent); r.Aborted {
		t.Error("a turn nobody aborted is marked Aborted")
	}
}

// An Interrupt outside a running turn aborts nothing: while spawning it is
// never sent, and when idle there is no turn for it to stop.
func TestAbortMarker_InterruptOutsideATurnArmsNothing(t *testing.T) {
	for _, st := range []ProcessState{StateSpawning, StateReady} {
		t.Run(st.String(), func(t *testing.T) {
			p, srv := shimTestPair(&ClaudeProtocol{})
			startServerDrain(srv)
			p.turn.mu.Lock()
			p.turn.state = st
			p.turn.mu.Unlock()

			p.Interrupt()
			if p.turn.abortRequested.armed() {
				t.Errorf("Interrupt in %v armed the abort marker", st)
			}
		})
	}
}

// Each arm has its own rollback, so one abort's failed send cannot disarm
// another that reached the CLI; a result takes every pending arm at once.
func TestAbortMarker_Counts(t *testing.T) {
	t.Parallel()
	var m abortMarker
	m.arm()
	m.arm()
	m.disarm()
	if !m.take() {
		t.Error("a rolled-back arm disarmed a concurrent one that was sent")
	}
	if m.take() {
		t.Error("a result left arms behind for the next result")
	}
	m.disarm()
	m.arm()
	if !m.take() {
		t.Error("a rollback after the result took the arm went below zero and swallowed the next arm")
	}
}

// On a legacy (non-replay) backend the marker travels on eventCh, which is
// what Send turns into its SendResult.
func TestAbortMarker_LegacyBackendCarriesItOnEventCh(t *testing.T) {
	p, srv := shimTestPair(&ACPProtocol{BackendID: "kiro"})
	startServerDrain(srv)
	p.startReadLoop()
	defer p.Kill()
	p.transition(evSendBegin)

	p.Interrupt()
	srv.SendStdout(`{"jsonrpc":"2.0","id":5,"result":{"stopReason":"cancelled"}}`)
	select {
	case ev := <-p.eventCh:
		if ev.Type != "result" || !ev.Aborted || ev.SubType != "cancelled" {
			t.Errorf("event = %q/%q Aborted=%v, want the cancelled result marked Aborted", ev.Type, ev.SubType, ev.Aborted)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no result reached eventCh")
	}
	if p.turn.abortRequested.armed() {
		t.Error("the marker outlived the result it was stamped on")
	}
}

// An interrupt that never reached the shim aborts nothing, so it must not
// leave the marker armed for the turn's real result.
func TestAbortMarker_FailedInterruptRollsBack(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	p.transition(evSendBegin)
	srv.conn.Close()

	p.Interrupt()
	if p.turn.abortRequested.armed() {
		t.Error("Interrupt whose send failed left abortRequested set")
	}
}

// A priority:"now" message that never reached the CLI aborted nothing.
func TestAbortMarker_FailedPriorityNowWriteRollsBack(t *testing.T) {
	p, srv := shimTestPair(&ClaudeProtocol{})
	p.transition(evTurnStarted)
	srv.conn.Close()

	if _, err := p.SendPassthrough(context.Background(), "stop", nil, nil, "now"); err == nil {
		t.Fatal("SendPassthrough over a closed shim succeeded")
	}
	if p.turn.abortRequested.armed() {
		t.Error("a priority:\"now\" write that failed left abortRequested set")
	}
}

// A turn is in flight for a priority:"now" send to abort both when the CLI
// started one itself (Running, nothing queued) and when a queued message's
// turn is owed but its system/init has not arrived yet.
func TestAbortMarker_PriorityNowArmsForEveryTurnInFlight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inTurn func(*testing.T, *passthroughShim)
	}{
		{"CLI-started turn", func(t *testing.T, sh *passthroughShim) {
			sh.emitInit("s1")
			testhelper.Eventually(t, func() bool { return sh.proc.State() == StateRunning }, 2*time.Second,
				"CLI-started turn never reached Running")
		}},
		{"owed turn", func(t *testing.T, sh *passthroughShim) {
			go func() { _, _ = sh.proc.SendPassthrough(context.Background(), "queued", nil, nil, "") }()
			sh.expectWrite(t, 2*time.Second)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh := newPassthroughShim(t)
			defer sh.close()
			go sh.proc.readLoop()

			tc.inTurn(t, sh)
			// Never answered: the shim closes under it, so its error is expected.
			go func() { _, _ = sh.proc.SendPassthrough(context.Background(), "stop that", nil, nil, "now") }()
			sh.expectWrite(t, 2*time.Second)
			if !sh.proc.turn.abortRequested.armed() {
				t.Error("a priority:\"now\" send with a turn in flight did not arm the abort marker")
			}
		})
	}
}
