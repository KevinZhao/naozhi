package turn

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// TestSubmit_AdmittedBeforeTheTurnStarts (#1963): Origin.Admitted runs after
// the Admission accepts and before start runs the turn, so with an inline
// Admission it still precedes every hook of the turn.
func TestSubmit_AdmittedBeforeTheTurnStarts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode Mode
		kind string
		ack  string
	}{
		{ModeCollect, "owner", "owner"},
		{ModePassthrough, "detached", "detached"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			h := newHarness(8, tc.mode)
			h.submit("m1", newOrigin(h.rec, "a", "im:x"), &fakeAdmission{rec: h.rec})
			h.rec.assertOrder(t, "admit:"+tc.kind, "admitted:a:"+tc.ack, "start:"+tc.kind, "begin:a:head", "finish:a:done")
		})
	}
}

// TestSubmit_QueuedAndDroppedAcks: a busy key queues (AckQueued) unless the
// queue is disabled (AckDropped); neither asks the Admission for a run.
func TestSubmit_QueuedAndDroppedAcks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		depth int
		want  Ack
	}{{8, AckQueued}, {0, AckDropped}} {
		t.Run(ackName(tc.want), func(t *testing.T) {
			t.Parallel()
			h := newHarness(tc.depth, ModeCollect)
			h.q.Enqueue("k", Msg{Text: "running"})
			b := newOrigin(h.rec, "b", "ws:b")
			if ack := h.submit("m2", b, &fakeAdmission{rec: h.rec}); ack != tc.want {
				t.Fatalf("Submit = %v, want %v", ack, tc.want)
			}
			if got := h.rec.snapshot(); len(got) != 1 || got[0] != "admitted:b:"+ackName(tc.want) {
				t.Fatalf("events = %v, want only the ack", got)
			}
		})
	}
}

// TestSubmit_DeclinedOwnerReleasesTheKey: when the Admission declines the
// owner run, the ack is AckShuttingDown and the key is free again.
func TestSubmit_DeclinedOwnerReleasesTheKey(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModeCollect)
	a := newOrigin(h.rec, "a", "ws:a")
	if ack := h.submit("m1", a, &fakeAdmission{rec: h.rec, decline: true}); ack != AckShuttingDown {
		t.Fatalf("Submit = %v, want AckShuttingDown", ack)
	}
	h.rec.assertOrder(t, "admit:owner", "admitted:a:shutting_down")
	if len(a.turnInfos()) != 0 {
		t.Fatal("a declined owner ran a turn")
	}
	if ack := h.submit("m2", newOrigin(h.rec, "b", "ws:b"), &fakeAdmission{rec: h.rec}); ack != AckOwner {
		t.Fatalf("Submit after a decline = %v, want AckOwner", ack)
	}
}

// TestSubmit_EvictionDropsTheEvictedOrigin (#3004 分叉 14): a full queue
// evicts its oldest message and tells that message's own origin.
func TestSubmit_EvictionDropsTheEvictedOrigin(t *testing.T) {
	t.Parallel()
	h := newHarness(1, ModeCollect)
	release := h.hold()
	adm := &fakeAdmission{rec: h.rec, async: true}
	b, c := newOrigin(h.rec, "b", "ws:b"), newOrigin(h.rec, "c", "ws:c")

	h.submit("m1", newOrigin(h.rec, "a", "ws:a"), adm)
	h.rec.waitFor(t, "send:k:m1", 1)
	h.submit("m2", b, adm)
	if ack := h.submit("m3", c, adm); ack != AckQueued {
		t.Fatalf("Submit into a full queue = %v, want AckQueued", ack)
	}
	h.rec.assertOrder(t, "admitted:b:queued", "dropped:b:evicted", "admitted:c:queued")
	if h.rec.count("dropped:c:evicted") != 0 {
		t.Fatal("the new message's origin was told of the eviction")
	}
	release()
	h.rec.waitFor(t, "send:k:m3", 1)
	release()
	h.rec.waitFor(t, "idle", 1)
	adm.wg.Wait()
	if len(b.turnInfos()) != 0 {
		t.Fatal("the evicted message got a turn")
	}
}

// TestSubmit_InterruptModeInterruptsOnce: ModeInterrupt's first follow-up of
// a turn interrupts it; later follow-ups of the same turn do not.
func TestSubmit_InterruptModeInterruptsOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModeInterrupt)
	h.s.interrupt = sessionview.InterruptSent
	release := h.hold()
	adm := &fakeAdmission{rec: h.rec, async: true}

	h.submit("m1", newOrigin(h.rec, "a", "ws:a"), adm)
	h.rec.waitFor(t, "send:k:m1", 1)
	h.submit("m2", newOrigin(h.rec, "b", "ws:b"), adm)
	h.submit("m3", newOrigin(h.rec, "c", "ws:c"), adm)
	if n := h.rec.count("interrupt:k"); n != 1 {
		t.Fatalf("Interrupt ran %d times, want 1", n)
	}
	h.rec.assertOrder(t, "interrupt:k", "admitted:b:queued")
	release()
	release()
	h.rec.waitFor(t, "idle", 1)
	adm.wg.Wait()
}

// TestReset_DropsThenDiscardsPendingThenResets (#2185): Reset tells each
// queued origin first, then fails pending passthrough sends, then resets.
func TestReset_DropsThenDiscardsPendingThenResets(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModeCollect)
	h.q.Enqueue("k", Msg{Text: "running"})
	h.submit("m2", newOrigin(h.rec, "b", "ws:b"), nil)
	h.submit("m3", newOrigin(h.rec, "c", "ws:c"), nil)

	h.o.Reset(context.Background(), "k", true)

	h.rec.assertOrder(t, "dropped:b:reset", "dropped:c:reset", "discardPending:k:session reset", "reset:k:true")
	if isOwner, _, _, _, _ := h.q.Enqueue("k", Msg{}); !isOwner {
		t.Fatal("key still owned after Reset")
	}
}

// TestReset_PanickingDroppedStillTellsTheRest: one origin's panic in Dropped
// does not stop the others being told, nor the reset itself.
func TestReset_PanickingDroppedStillTellsTheRest(t *testing.T) {
	h := newHarness(8, ModeCollect)
	h.q.Enqueue("k", Msg{Text: "running"})
	b := newOrigin(h.rec, "b", "ws:b")
	b.panicIn = "dropped"
	h.submit("m2", b, nil)
	h.submit("m3", newOrigin(h.rec, "c", "ws:c"), nil)

	h.o.Reset(context.Background(), "k", false)

	h.rec.assertOrder(t, "dropped:b:reset", "dropped:c:reset", "reset:k:false")
}

// TestShouldNotifyAndCleanup: both delegate to the Queue.
func TestShouldNotifyAndCleanup(t *testing.T) {
	t.Parallel()
	h := newHarness(8, ModeCollect)
	if !h.o.ShouldNotify("k") || h.o.ShouldNotify("k") {
		t.Fatal("ShouldNotify is not the Queue's 3s cooldown")
	}
	h.q.Enqueue("k", Msg{Text: "running"})
	h.o.Cleanup("k")
	if isOwner, _, _, gen, _ := h.q.Enqueue("k", Msg{}); !isOwner || gen != 0 {
		t.Fatalf("after Cleanup Enqueue = owner %v gen %d, want a fresh entry", isOwner, gen)
	}
}

// TestOrchestrator_ConcurrentSubmits: many concurrent requests on one key
// each get exactly one turn as a head, and the key ends idle. Run under
// -race; it also probes ShouldNotify and depth concurrently.
func TestOrchestrator_ConcurrentSubmits(t *testing.T) {
	t.Parallel()
	h := newHarness(1000, ModeCollect)
	adm := &fakeAdmission{rec: h.rec, async: true}
	const writers, each = 8, 20
	origins := make([][]*fakeOrigin, writers)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		for {
			select {
			case <-stop:
				return
			default:
				h.o.ShouldNotify("k")
				_ = h.q.depth("k")
			}
		}
	}()
	for w := range writers {
		origins[w] = make([]*fakeOrigin, each)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				o := newOrigin(h.rec, fmt.Sprintf("w%d-%d", w, i), fmt.Sprintf("ws:%d:%d", w, i))
				origins[w][i] = o
				h.submit(o.name, o, adm)
			}
		}()
	}
	wg.Wait()
	adm.wg.Wait()
	close(stop)
	<-probeDone

	for _, row := range origins {
		for _, o := range row {
			heads := 0
			for _, info := range o.turnInfos() {
				if info.Role == RoleHead {
					heads++
				}
			}
			if heads != 1 {
				t.Fatalf("%s was a head %d times, want 1", o.name, heads)
			}
		}
	}
	if isOwner, _, _, _, _ := h.q.Enqueue("k", Msg{}); !isOwner {
		t.Fatal("key still owned after every owner loop returned")
	}
}
