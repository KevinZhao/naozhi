package server

import (
	"strconv"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
)

// TestSubscriberCount_KeyAbsentIffNoSubscriber: a key is tracked exactly while
// it has a subscriber, so the per-key cap check stays O(1) and the index is
// bounded by keys currently watched. Only the last subscriber leaving reports
// the key emptied — the signal that drops its marshal cache slot.
func TestSubscriberCount_KeyAbsentIffNoSubscriber(t *testing.T) {
	h := &Hub{subs: newSubscriberRegistry()}
	a := &wsClient{done: make(chan struct{})}
	b := &wsClient{done: make(chan struct{})}
	registerSub(h, a, "k")
	registerSub(h, b, "k")
	if n := subscriberCountOf(h, "k"); n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}

	if emptied := h.subs.unsubscribe(a, "k", 0); emptied {
		t.Error("unsubscribe reported the key emptied while b still subscribes")
	}
	if n := subscriberCountOf(h, "k"); n != 1 {
		t.Fatalf("count = %d after one unsubscribe, want 1", n)
	}
	if emptied := h.subs.unsubscribe(a, "k", 0); emptied {
		t.Error("a second unsubscribe of the same key reported it emptied")
	}
	if n := subscriberCountOf(h, "k"); n != 1 {
		t.Fatalf("a repeated unsubscribe changed the count to %d", n)
	}
	if emptied := h.subs.unsubscribe(b, "k", 0); !emptied {
		t.Error("the last subscriber leaving did not report the key emptied")
	}
	if n := subscribedKeyCount(h); n != 0 {
		t.Errorf("%d keys tracked with no subscriber left", n)
	}
}

// TestSubscriberCount_RemoveReportsEmptiedKeys: unregistering a client hands
// back its closures and exactly the keys it was the last subscriber of.
func TestSubscriberCount_RemoveReportsEmptiedKeys(t *testing.T) {
	h := &Hub{subs: newSubscriberRegistry()}
	a := &wsClient{done: make(chan struct{})}
	b := &wsClient{done: make(chan struct{})}
	ran := 0
	registerSub(h, a, "")
	subscribeTest(h, a, "shared", func() { ran++ })
	subscribeTest(h, a, "solo", func() { ran++ })
	registerSub(h, b, "shared")

	unsubs, emptied, removed := h.subs.remove(a)
	if !removed {
		t.Fatal("remove of a registered client reported removed=false")
	}
	for _, u := range unsubs {
		u()
	}
	if ran != 2 {
		t.Errorf("remove handed back %d of a's 2 closures", ran)
	}
	if len(emptied) != 1 || emptied[0] != "solo" {
		t.Errorf("emptied = %v, want [solo]", emptied)
	}
	if subscriberCountOf(h, "shared") != 1 || subscriberCountOf(h, "solo") != 0 {
		t.Errorf("counts shared=%d solo=%d after remove, want 1/0", subscriberCountOf(h, "shared"), subscriberCountOf(h, "solo"))
	}
	if _, _, again := h.subs.remove(a); again {
		t.Error("a second remove reported removed=true")
	}
}

// TestHandleSubscribe_NotFoundReleasesTheReservation: the slot reserved
// before the session lookup is given back when there is no session.
func TestHandleSubscribe_NotFoundReleasesTheReservation(t *testing.T) {
	hub, _ := newTestHub("")
	defer hub.Shutdown()
	c := newTestWSClient()
	hub.register(c)

	hub.handleSubscribe(c, node.ClientMsg{Type: "subscribe", Key: "test:d:u:missing"})

	if msg := readClientMsg(t, c, 2*time.Second); msg.Error != "session not found" {
		t.Fatalf("reply = %+v, want session not found", msg)
	}
	if n := subscriptionCount(hub, c); n != 0 {
		t.Errorf("%d subscriptions left behind after the not-found path", n)
	}
	if n := subscriberCountOf(hub, "test:d:u:missing"); n != 0 {
		t.Errorf("key count %d left behind after the not-found path", n)
	}
}

// TestHandleSubscribe_AfterDrainIsIgnored: a subscribe racing Shutdown finds
// its client drained and takes no slot.
func TestHandleSubscribe_AfterDrainIsIgnored(t *testing.T) {
	hub, _ := newTestHub("")
	const key = "test:d:u:general"
	c := newTestWSClient()
	hub.register(c)
	subscribeTest(hub, c, "test:d:u:before", func() {})
	hub.Shutdown()
	if subscribedKeyCount(hub) != 0 || hub.subs.count("test:d:u:before") != 0 {
		t.Fatal("Shutdown left a key subscribed")
	}

	hub.handleSubscribe(c, node.ClientMsg{Type: "subscribe", Key: key})
	if subscribedKeyCount(hub) != 0 || isRegistered(hub, c) {
		t.Error("a subscribe after Shutdown re-populated the registry")
	}
	if n := authCount(hub); n != 0 {
		t.Errorf("%d clients still authenticated after Shutdown: a late broadcast would reach them", n)
	}
}

// TestRegistry_ResubscribeSameKeyReplacesTheOldSubscription: subscribing to a
// key the client already holds runs the old unsubscribe and keeps one slot.
func TestRegistry_ResubscribeSameKeyReplacesTheOldSubscription(t *testing.T) {
	h := &Hub{subs: newSubscriberRegistry()}
	c := &wsClient{done: make(chan struct{})}
	registerSub(h, c, "")
	oldRan := false
	subscribeTest(h, c, "k", func() { oldRan = true })
	subscribeTest(h, c, "k", func() {})
	if !oldRan {
		t.Error("re-subscribing did not run the previous subscription's unsubscribe")
	}
	if subscriberCountOf(h, "k") != 1 || subscriptionCount(h, c) != 1 {
		t.Errorf("re-subscribe counted twice: key=%d client=%d, want 1/1", subscriberCountOf(h, "k"), subscriptionCount(h, c))
	}
}

// TestRegistry_PerClientCap: a client holds at most maxSubscriptionsPerClient
// keys; re-subscribing to one it holds still works at the cap.
func TestRegistry_PerClientCap(t *testing.T) {
	r := newSubscriberRegistry()
	c := &wsClient{done: make(chan struct{})}
	r.add(c)
	for i := 0; i < maxSubscriptionsPerClient; i++ {
		if res := r.reserve(c, "k"+strconv.Itoa(i)); res != reserveOK {
			t.Fatalf("reserve %d refused below the cap: %v", i, res)
		}
	}
	if res := r.reserve(c, "one-more"); res != reserveClientFull {
		t.Fatalf("reserve past the cap = %v, want reserveClientFull", res)
	}
	if res := r.reserve(c, "k0"); res != reserveOK {
		t.Errorf("re-subscribe at the cap = %v, want reserveOK", res)
	}
}

// TestRegistry_UnsubscribeRunsTheClosure: an unsubscribe ends the EventLog
// subscription, not just the bookkeeping.
func TestRegistry_UnsubscribeRunsTheClosure(t *testing.T) {
	h := &Hub{subs: newSubscriberRegistry()}
	c := &wsClient{done: make(chan struct{})}
	registerSub(h, c, "")
	ran := false
	subscribeTest(h, c, "k", func() { ran = true })
	h.subs.unsubscribe(c, "k", 0)
	if !ran {
		t.Error("unsubscribe did not run the subscription's closure")
	}
}

// TestRegistry_InstallAfterTheSlotWasTakenRejoins: if the reserved slot was
// taken away before install (a stale loop's expire), the installed
// subscription is counted again, so the key's set and the client's keys agree.
func TestRegistry_InstallAfterTheSlotWasTakenRejoins(t *testing.T) {
	h := &Hub{subs: newSubscriberRegistry()}
	c := &wsClient{done: make(chan struct{})}
	registerSub(h, c, "")
	h.subs.reserve(c, "k")
	h.subs.expire(c, "k", 0)
	if _, ok := h.subs.install(c, "k", func() {}, func() bool { return true }); !ok {
		t.Fatal("install declined")
	}
	if !isSubscribed(h, c, "k") || subscriberCountOf(h, "k") != 1 || h.subs.count("k") != 1 {
		t.Errorf("after install: subscribed=%v set=%d fast=%d, want true/1/1",
			isSubscribed(h, c, "k"), subscriberCountOf(h, "k"), h.subs.count("k"))
	}
}

// TestShutdown_ReleasesConnectionSlots: the drained clients give their
// maxWSConns slots back; unregister no longer can, since they are gone.
func TestShutdown_ReleasesConnectionSlots(t *testing.T) {
	hub, _ := newTestHub("")
	for i := 0; i < 2; i++ {
		if !hub.admit.reserveConn() {
			t.Fatal("reserveConn refused")
		}
		hub.register(newTestWSClient())
	}
	hub.Shutdown()
	if n := hub.admit.conns.Load(); n != 0 {
		t.Errorf("%d connection slots still held after Shutdown", n)
	}
}

// TestHandleUnsubscribe_LastSubscriberDropsTheMarshalCache: the cached
// history frame goes with the key's last subscriber, not before.
func TestHandleUnsubscribe_LastSubscriberDropsTheMarshalCache(t *testing.T) {
	hub, _ := newTestHub("")
	defer hub.Shutdown()
	const key = "test:d:u:general"
	a, b := newTestWSClient(), newTestWSClient()
	registerSub(hub, a, key)
	registerSub(hub, b, key)
	hub.historyMarshalCache.slot(key)

	hub.handleUnsubscribe(a, node.ClientMsg{Type: "unsubscribe", Key: key})
	if _, ok := hub.historyMarshalCache.entries.Load(key); !ok {
		t.Fatal("cache slot dropped while another subscriber remains")
	}
	hub.handleUnsubscribe(b, node.ClientMsg{Type: "unsubscribe", Key: key})
	if _, ok := hub.historyMarshalCache.entries.Load(key); ok {
		t.Error("cache slot kept after the last subscriber left")
	}
}

// TestHandleAuth_TokenJoinsTheAuthenticatedSet: a client that authenticates
// with the token receives the "all authenticated" broadcasts from then on.
func TestHandleAuth_TokenJoinsTheAuthenticatedSet(t *testing.T) {
	hub, _ := newTestHub("secret")
	defer hub.Shutdown()
	c := &wsClient{hub: hub, send: make(chan []byte, 4), done: make(chan struct{})}
	hub.register(c)
	if authCount(hub) != 0 {
		t.Fatal("client authenticated before auth")
	}
	hub.handleAuth(c, node.ClientMsg{Type: "auth", Token: "secret"})
	ptr, snap := hub.snapshotAuthenticated()
	defer releaseBroadcastSnap(ptr, snap)
	if len(snap) != 1 || snap[0] != c {
		t.Errorf("authenticated set after token auth = %d clients, want exactly c", len(snap))
	}
}

// TestResubscribe_StaleLoopLeavesTheNewerSubscription: a loop parked on an
// old generation exits once a newer subscribe took the key, even while the
// session is gone, instead of waiting out its timeout and expiring the newer
// subscription.
func TestResubscribe_StaleLoopLeavesTheNewerSubscription(t *testing.T) {
	hub, _ := newTestHub("")
	defer hub.Shutdown()
	hub.resubscribeInterval = time.Millisecond
	const key = "test:d:u:gone"
	c := newTestWSClient()
	hub.register(c)
	oldGen := subscribeTest(hub, c, key, func() {})
	subscribeTest(hub, c, key, func() {}) // the newer subscription

	var notify <-chan struct{}
	if ok, _ := hub.resubscribeEvents(c, key, oldGen, &notify); ok {
		t.Fatal("a stale loop resubscribed")
	}
	if !isSubscribed(hub, c, key) {
		t.Error("the stale loop expired the newer subscription")
	}
	select {
	case raw := <-c.send:
		t.Errorf("the stale loop sent %s", raw)
	default:
	}
}

// TestResubscribe_TimeoutExpiresTheSubscription: with no process coming back
// the loop gives up, frees the slot (and the marshal cache with the last
// subscriber) and tells the client.
func TestResubscribe_TimeoutExpiresTheSubscription(t *testing.T) {
	hub, _ := newTestHub("")
	defer hub.Shutdown()
	hub.resubscribeInterval = time.Millisecond
	const key = "test:d:u:gone"
	c := newTestWSClient()
	hub.register(c)
	gen := subscribeTest(hub, c, key, func() {})
	hub.historyMarshalCache.slot(key)

	var notify <-chan struct{}
	if ok, _ := hub.resubscribeEvents(c, key, gen, &notify); ok {
		t.Fatal("resubscribed with no session")
	}
	if isSubscribed(hub, c, key) || subscriberCountOf(hub, key) != 0 {
		t.Error("the timed-out subscription still holds its slot")
	}
	if _, ok := hub.historyMarshalCache.entries.Load(key); ok {
		t.Error("the marshal cache slot survived its last subscriber timing out")
	}
	msg := readClientMsg(t, c, time.Second)
	if msg.Type != "session_state" || msg.Reason != "subscription_timeout" {
		t.Errorf("client got %+v, want session_state subscription_timeout", msg)
	}
}

// TestRegistry_RecycledSetsStartEmpty: a key's subscriber set reused after
// another key emptied it carries none of the old subscribers.
func TestRegistry_RecycledSetsStartEmpty(t *testing.T) {
	h := &Hub{subs: newSubscriberRegistry()}
	a := &wsClient{done: make(chan struct{})}
	b := &wsClient{done: make(chan struct{})}
	registerSub(h, a, "first")
	h.subs.unsubscribe(a, "first", 0)
	registerSub(h, b, "second")
	got := h.subs.subscribersOf("second", nil)
	if len(got) != 1 || got[0] != b {
		t.Fatalf("second's subscribers = %v, want only b", got)
	}
	// Leaving a key nobody holds must not poison the spare list.
	h.subs.mu.Lock()
	h.subs.leaveLocked(a, "never-joined")
	h.subs.mu.Unlock()
	registerSub(h, a, "third") // would panic on a recycled nil set
	if subscriberCountOf(h, "third") != 1 {
		t.Error("joining after a stray leave did not count")
	}
}
