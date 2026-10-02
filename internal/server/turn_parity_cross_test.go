package server

// Cross-entry rows of #3004's divergence table: the IM dispatcher and the
// dashboard engine own turns on ONE queue, so each drains what the other
// enqueued. These pin who hears about the outcome today, and the order the
// dashboard sees state and errors in.

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

func TestTurnParity05_Cross_DashResetLeavesIMHourglass(t *testing.T) {
	h := newParityHarness(t, parityOpts{reactor: true})
	turns := h.session(parityKey, false)
	ws := h.ws()
	owner := h.imAsync("m1", "first")
	turns.next(t, "IM owner turn")
	h.imSend("m2", "from im")
	ws.send("w1", "from dash")
	if s := ws.ack(t, "w1"); s != "queued" {
		t.Fatalf("w1 ack = %q", s)
	}
	ws.send("w2", "/new")
	if s := ws.ack(t, "w2"); s != "reset" {
		t.Fatalf("w2 ack = %q", s)
	}
	turns.answer(okTurn("R1"))
	h.plat.waitReply(t, "owner reply")
	h.waitDone(owner, "IM owner loop")
	turns.noMoreTurns(t)
	if h.plat.removedFor("m2") != 0 {
		t.Fatal("dashboard /new cleared the IM ⏳; today it drops the message and leaves the ⏳")
	}
	if acks := ws.errorAcks(); len(acks) != 0 {
		t.Fatalf("dropped dashboard message got %+v, want nothing", acks)
	}
}

func TestTurnParity13_Cross_DashShutdownLeavesIMHourglass(t *testing.T) {
	h := newParityHarness(t, parityOpts{reactor: true, collect: time.Hour})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "first")
	turns.next(t, "dashboard owner turn")
	h.imSend("m2", "from im")
	h.engine().cancel()
	turns.answer(okTurn("R1"))
	h.waitEngineIdle()
	turns.noMoreTurns(t)
	if h.plat.addedFor("m2") != 1 || h.plat.removedFor("m2") != 0 {
		t.Fatalf("IM ⏳ added=%d removed=%d, want it left behind by the dashboard owner's shutdown exit",
			h.plat.addedFor("m2"), h.plat.removedFor("m2"))
	}
}

func TestTurnParity14_Cross_DashEvictionLeavesIMHourglass(t *testing.T) {
	h := newParityHarness(t, parityOpts{reactor: true, maxDepth: 1})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "first")
	turns.next(t, "dashboard owner turn")
	h.imSend("m2", "from im")
	ws.send("w3", "from dash")
	ws.ack(t, "w3")
	turns.answer(okTurn("R1"))
	if c := turns.turn(t, "drain turn", okTurn("R2")); c.Text != "from dash" {
		t.Fatalf("drain turn = %q, want only the dashboard message", c.Text)
	}
	h.waitEngineIdle()
	if h.plat.removedFor("m2") != 0 || len(h.plat.allReplies()) != 0 {
		t.Fatalf("evicted IM message: ⏳ removed %d, replies %q; want neither", h.plat.removedFor("m2"), h.plat.allReplies())
	}
}

func TestTurnParity19a_Cross_IMOwnerRepliesForDashMessage(t *testing.T) {
	h := newParityHarness(t, parityOpts{reactor: true})
	turns := h.session(parityKey, false)
	ws := h.ws()
	owner := h.imAsync("m1", "first")
	turns.next(t, "IM owner turn")
	ws.send("w2", "from dash")
	if s := ws.ack(t, "w2"); s != "queued" {
		t.Fatalf("w2 ack = %q", s)
	}
	turns.answer(okTurn("R1"))
	h.plat.waitReply(t, "owner reply")
	if c := turns.turn(t, "drain turn", okTurn("R2")); c.Text != "from dash" {
		t.Fatalf("drain turn = %q", c.Text)
	}
	if r := h.plat.waitReply(t, "drain reply"); !strings.HasPrefix(r, "R2") {
		t.Fatalf("IM reply for the dashboard-enqueued batch = %q, want R2 delivered to the IM chat", r)
	}
	h.waitDone(owner, "IM owner loop")
	if acks := ws.errorAcks(); len(acks) != 0 {
		t.Fatalf("error acks %+v", acks)
	}
}

func TestTurnParity19b_Cross_DashOwnerDropsIMReply(t *testing.T) {
	h := newParityHarness(t, parityOpts{reactor: true})
	turns := h.session(parityKey, false)
	ws := h.ws()
	ws.send("w1", "first")
	turns.next(t, "dashboard owner turn")
	h.imSend("m2", "from im")
	turns.answer(okTurn("R1"))
	if c := turns.turn(t, "drain turn", okTurn("R2")); c.Text != "from im" {
		t.Fatalf("drain turn = %q", c.Text)
	}
	h.waitEngineIdle()
	if r := h.plat.allReplies(); len(r) != 0 {
		t.Fatalf("IM replies %q, want none: a dashboard owner does not answer the IM chat", r)
	}
	if h.plat.removedFor("m2") != 0 {
		t.Fatal("IM ⏳ cleared; today a dashboard owner never clears it")
	}
}

func TestTurnParity20_Cross_ThinkingBannerOnlyOnIMOwner(t *testing.T) {
	thinking := []clievent.Event{{Type: "assistant"}}
	t.Run("IM owner", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{reactor: true, interim: true})
		turns := h.session(parityKey, false)
		owner := h.imAsync("m1", "first")
		turns.turn(t, "IM owner turn", parityOutcome{Result: &clievent.SendResult{Text: "R1"}, Events: thinking})
		if r := h.plat.waitReply(t, "thinking banner"); r != "💭 思考中..." {
			t.Fatalf("first IM reply = %q, want the thinking banner", r)
		}
		h.waitDone(owner, "IM owner loop")
		// Not "last": a stale editLoop redraw can still land after it (#3066).
		if e := h.plat.allEdits(); !slices.ContainsFunc(e, func(s string) bool { return strings.HasPrefix(s, "R1") }) {
			t.Fatalf("banner edits = %q, want the answer edited into the banner", e)
		}
		if r := h.plat.allReplies(); len(r) != 1 {
			t.Fatalf("IM replies = %q, want only the banner (the answer is an edit)", r)
		}
	})
	t.Run("dashboard owner draining an IM message", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{reactor: true, interim: true})
		turns := h.session(parityKey, false)
		ws := h.ws()
		ws.send("w1", "first")
		turns.next(t, "dashboard owner turn")
		h.imSend("m2", "from im")
		turns.answer(okTurn("R1"))
		turns.turn(t, "drain turn", parityOutcome{Result: &clievent.SendResult{Text: "R2"}, Events: thinking})
		h.waitEngineIdle()
		if r, e := h.plat.allReplies(), h.plat.allEdits(); len(r) != 0 || len(e) != 0 {
			t.Fatalf("IM replies %q, edits %q; want no banner (the dashboard turn has no IM callback)", r, e)
		}
	})
}

// readyBefore reports whether a non-running session_state for parityKey
// appears in frames before index end.
func readyBefore(frames []parityFrame, end int) bool {
	for _, f := range frames[:end] {
		if f.Type == "session_state" && f.Key == parityKey && f.State != "" && f.State != "running" {
			return true
		}
	}
	return false
}

func frameIndex(frames []parityFrame, match func(parityFrame) bool) int {
	for i, f := range frames {
		if match(f) {
			return i
		}
	}
	return -1
}

func TestTurnParity21_Cross_StateBroadcastPrecedesOutcome(t *testing.T) {
	t.Run("WS error ack", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{mode: "passthrough"})
		turns := h.session(parityKey, true)
		ws := h.ws()
		ws.send("w1", "first")
		turns.turn(t, "turn", parityOutcome{Err: errParityBoom})
		ws.waitFor(t, "error ack", func(f parityFrame) bool { return f.Type == "send_ack" && f.Status == "error" })
		frames := ws.flush()
		i := frameIndex(frames, func(f parityFrame) bool { return f.Type == "send_ack" && f.Status == "error" })
		if !readyBefore(frames, i) {
			t.Fatalf("frames %+v: the settled session_state must precede the error ack today", frames)
		}
	})
	t.Run("HTTP send_error", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{mode: "passthrough"})
		turns := h.session(parityKey, true)
		ws := h.ws()
		if s := h.httpSend(t, "first"); s != "accepted" {
			t.Fatalf("HTTP status = %q", s)
		}
		turns.turn(t, "turn", parityOutcome{Err: errParityBoom})
		ws.waitFor(t, "send_error", func(f parityFrame) bool { return f.Type == "send_error" })
		frames := ws.flush()
		if !readyBefore(frames, frameIndex(frames, func(f parityFrame) bool { return f.Type == "send_error" })) {
			t.Fatalf("frames %+v: the settled session_state must precede send_error today", frames)
		}
	})
	t.Run("IM reply", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{})
		turns := h.session(parityKey, false)
		ws := h.ws()
		var (
			mu      sync.Mutex
			atReply []parityFrame
		)
		h.plat.setOnReply(func(string) {
			mu.Lock()
			defer mu.Unlock()
			atReply = append([]parityFrame(nil), ws.flush()...)
		})
		owner := h.imAsync("m1", "first")
		turns.turn(t, "IM owner turn", okTurn("R1"))
		h.plat.waitReply(t, "owner reply")
		h.waitDone(owner, "IM owner loop")
		mu.Lock()
		defer mu.Unlock()
		if !readyBefore(atReply, len(atReply)) {
			t.Fatalf("frames buffered when the IM reply went out: %+v; the settled session_state must already be there", atReply)
		}
	})
}

func TestTurnParity22_Cross_HTTPFailureBroadcastOncePerRequest(t *testing.T) {
	h := newParityHarness(t, parityOpts{mode: "passthrough"})
	turns := h.session(parityKey, true)
	a, b := h.ws(), h.ws()
	isSendError := func(f parityFrame) bool { return f.Type == "send_error" }

	h.httpSend(t, "first")
	turns.turn(t, "failing turn", parityOutcome{Err: errParityBoom})
	a.waitFor(t, "send_error on a", isSendError)
	b.waitFor(t, "send_error on b", isSendError)
	h.waitEngineIdle()

	h.httpSend(t, "second")
	turns.turn(t, "turn aborted by /urgent", parityOutcome{Err: clierr.ErrAbortedByUrgent})
	h.waitEngineIdle()

	for name, w := range map[string]*parityWS{"a": a, "b": b} {
		errs := w.framesOfType("send_error")
		if len(errs) != 1 || errs[0].Error != asyncErrorMessage(errParityBoom) {
			t.Errorf("subscriber %s send_error frames = %+v, want exactly one, for the real failure", name, errs)
		}
	}
}
