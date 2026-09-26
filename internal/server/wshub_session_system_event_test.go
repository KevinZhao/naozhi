package server

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
)

func recvMsg(t *testing.T, out <-chan node.ServerMsg) (node.ServerMsg, bool) {
	t.Helper()
	select {
	case msg := <-out:
		return msg, true
	case <-time.After(time.Second):
		return node.ServerMsg{}, false
	}
}

// recvNone asserts none of the channels delivers a frame within a single
// shared window. broadcastSessionSystemEvent fans out synchronously, but each
// captured client relays send→out through a goroutine, so a short grace
// period is needed for a wrongly-sent frame to surface; sharing one window
// across all channels keeps the negative path O(window), not O(channels).
func recvNone(t *testing.T, outs ...<-chan node.ServerMsg) {
	t.Helper()
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		for i, out := range outs {
			select {
			case msg := <-out:
				t.Fatalf("channel %d must not receive a frame, got %+v", i, msg)
			default:
			}
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestBroadcastSessionSystemEvent_ReachesSubscribers verifies R176-ARCH-NX
// (#433) parity: a remote-send failure fans out to every dashboard subscribed
// to the session key as a `system` event, not just the originating tab.
func TestBroadcastSessionSystemEvent_ReachesSubscribers(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	sub, subOut := newCapturedClient(t, hub)
	registerSub(hub, sub, "feishu:p2p:alice")

	hub.broadcastSessionSystemEvent("feishu:p2p:alice", "发送失败：remote down")

	msg, ok := recvMsg(t, subOut)
	if !ok {
		t.Fatal("subscriber received no frame")
	}
	if msg.Type != "event" {
		t.Fatalf("Type = %q, want event", msg.Type)
	}
	if msg.Key != "feishu:p2p:alice" {
		t.Fatalf("Key = %q, want feishu:p2p:alice", msg.Key)
	}
	if msg.Event == nil {
		t.Fatal("Event is nil")
	}
	if msg.Event.Type != "system" {
		t.Fatalf("Event.Type = %q, want system", msg.Event.Type)
	}
	if msg.Event.Summary != "发送失败：remote down" {
		t.Fatalf("Event.Summary = %q", msg.Event.Summary)
	}
	if msg.Event.Time == 0 {
		t.Fatal("Event.Time should be stamped")
	}
}

// TestBroadcastSessionSystemEvent_SkipsNonSubscribers ensures the failure is
// scoped to the session's subscribers — a tab watching a different session
// must not receive cross-tenant noise.
func TestBroadcastSessionSystemEvent_SkipsNonSubscribers(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	other, otherOut := newCapturedClient(t, hub)
	registerSub(hub, other, "feishu:p2p:bob")

	hub.broadcastSessionSystemEvent("feishu:p2p:alice", "发送失败：remote down")

	recvNone(t, otherOut)
}

// TestBroadcastSessionSystemEvent_NoSubscribersNoop verifies the
// snapshot-before-marshal fast path: a failure on a session nobody is watching
// must not deliver anything (and must not panic). The single connected client
// is subscribed to a DIFFERENT key, so the target key has zero subscribers.
func TestBroadcastSessionSystemEvent_NoSubscribersNoop(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	c, out := newCapturedClient(t, hub)
	registerSub(hub, c, "feishu:p2p:elsewhere")

	hub.broadcastSessionSystemEvent("node1:p2p:unwatched", "发送失败：x")

	recvNone(t, out)
}

// TestBroadcastSessionSystemEvent_ZeroCountFastPath verifies R202606g-PERF-003
// (#2308): when the key's lock-free count is zero the broadcast
// returns before touching the snapshot pool. We assert observable behaviour —
// nothing is delivered — even though a client is wired into the Hub maps for a
// DIFFERENT key, so the target key's fast count stays absent/zero.
func TestBroadcastSessionSystemEvent_ZeroCountFastPath(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	// A live, authenticated client subscribed elsewhere; the target key has no
	// count entry, exercising the fast path.
	c, out := newCapturedClient(t, hub)
	registerSub(hub, c, "feishu:p2p:elsewhere")

	hub.broadcastSessionSystemEvent("feishu:p2p:zero", "发送失败：x")

	recvNone(t, out)
}

// TestBroadcastSessionSystemEvent_MultipleSubscribers verifies every client
// subscribed to the key receives the system event.
func TestBroadcastSessionSystemEvent_MultipleSubscribers(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	const key = "feishu:p2p:alice"
	outs := make([]<-chan node.ServerMsg, 0, 3)
	for i := 0; i < 3; i++ {
		c, out := newCapturedClient(t, hub)
		registerSub(hub, c, key)
		outs = append(outs, out)
	}
	// A subscriber on a different key must stay silent.
	other, otherOut := newCapturedClient(t, hub)
	registerSub(hub, other, "feishu:p2p:bob")

	hub.broadcastSessionSystemEvent(key, "发送失败：remote down")

	for i, out := range outs {
		msg, ok := recvMsg(t, out)
		if !ok {
			t.Fatalf("subscriber %d received no frame", i)
		}
		if msg.Type != "event" || msg.Key != key || msg.Event == nil || msg.Event.Type != "system" {
			t.Fatalf("subscriber %d got unexpected frame: %+v", i, msg)
		}
	}
	recvNone(t, otherOut)
}

// TestBroadcastSessionSystemEvent_ConcurrentChurn runs the fan-out under the
// race detector while subscribed clients are unregistered and re-registered
// beside it. A client leaving mid-snapshot must not corrupt state or trip the
// detector. The test asserts no panic / no race; delivery counts are
// nondeterministic by design so they are not checked.
func TestBroadcastSessionSystemEvent_ConcurrentChurn(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	const key = "feishu:p2p:churn"
	done := make(chan struct{})
	var wg sync.WaitGroup

	// drain continuously empties a captured client's output so SendRaw never
	// overflows the 64-deep buffer (which would trip the test helper's
	// non-idempotent done-close on the slow-client path).
	drain := func(out <-chan node.ServerMsg) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-out:
				case <-done:
					return
				}
			}
		}()
	}

	// Seed a stable subscriber so there is always something to deliver to.
	stable, stableOut := newCapturedClient(t, hub)
	registerSub(hub, stable, key)
	drain(stableOut)

	churn := make([]*wsClient, 8)
	for i := range churn {
		c, out := newCapturedClient(t, hub)
		registerSub(hub, c, key)
		drain(out)
		churn[i] = c
	}

	// Broadcaster: hammer the two-phase fan-out.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				hub.broadcastSessionSystemEvent(key, "发送失败：x")
			}
		}
	}()

	// Churner: unregister and re-register subscribed clients, as disconnects
	// and reconnects do.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				for _, c := range churn {
					hub.subs.remove(c)
					registerSub(hub, c, key)
				}
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(done)
	wg.Wait()
}

// TestBroadcastSessionSystemEvent_FullKeyAmongManyClients: a key at its
// subscriber cap, among many more clients subscribed elsewhere, reaches every
// one of its subscribers and none of the others.
func TestBroadcastSessionSystemEvent_FullKeyAmongManyClients(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	const key = "feishu:p2p:chunk"
	outs := make([]<-chan node.ServerMsg, 0, maxSubscribersPerKey)
	for i := 0; i < maxSubscribersPerKey; i++ {
		c, out := newCapturedClient(t, hub)
		registerSub(hub, c, key)
		outs = append(outs, out)
	}
	const otherCount = 3 * maxSubscribersPerKey
	otherOuts := make([]<-chan node.ServerMsg, 0, otherCount)
	for i := 0; i < otherCount; i++ {
		c, out := newCapturedClient(t, hub)
		registerSub(hub, c, "feishu:p2p:other"+strconv.Itoa(i%3))
		otherOuts = append(otherOuts, out)
	}

	hub.broadcastSessionSystemEvent(key, "发送失败：remote down")

	for i, out := range outs {
		msg, ok := recvMsg(t, out)
		if !ok {
			t.Fatalf("subscriber %d received no frame", i)
		}
		if msg.Type != "event" || msg.Key != key || msg.Event == nil || msg.Event.Type != "system" {
			t.Fatalf("subscriber %d got unexpected frame: %+v", i, msg)
		}
	}
	recvNone(t, otherOuts...)
}

// TestBroadcastSessionSystemEvent_EmptyArgsNoop guards the early return so an
// empty key or summary cannot emit a malformed frame.
func TestBroadcastSessionSystemEvent_EmptyArgsNoop(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	sub, subOut := newCapturedClient(t, hub)
	registerSub(hub, sub, "feishu:p2p:alice")

	hub.broadcastSessionSystemEvent("", "发送失败：x")
	hub.broadcastSessionSystemEvent("feishu:p2p:alice", "")

	recvNone(t, subOut)
}
