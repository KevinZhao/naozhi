package server

// Cross-entry rows of #3004's divergence table: IM and dashboard turns run on
// ONE turn.Orchestrator, so each entry's owner loop drains what the other
// enqueued. These pin who hears about the outcome, and the order the
// dashboard sees state and errors in. The rows D flipped say so.

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// Row 5 flipped in D: a dashboard /new tells every message it discards; the
// IM one's ⏳ comes off.
func TestTurnParity05_Cross_DashResetClearsIMHourglass(t *testing.T) {
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
	if h.plat.removedFor("m2") != 1 {
		t.Fatalf("IM ⏳ removed %d times after a dashboard /new discarded the message, want 1", h.plat.removedFor("m2"))
	}
	if acks := ws.errorAcks(); len(acks) != 0 {
		t.Fatalf("dropped dashboard message got %+v, want nothing", acks)
	}
}

// Row 13 flipped in D: a dashboard owner's shutdown exit tells what it
// discards; the IM message's ⏳ comes off.
func TestTurnParity13_Cross_DashShutdownClearsIMHourglass(t *testing.T) {
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
	if h.plat.addedFor("m2") != 1 || h.plat.removedFor("m2") != 1 {
		t.Fatalf("IM ⏳ added=%d removed=%d, want it cleared by the dashboard owner's shutdown exit",
			h.plat.addedFor("m2"), h.plat.removedFor("m2"))
	}
}

// Row 14 flipped in D: an IM message pushed out by a dashboard one is told
// through its own origin; its ⏳ comes off.
func TestTurnParity14_Cross_DashEvictionClearsIMHourglass(t *testing.T) {
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
	if h.plat.removedFor("m2") != 1 || len(h.plat.allReplies()) != 0 {
		t.Fatalf("evicted IM message: ⏳ removed %d, replies %q; want the ⏳ cleared once and no reply", h.plat.removedFor("m2"), h.plat.allReplies())
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

// Row 19b flipped in D: a dashboard owner's drain turn answers the IM chat
// whose message it carried, and clears that message's ⏳.
func TestTurnParity19b_Cross_DashOwnerRepliesToIM(t *testing.T) {
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
	if r := h.plat.allReplies(); len(r) != 1 || !strings.HasPrefix(r[0], "R2") {
		t.Fatalf("IM replies %q, want R2 delivered to the IM chat", r)
	}
	if h.plat.removedFor("m2") != 1 {
		t.Fatalf("IM ⏳ removed %d times, want 1", h.plat.removedFor("m2"))
	}
	if acks := ws.errorAcks(); len(acks) != 0 {
		t.Fatalf("error acks %+v", acks)
	}
}

// Row 20 flipped in D: every IM receiver of a turn gets the thinking banner,
// so does an IM message a dashboard owner drained.
func TestTurnParity20_Cross_ThinkingBannerOnEveryIMReceiver(t *testing.T) {
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
		if e := h.plat.allEdits(); len(e) == 0 || !strings.HasPrefix(e[len(e)-1], "R1") {
			t.Fatalf("banner edits = %q, want the answer edited into the banner last", e)
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
		if r := h.plat.allReplies(); len(r) != 1 || r[0] != "💭 思考中..." {
			t.Fatalf("IM replies = %q, want only the thinking banner", r)
		}
		if e := h.plat.allEdits(); len(e) == 0 || !strings.HasPrefix(e[len(e)-1], "R2") {
			t.Fatalf("banner edits = %q, want the answer edited into the banner last", e)
		}
	})
}

// isReady matches a settled (non-running) session_state for parityKey.
func isReady(f parityFrame) bool {
	return f.Type == "session_state" && f.Key == parityKey && f.State != "" && f.State != "running"
}

// readyBefore reports whether a settled session_state for parityKey appears
// in frames before index end.
func readyBefore(frames []parityFrame, end int) bool {
	return slices.ContainsFunc(frames[:end], isReady)
}

func frameIndex(frames []parityFrame, match func(parityFrame) bool) int {
	for i, f := range frames {
		if match(f) {
			return i
		}
	}
	return -1
}

// Row 21 flipped in D: a dashboard origin's failure goes out before the
// settled session_state (which would make the client drop it); the IM reply
// still follows it.
func TestTurnParity21_Cross_OutcomeOrderAroundStateBroadcast(t *testing.T) {
	// errorThenReady fails t unless, among the frames after the first from,
	// the one match accepts arrives before the settled session_state.
	errorThenReady := func(t *testing.T, ws *parityWS, from int, match func(parityFrame) bool) {
		t.Helper()
		ws.waitFor(t, "settled session_state", isReady)
		frames := ws.flush()[from:]
		if i := frameIndex(frames, match); i < 0 || readyBefore(frames, i) {
			t.Fatalf("frames %+v: the failure must precede the settled session_state", frames)
		}
	}
	t.Run("WS error ack", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{})
		turns := h.session(parityKey, false)
		ws := h.ws()
		ws.send("w1", "first")
		turns.turn(t, "turn", parityOutcome{Err: errParityBoom})
		errorThenReady(t, ws, 0, func(f parityFrame) bool { return f.Type == "send_ack" && f.Status == "error" })
	})
	t.Run("HTTP send_error", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{})
		turns := h.session(parityKey, false)
		ws := h.ws()
		if s := h.httpSend(t, "first"); s != "accepted" {
			t.Fatalf("HTTP status = %q", s)
		}
		turns.next(t, "owner turn")
		if s := h.httpSend(t, "queued"); s != "queued" {
			t.Fatalf("HTTP status = %q", s)
		}
		turns.answer(okTurn("R1"))
		ws.waitFor(t, "owner turn settled", isReady)
		from := len(ws.seen)
		turns.turn(t, "drain turn", parityOutcome{Err: errParityBoom})
		errorThenReady(t, ws, from, func(f parityFrame) bool { return f.Type == "send_error" })
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

// Row 22 flipped in D: HTTP sends on a key share one receiver, so a failed
// turn is one send_error per subscriber however many HTTP sends it carried;
// informational outcomes are not broadcast.
func TestTurnParity22_Cross_HTTPFailureBroadcastOncePerTurn(t *testing.T) {
	isSendError := func(f parityFrame) bool { return f.Type == "send_error" }
	t.Run("one request", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{mode: "passthrough"})
		turns := h.session(parityKey, true)
		a, b := h.ws(), h.ws()

		if s := h.httpSend(t, "first"); s != "accepted" {
			t.Fatalf("passthrough HTTP status = %q, want accepted", s)
		}
		turns.turn(t, "failing turn", parityOutcome{Err: errParityBoom})
		a.waitFor(t, "send_error on a", isSendError)
		b.waitFor(t, "send_error on b", isSendError)
		h.waitEngineIdle()

		h.httpSend(t, "second")
		turns.turn(t, "turn ended by /new", parityOutcome{Err: clierr.ErrSessionReset})
		h.waitEngineIdle()

		for name, w := range map[string]*parityWS{"a": a, "b": b} {
			errs := w.framesOfType("send_error")
			if len(errs) != 1 || errs[0].Error != asyncErrorMessage(errParityBoom) {
				t.Errorf("subscriber %s send_error frames = %+v, want exactly one, for the real failure", name, errs)
			}
		}
	})
	t.Run("merged turn of three requests", func(t *testing.T) {
		h := newParityHarness(t, parityOpts{})
		turns := h.session(parityKey, false)
		a, b := h.ws(), h.ws()
		h.httpSend(t, "owner")
		turns.next(t, "owner turn")
		for _, text := range []string{"q1", "q2", "q3"} {
			if s := h.httpSend(t, text); s != "queued" {
				t.Fatalf("HTTP %s status = %q", text, s)
			}
		}
		turns.answer(okTurn("R1"))
		turns.turn(t, "merged drain turn", parityOutcome{Err: errParityBoom})
		h.waitEngineIdle()
		for name, w := range map[string]*parityWS{"a": a, "b": b} {
			if errs := w.framesOfType("send_error"); len(errs) != 1 {
				t.Errorf("subscriber %s send_error frames = %+v, want exactly one for the merged turn", name, errs)
			}
		}
	})
}
