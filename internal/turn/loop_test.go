package turn

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// TestOwnerLoop_DrainsEveryBatchUntilEmpty: after each drain turn the loop
// re-arms its collect timer, so a message queued during the second turn
// still gets a third.
func TestOwnerLoop_DrainsEveryBatchUntilEmpty(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModeCollect)
	release := h.hold()
	adm := &fakeAdmission{rec: h.rec, async: true}

	if ack := h.submit("m1", newOrigin(h.rec, "a", "ws:a"), adm); ack != AckOwner {
		t.Fatalf("first Submit = %v, want AckOwner", ack)
	}
	h.rec.waitFor(t, "send:k:m1", 1)
	if ack := h.submit("m2", newOrigin(h.rec, "b", "ws:b"), adm); ack != AckQueued {
		t.Fatalf("second Submit = %v, want AckQueued", ack)
	}
	release()
	h.rec.waitFor(t, "send:k:m2", 1)
	h.submit("m3", newOrigin(h.rec, "c", "ws:c"), adm)
	release()
	h.rec.waitFor(t, "send:k:m3", 1)
	release()
	h.rec.waitFor(t, "idle", 1)
	adm.wg.Wait()

	if got, want := h.s.texts(), []string{"m1", "m2", "m3"}; !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
}

// TestOwnerLoop_MergesQueuedIntoOneTurn: everything queued during a turn is
// coalesced into one follow-up Send.
func TestOwnerLoop_MergesQueuedIntoOneTurn(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModeCollect)
	release := h.hold()
	adm := &fakeAdmission{rec: h.rec, async: true}
	b, c := newOrigin(h.rec, "b", "ws:b"), newOrigin(h.rec, "c", "ws:c")

	h.submit("m1", newOrigin(h.rec, "a", "ws:a"), adm)
	h.rec.waitFor(t, "send:k:m1", 1)
	h.submit("m2", b, adm)
	h.submit("m3", c, adm)
	release()
	h.rec.waitFor(t, "begin:c:head", 1)
	release()
	h.rec.waitFor(t, "idle", 1)
	adm.wg.Wait()

	texts := h.s.texts()
	if len(texts) != 2 {
		t.Fatalf("want 2 turns, got %d: %v", len(texts), texts)
	}
	body := coalescedBody(texts[1])
	if !strings.HasPrefix(texts[1], coalescePrefix) || !strings.Contains(body, "] m2\n") || !strings.Contains(body, "] m3\n") {
		t.Fatalf("drain turn text not the coalesced batch: %q", texts[1])
	}
	for _, o := range []*fakeOrigin{b, c} {
		infos := o.turnInfos()
		if len(infos) != 1 || infos[0].Merged != 2 || infos[0].First {
			t.Fatalf("%s TurnInfo = %+v, want one non-First head with Merged 2", o.name, infos)
		}
	}
}

// TestOwnerLoop_OneDeliveryPerSink (#3004 分叉 22): batch members sharing a
// sink get one Begin, on the first of them, with the rest as Mates.
func TestOwnerLoop_OneDeliveryPerSink(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModeCollect)
	release := h.hold()
	adm := &fakeAdmission{rec: h.rec, async: true}
	a := newOrigin(h.rec, "a", "http:k")
	b, c, d := newOrigin(h.rec, "b", "http:k"), newOrigin(h.rec, "c", "http:k"), newOrigin(h.rec, "d", "http:k")
	w1, w2 := newOrigin(h.rec, "w1", "ws:1"), newOrigin(h.rec, "w2", "ws:2")

	h.submit("m1", a, adm)
	h.rec.waitFor(t, "send:k:m1", 1)
	for i, o := range []*fakeOrigin{b, w1, c, w2, d} {
		h.submit("q"+string(rune('0'+i)), o, adm)
	}
	release()
	h.rec.waitFor(t, "begin:w2:head", 1)
	release()
	h.rec.waitFor(t, "idle", 1)
	adm.wg.Wait()

	if n := h.rec.count("begin:b:head"); n != 1 {
		t.Fatalf("head of the http sink begun %d times, want 1", n)
	}
	for _, ev := range []string{"begin:c:head", "begin:d:head", "begin:a:observer"} {
		if n := h.rec.count(ev); n != 0 {
			t.Fatalf("%s recorded %d times; members of one sink must share the head's delivery", ev, n)
		}
	}
	if infos := b.turnInfos(); len(infos) != 1 || len(infos[0].Mates) != 2 || infos[0].Mates[0] != c || infos[0].Mates[1] != d {
		t.Fatalf("head TurnInfo = %+v, want Mates [c d]", infos)
	}
	if n := h.rec.count("finish:b:done"); n != 1 {
		t.Fatalf("http sink finished %d times, want 1", n)
	}
	for _, w := range []*fakeOrigin{w1, w2} {
		if got := w.finished(); len(got) != 1 {
			t.Fatalf("%s finished %d times, want 1 (distinct sinks each get a delivery)", w.name, len(got))
		}
	}
}

// TestOwnerLoop_OwnerObservesBatchItIsNotIn (#3004 分叉 19a): the owner's
// sink receives a drain turn made only of other sinks' requests, as
// Observer.
func TestOwnerLoop_OwnerObservesBatchItIsNotIn(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModeCollect)
	release := h.hold()
	adm := &fakeAdmission{rec: h.rec, async: true}
	owner := newOrigin(h.rec, "im", "im:feishu:chat")

	h.submit("m1", owner, adm)
	h.rec.waitFor(t, "send:k:m1", 1)
	h.submit("m2", newOrigin(h.rec, "ws", "ws:1"), adm)
	release()
	h.rec.waitFor(t, "begin:im:observer", 1)
	release()
	h.rec.waitFor(t, "idle", 1)
	adm.wg.Wait()

	infos := owner.turnInfos()
	if len(infos) != 2 || infos[0].Role != RoleHead || !infos[0].First || infos[1].Role != RoleObserver || infos[1].First {
		t.Fatalf("owner TurnInfos = %+v, want [First head, non-First observer]", infos)
	}
	if got := owner.finished(); len(got) != 2 || got[1].Stage != StageDone || got[1].Result == nil || got[1].Result.Text != "re:m2" {
		t.Fatalf("owner outcomes = %+v, want the drain turn's result delivered to the observer", got)
	}
}

// TestDeliver_NonBlockingThenAfterTurnThenBlocking (#3004 分叉 21):
// receivers whose Blocking is false are finished before Sender.AfterTurn,
// blocking ones after it, whatever their order in the batch.
func TestDeliver_NonBlockingThenAfterTurnThenBlocking(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModeCollect)
	release := h.hold()
	adm := &fakeAdmission{rec: h.rec, async: true}
	im := newOrigin(h.rec, "im", "im:x")
	im.blocking = true
	imQueued := newOrigin(h.rec, "imq", "im:x")
	imQueued.blocking = true
	ws := newOrigin(h.rec, "ws", "ws:1")

	h.submit("m1", im, adm)
	h.rec.waitFor(t, "send:k:m1", 1)
	h.submit("m2", imQueued, adm)
	h.submit("m3", ws, adm)
	release()
	h.rec.waitFor(t, "begin:ws:head", 1)
	release()
	h.rec.waitFor(t, "idle", 1)
	adm.wg.Wait()

	h.rec.assertOrder(t, "after:k", "finish:im:done", "finish:ws:done", "after:k", "finish:imq:done")
	if n := h.rec.count("after:k"); n != 2 {
		t.Fatalf("AfterTurn ran %d times, want once per turn (2)", n)
	}
}

// TestOwnerLoop_OutcomeStages: a GetOrCreate failure is StageSession with no
// Send and no AfterTurn; a Send failure is StageSend with the error; success
// is StageDone with the session and result.
func TestOwnerLoop_OutcomeStages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		getErr    error
		sendErr   error
		wantStage Stage
		wantSend  bool
	}{
		{"session", errBoom, nil, StageSession, false},
		{"send", nil, errBoom, StageSend, true},
		{"done", nil, nil, StageDone, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(8, ModeCollect)
			h.s.getErr, h.s.sendErr = tc.getErr, tc.sendErr
			a := newOrigin(h.rec, "a", "ws:a")
			h.submit("m1", a, &fakeAdmission{rec: h.rec})

			got := a.finished()
			if len(got) != 1 {
				t.Fatalf("finished %d times, want 1", len(got))
			}
			out := got[0]
			if out.Stage != tc.wantStage || out.Panic {
				t.Fatalf("outcome = %+v, want stage %v", out, tc.wantStage)
			}
			if (len(h.s.sendCalls()) == 1) != tc.wantSend || (h.rec.count("after:k") == 1) != tc.wantSend {
				t.Fatalf("Send/AfterTurn ran = %v/%v, want %v", len(h.s.sendCalls()), h.rec.count("after:k"), tc.wantSend)
			}
			switch tc.wantStage {
			case StageSession:
				if out.Err != errBoom || out.Sess != nil {
					t.Fatalf("session-stage outcome = %+v", out)
				}
				if n := h.rec.count("ready:a:0"); n != 0 {
					t.Fatal("SessionReady ran after GetOrCreate failed")
				}
			case StageSend:
				if out.Err != errBoom || out.Sess == nil || out.Result != nil {
					t.Fatalf("send-stage outcome = %+v", out)
				}
			case StageDone:
				if out.Err != nil || out.Sess == nil || out.Result == nil || out.Result.Text != "re:m1" {
					t.Fatalf("done outcome = %+v", out)
				}
			}
		})
	}
}

// TestOwnerLoop_HookOrder: Begin and BeforeSession precede GetOrCreate,
// SessionReady follows it and precedes Send, Finish comes last, and the
// owner loop's NotifyIdle runs after everything.
func TestOwnerLoop_HookOrder(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModeCollect)
	h.s.status = sessionview.SessionNew
	h.submit("m1", newOrigin(h.rec, "a", "ws:a"), &fakeAdmission{rec: h.rec})
	h.rec.assertOrder(t, "opts:a", "begin:a:head", "before:a", "get:k", "ready:a:2", "send:k:m1", "finish:a:done", "after:k", "idle")
	if h.s.sendCalls()[0].onEvent != nil {
		t.Fatal("no receiver returned a callback but Send got a non-nil onEvent")
	}
}

// TestOwnerLoop_SessionOptsFromOwnerEveryTurn (#3004 分叉 26): every turn
// asks the owner, never a queued origin, for the session options.
func TestOwnerLoop_SessionOptsFromOwnerEveryTurn(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModeCollect)
	release := h.hold()
	adm := &fakeAdmission{rec: h.rec, async: true}
	a := newOrigin(h.rec, "a", "ws:a")
	a.opts.Model = "owner-model"
	b := newOrigin(h.rec, "b", "ws:b")
	b.opts.Model = "queued-model"

	h.submit("m1", a, adm)
	h.rec.waitFor(t, "send:k:m1", 1)
	h.submit("m2", b, adm)
	release()
	h.rec.waitFor(t, "send:k:m2", 1)
	release()
	h.rec.waitFor(t, "idle", 1)
	adm.wg.Wait()

	if h.rec.count("opts:a") != 2 || h.rec.count("opts:b") != 0 {
		t.Fatalf("SessionOpts calls: a=%d b=%d, want a=2 b=0", h.rec.count("opts:a"), h.rec.count("opts:b"))
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	for i, o := range h.s.opts {
		if o.Model != "owner-model" {
			t.Fatalf("turn %d GetOrCreate opts = %+v, want the owner's", i, o)
		}
	}
}

// TestOwnerLoop_EventCallbacksReachEveryReceiver: every receiver's
// SessionReady callback sees the turn's events.
func TestOwnerLoop_EventCallbacksReachEveryReceiver(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModeCollect)
	release := h.hold()
	adm := &fakeAdmission{rec: h.rec, async: true}
	a, b, c := newOrigin(h.rec, "a", "ws:a"), newOrigin(h.rec, "b", "ws:b"), newOrigin(h.rec, "c", "ws:c")
	for _, o := range []*fakeOrigin{a, b, c} {
		o.onEvent = func(ev clievent.Event) { h.rec.add("event:%s:%s", o.name, ev.Type) }
	}

	h.submit("m1", a, adm)
	h.rec.waitFor(t, "send:k:m1", 1)
	h.submit("m2", b, adm)
	h.submit("m3", c, adm)
	release()
	release()
	h.rec.waitFor(t, "idle", 1)
	adm.wg.Wait()

	// The drain turn has three receivers: b, c and the observing owner.
	for name, want := range map[string]int{"a": 2, "b": 1, "c": 1} {
		if got := h.rec.count("event:" + name + ":assistant"); got != want {
			t.Fatalf("%s saw %d events, want %d: %v", name, got, want, h.rec.snapshot())
		}
	}
}

// TestOwnerLoop_ShutdownDropsQueued: when the loop's ctx ends, the messages
// still queued see DropShutdown, ownership is released, and NotifyIdle runs.
func TestOwnerLoop_ShutdownDropsQueued(t *testing.T) {
	t.Parallel()
	rec := newRecorder()
	// A collect delay that never fires, so the select can only take ctx.Done.
	q := NewQueueWithMode(8, time.Hour, ModeCollect)
	s := newSender(rec)
	o := New(q, s)
	gate := make(chan struct{})
	s.gate = gate
	ctx, cancel := context.WithCancel(context.Background())
	adm := &fakeAdmission{rec: rec, async: true, ctx: ctx}

	o.Submit(context.Background(), Request{Key: "k", Text: "m1", Origin: newOrigin(rec, "a", "ws:a")}, adm)
	rec.waitFor(t, "send:k:m1", 1)
	o.Submit(context.Background(), Request{Key: "k", Text: "m2", Origin: newOrigin(rec, "b", "ws:b")}, adm)
	cancel()
	gate <- struct{}{}
	rec.waitFor(t, "idle", 1)
	adm.wg.Wait()

	rec.assertOrder(t, "finish:a:done", "dropped:b:shutdown", "idle")
	if isOwner, _, _, _, _ := q.Enqueue("k", Msg{Text: "next"}); !isOwner {
		t.Fatal("ownership not released after shutdown")
	}
}

// TestOwnerLoop_ResetStopsTheStaleOwner: after Reset bumps the generation,
// the running owner exits after its turn instead of draining, and the next
// request owns the key afresh.
func TestOwnerLoop_ResetStopsTheStaleOwner(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModeCollect)
	release := h.hold()
	adm := &fakeAdmission{rec: h.rec, async: true}

	h.submit("m1", newOrigin(h.rec, "a", "ws:a"), adm)
	h.rec.waitFor(t, "send:k:m1", 1)
	h.submit("m2", newOrigin(h.rec, "b", "ws:b"), adm)
	h.o.Reset(context.Background(), "k", false)
	if ack := h.submit("m3", newOrigin(h.rec, "c", "ws:c"), adm); ack != AckOwner {
		t.Fatalf("Submit after Reset = %v, want AckOwner", ack)
	}
	release()
	release()
	h.rec.waitFor(t, "idle", 2)
	adm.wg.Wait()

	if got, want := h.s.texts(), []string{"m1", "m3"}; !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v (the reset batch must not be drained)", got, want)
	}
	if h.rec.count("dropped:b:reset") != 1 {
		t.Fatalf("queued origin not told of the reset: %v", h.rec.snapshot())
	}
}

// TestOwnerLoop_NilOriginIsSilent: requests with no Origin (enqueued straight
// into the Queue, or submitted without one) take part in turns but receive
// nothing, and nothing panics.
func TestOwnerLoop_NilOriginIsSilent(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModeCollect)
	release := h.hold()
	adm := &fakeAdmission{rec: h.rec, async: true}
	owner, b := newOrigin(h.rec, "a", "ws:a"), newOrigin(h.rec, "b", "ws:b")

	h.submit("m1", owner, adm)
	h.rec.waitFor(t, "send:k:m1", 1)
	h.q.Enqueue("k", Msg{Text: "direct"})
	h.submit("m2", b, adm)
	release()
	h.rec.waitFor(t, "begin:b:head", 1)
	release()
	h.rec.waitFor(t, "idle", 1)
	adm.wg.Wait()

	if infos := b.turnInfos(); len(infos) != 1 || infos[0].Merged != 2 || len(infos[0].Mates) != 0 {
		t.Fatalf("b TurnInfo = %+v, want head of a 2-request batch with no Mates", infos)
	}
	if h.rec.count("begin:a:observer") != 1 {
		t.Fatal("owner did not observe the batch")
	}

	// No Origin at all: the turn runs with zero options and no delivery.
	h2 := newHarness(8, ModeCollect)
	if ack := h2.submit("solo", nil, &fakeAdmission{rec: h2.rec}); ack != AckOwner {
		t.Fatalf("nil-origin Submit = %v", ack)
	}
	h2.rec.assertOrder(t, "get:k", "send:k:solo", "after:k", "idle")
	h2.q.Enqueue("k", Msg{Text: "owner"})
	h2.q.Enqueue("k", Msg{Text: "queued"})
	h2.o.Reset(context.Background(), "k", true)
	h2.rec.assertOrder(t, "discardPending:k:session reset", "reset:k:true")
}

// TestOwnerLoop_PanicInFirstTurn: the loop's recover keeps the panic from
// reaching the Admission, counts it, tells the owner, releases the key, and
// only then reports idle.
func TestOwnerLoop_PanicInFirstTurn(t *testing.T) {
	h := newHarness(8, ModeCollect)
	h.s.panicIf = func(string) bool { return true }
	a := newOrigin(h.rec, "a", "ws:a")
	before := metrics.PanicRecoveredTotal.Value()

	h.submit("m1", a, &fakeAdmission{rec: h.rec})

	if d := metrics.PanicRecoveredTotal.Value() - before; d != 1 {
		t.Fatalf("PanicRecoveredTotal moved by %d, want 1", d)
	}
	h.rec.assertOrder(t, "send:k:m1", "finish:a:send+panic", "idle")
	if n := h.rec.count("after:k"); n != 0 {
		t.Fatalf("AfterTurn ran %d times on a turn that panicked in Send", n)
	}
	if isOwner, _, _, _, _ := h.q.Enqueue("k", Msg{}); !isOwner {
		t.Fatal("key still owned after the panic")
	}
}

// TestOwnerLoop_PanicInDrainTurn (#3004 分叉 12): a panic in a drain turn
// tells every member of that batch and the owner (as Observer), drops what
// is still queued, does not re-tell the finished first turn, and NotifyIdle
// runs after all of it.
func TestOwnerLoop_PanicInDrainTurn(t *testing.T) {
	h := newHarness(8, ModeCollect)
	h.s.panicIf = func(text string) bool { return text != "m1" }
	release := h.hold()
	adm := &fakeAdmission{rec: h.rec, async: true}
	a := newOrigin(h.rec, "a", "ws:a")
	b, c, d := newOrigin(h.rec, "b", "ws:b"), newOrigin(h.rec, "c", "ws:c"), newOrigin(h.rec, "d", "ws:d")
	before := metrics.PanicRecoveredTotal.Value()

	h.submit("m1", a, adm)
	h.rec.waitFor(t, "send:k:m1", 1)
	h.submit("m2", b, adm)
	h.submit("m3", c, adm)
	release()
	h.rec.waitFor(t, "begin:a:observer", 1)
	h.submit("m4", d, adm)
	release()
	h.rec.waitFor(t, "idle", 1)
	adm.wg.Wait()

	if delta := metrics.PanicRecoveredTotal.Value() - before; delta != 1 {
		t.Fatalf("PanicRecoveredTotal moved by %d, want 1", delta)
	}
	for _, ev := range []string{"finish:b:send+panic", "finish:c:send+panic", "finish:a:send+panic", "dropped:d:panic"} {
		if h.rec.count(ev) != 1 {
			t.Fatalf("missing %s: %v", ev, h.rec.snapshot())
		}
		h.rec.assertOrder(t, ev, "idle")
	}
	h.rec.assertOrder(t, "dropped:d:panic", "finish:b:send+panic")
	if got := a.finished(); len(got) != 2 || got[0].Panic || !got[1].Panic {
		t.Fatalf("owner outcomes = %+v, want [first turn done, observer panic]", got)
	}
}

// TestOwnerLoop_PanicInAHook: a receiver whose Finish or Begin panics is not
// called again, and the receivers after it are still told.
func TestOwnerLoop_PanicInAHook(t *testing.T) {
	for _, hook := range []string{"finish", "begin"} {
		t.Run(hook, func(t *testing.T) {
			h := newHarness(8, ModeCollect)
			release := h.hold()
			adm := &fakeAdmission{rec: h.rec, async: true}
			b, c := newOrigin(h.rec, "b", "ws:b"), newOrigin(h.rec, "c", "ws:c")
			b.panicIn = hook

			h.submit("m1", newOrigin(h.rec, "a", "ws:a"), adm)
			h.rec.waitFor(t, "send:k:m1", 1)
			h.submit("m2", b, adm)
			h.submit("m3", c, adm)
			release()
			if hook == "finish" {
				release()
			}
			h.rec.waitFor(t, "idle", 1)
			adm.wg.Wait()

			if n := h.rec.count("begin:b:head"); n != 1 {
				t.Fatalf("b begun %d times, want 1", n)
			}
			finishes := 0
			for _, e := range h.rec.snapshot() {
				if strings.HasPrefix(e, "finish:b:") {
					finishes++
				}
			}
			if want := map[string]int{"finish": 1, "begin": 0}[hook]; finishes != want {
				t.Fatalf("b finished %d times, want %d", finishes, want)
			}
			wantC := map[string]string{"finish": "finish:c:done+panic", "begin": "finish:c:session+panic"}[hook]
			if h.rec.count(wantC) != 1 {
				t.Fatalf("c not told (%s): %v", wantC, h.rec.snapshot())
			}
		})
	}
}
