package turn

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/metrics"
)

// TestDetached_PassthroughModeRunsEachRequestAlone: in ModePassthrough every
// request is its own First turn with SendSpec.Passthrough, outside the Queue
// and without NotifyIdle.
func TestDetached_PassthroughModeRunsEachRequestAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModePassthrough)
	release := h.hold()
	adm := &fakeAdmission{rec: h.rec, async: true}
	a, b := newOrigin(h.rec, "a", "im:x"), newOrigin(h.rec, "b", "im:x")

	if ack := h.submit("m1", a, adm); ack != AckDetached {
		t.Fatalf("Submit = %v, want AckDetached", ack)
	}
	if ack := h.submit("m2", b, adm); ack != AckDetached {
		t.Fatalf("concurrent Submit = %v, want AckDetached", ack)
	}
	h.rec.waitFor(t, "send:k:m1", 1)
	h.rec.waitFor(t, "send:k:m2", 1)
	release()
	release()
	adm.wg.Wait()

	for _, c := range h.s.sendCalls() {
		if c.spec != (SendSpec{Passthrough: true, Priority: PriorityNormal}) {
			t.Fatalf("spec = %+v, want passthrough at normal priority", c.spec)
		}
	}
	for _, o := range []*fakeOrigin{a, b} {
		infos := o.turnInfos()
		if len(infos) != 1 || !infos[0].First || infos[0].Role != RoleHead || infos[0].Merged != 1 || len(infos[0].Mates) != 0 {
			t.Fatalf("%s TurnInfo = %+v, want a lone First head", o.name, infos)
		}
		if got := o.finished(); len(got) != 1 || got[0].Stage != StageDone {
			t.Fatalf("%s outcomes = %+v", o.name, got)
		}
	}
	if h.rec.count("idle") != 0 || h.rec.count("start:owner") != 0 {
		t.Fatalf("detached turns touched the owner loop: %v", h.rec.snapshot())
	}
}

// TestDetached_UrgentPreemptsInCollectMode (#3004 分叉 6): a PriorityNow
// request runs detached in any mode, is not First, does not queue behind the
// running owner, and carries PriorityNow to Send.
func TestDetached_UrgentPreemptsInCollectMode(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModeCollect)
	release := h.hold()
	adm := &fakeAdmission{rec: h.rec, async: true}
	u := newOrigin(h.rec, "u", "im:x")

	h.submit("m1", newOrigin(h.rec, "a", "im:x"), adm)
	h.rec.waitFor(t, "send:k:m1", 1)
	ack := h.o.Submit(context.Background(), Request{Key: "k", Text: "now", Priority: PriorityNow, Origin: u}, adm)
	if ack != AckDetached {
		t.Fatalf("urgent Submit = %v, want AckDetached", ack)
	}
	h.rec.waitFor(t, "send:k:now", 1)
	if d := h.q.depth("k"); d != 0 {
		t.Fatalf("urgent request queued (depth %d)", d)
	}
	release()
	release()
	h.rec.waitFor(t, "idle", 1)
	adm.wg.Wait()

	calls := h.s.sendCalls()
	if calls[1].spec != (SendSpec{Passthrough: true, Priority: PriorityNow}) {
		t.Fatalf("urgent spec = %+v", calls[1].spec)
	}
	if infos := u.turnInfos(); len(infos) != 1 || infos[0].First {
		t.Fatalf("urgent TurnInfo = %+v, want non-First", infos)
	}
}

// TestDetached_PanicRecovered (#3004 分叉 9): a panicking detached turn is
// recovered and counted, its origin is told, and key's queue is discarded
// as for an owner-loop panic.
func TestDetached_PanicRecovered(t *testing.T) {
	h := newHarness(8, ModeCollect)
	h.s.panicEarly = func(text string) bool { return text == "now" }
	release := h.hold()
	adm := &fakeAdmission{rec: h.rec, async: true}
	u, q := newOrigin(h.rec, "u", "im:x"), newOrigin(h.rec, "q", "ws:q")
	before := metrics.PanicRecoveredTotal.Value()

	h.submit("m1", newOrigin(h.rec, "a", "im:x"), adm)
	h.rec.waitFor(t, "send:k:m1", 1)
	h.submit("m2", q, adm)
	h.o.Submit(context.Background(), Request{Key: "k", Text: "now", Priority: PriorityNow, Origin: u}, adm)
	h.rec.waitFor(t, "finish:u:send+panic", 1)
	release()
	h.rec.waitFor(t, "idle", 1)
	adm.wg.Wait()

	if d := metrics.PanicRecoveredTotal.Value() - before; d != 1 {
		t.Fatalf("PanicRecoveredTotal moved by %d, want 1", d)
	}
	if h.rec.count("dropped:q:panic") != 1 {
		t.Fatalf("queued origin not dropped on the detached panic: %v", h.rec.snapshot())
	}
	if h.rec.count("after:k") != 1 {
		t.Fatalf("AfterTurn count = %d, want 1 (only the owner's turn reached delivery)", h.rec.count("after:k"))
	}
}

// TestDetached_Declined: a declined detached Admit runs nothing and reports
// AckShuttingDown.
func TestDetached_Declined(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModePassthrough)
	a := newOrigin(h.rec, "a", "im:x")
	if ack := h.submit("m1", a, &fakeAdmission{rec: h.rec, decline: true}); ack != AckShuttingDown {
		t.Fatalf("Submit = %v, want AckShuttingDown", ack)
	}
	h.rec.assertOrder(t, "admit:detached", "admitted:a:shutting_down")
	if len(a.turnInfos()) != 0 || len(h.s.sendCalls()) != 0 {
		t.Fatal("a declined detached turn ran")
	}
}
