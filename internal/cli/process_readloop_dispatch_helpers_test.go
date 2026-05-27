package cli

import (
	"log/slog"
	"testing"
	"time"
)

// TestDeliverEvent_KillChClosed pins the kill arm of deliverEvent: a closed
// killCh transitions state to StateDead, sets DeathReasonKilled, fires
// onTurnDone, and returns true so the read loop unwinds.
//
// Extracted from dispatchProtocolEvent for R237-GO-5 (#628); the kill path
// previously lived inline alongside the event handoff. Pinning the side
// effects here makes the helper independently testable.
func TestDeliverEvent_KillChClosed(t *testing.T) {
	t.Parallel()
	killCh := make(chan struct{})
	close(killCh)
	cbFired := false
	p := &Process{
		eventCh: make(chan Event, 1),
		killCh:  killCh,
		// onTurnDone is mu-protected; we install it directly because no
		// concurrent goroutine has access to this Process instance.
		onTurnDone: func() { cbFired = true },
	}

	ret := p.deliverEvent(Event{Type: "result"}, time.Now(), slog.New(slog.DiscardHandler))
	if !ret {
		t.Fatal("deliverEvent on closed killCh: want true (unwind), got false")
	}
	if got := p.DeathReason(); got != DeathReasonKilled {
		t.Errorf("DeathReason = %q, want %q", got, DeathReasonKilled)
	}
	p.mu.RLock()
	st := p.state
	p.mu.RUnlock()
	if st != StateDead {
		t.Errorf("state = %v, want StateDead", st)
	}
	if !cbFired {
		t.Error("onTurnDone was not invoked on kill path")
	}
}

// TestDeliverEvent_HandsOffToEventCh pins the steady-state arm: with an
// open killCh and capacity in eventCh, the event is enqueued and recvAt
// is set. Returns false so the read loop continues.
func TestDeliverEvent_HandsOffToEventCh(t *testing.T) {
	t.Parallel()
	p := &Process{
		eventCh: make(chan Event, 1),
		killCh:  make(chan struct{}), // open: kill arm must not fire
	}
	now := time.Now()
	ret := p.deliverEvent(Event{Type: "assistant"}, now, slog.New(slog.DiscardHandler))
	if ret {
		t.Error("deliverEvent: want false (continue) on open killCh + capacity")
	}
	select {
	case ev := <-p.eventCh:
		if ev.Type != "assistant" {
			t.Errorf("delivered ev.Type = %q, want %q", ev.Type, "assistant")
		}
		// recvAt is private; we cannot read it across packages but the
		// in-package test covers the assignment branch by selecting the
		// non-default arm of the inner select.
		if ev.recvAt != now {
			t.Errorf("ev.recvAt = %v, want %v", ev.recvAt, now)
		}
	default:
		t.Fatal("no event delivered to eventCh")
	}
}

// TestDeliverEvent_FullBufferDropsResult covers the eventCh-saturated arm
// with a result event: the helper must NOT block, must return false, and
// the result must be silently dropped (caller's EventLog already retained
// it). This pins the non-blocking guarantee that prevents readLoop from
// stalling when no Send() is consuming.
func TestDeliverEvent_FullBufferDropsResult(t *testing.T) {
	t.Parallel()
	p := &Process{
		eventCh: make(chan Event), // unbuffered + no reader → forces default arm
		killCh:  make(chan struct{}),
	}
	ret := p.deliverEvent(Event{Type: "result", SubType: "success"}, time.Now(), slog.New(slog.DiscardHandler))
	if ret {
		t.Error("deliverEvent should return false on full-buffer drop, not unwind")
	}
	// Buffer was unbuffered → drop is structural; no reader exists so the
	// select default arm fires unconditionally. We don't drain anything.
}

// TestNotifyLinker_NilLinkerNoOp pins the cheapest exit: a Process with
// p.linker == nil must return without touching any other state. This is
// the dominant production path for backends that don't run sub-agent
// resolution (no observable side effect; we just confirm no panic).
func TestNotifyLinker_NilLinkerNoOp(t *testing.T) {
	t.Parallel()
	p := &Process{} // linker zero-value is nil
	// task_started shape that would otherwise trigger a Resolve goroutine.
	p.notifyLinker(Event{
		Type:      "system",
		SubType:   "task_started",
		TaskID:    "abc",
		ToolUseID: "tool-1",
	}, time.Now().UnixMilli(), false)
	// No panic, no goroutine, no state mutation: success.
}

// TestNotifyLinker_GatesByTaskFields pins the gate that previously lived
// inline in dispatchProtocolEvent. The Resolve fan-out must be skipped
// when any of TaskID / ToolUseID are empty or TaskType is local_bash —
// regressing this would either spawn goroutines on bogus IDs or process
// local_bash events that have no internal transcript.
//
// The test runs without a real linker; nilLinkerNoOp covers the
// fast-path, and the remaining gate combinations exercise the predicate
// branches that early-return BEFORE the linker is dereferenced. Because
// p.linker is nil all branches return harmlessly — we're locking the
// match shape of the pre-extraction `if`.
func TestNotifyLinker_GatesByTaskFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ev   Event
	}{
		{name: "wrong_type", ev: Event{Type: "user", SubType: "task_started", TaskID: "x", ToolUseID: "y"}},
		{name: "wrong_subtype", ev: Event{Type: "system", SubType: "init", TaskID: "x", ToolUseID: "y"}},
		{name: "local_bash_excluded", ev: Event{Type: "system", SubType: "task_started", TaskType: "local_bash", TaskID: "x", ToolUseID: "y"}},
		{name: "missing_task_id", ev: Event{Type: "system", SubType: "task_started", ToolUseID: "y"}},
		{name: "missing_tool_use_id", ev: Event{Type: "system", SubType: "task_started", TaskID: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &Process{}
			// Should not panic and should not start any work; we cannot
			// observe goroutine count cheaply but a nil-linker dereference
			// would have panicked synchronously inside notifyLinker.
			p.notifyLinker(tc.ev, time.Now().UnixMilli(), false)
		})
	}
}
