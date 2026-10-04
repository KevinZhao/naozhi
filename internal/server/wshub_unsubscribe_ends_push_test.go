package server

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// TestUnsubscribe_EndsTheParkedPushLoop: unsubscribe closes the push loop's
// notify channel, which sends the loop into resubscribeEvents as if its
// process had gone away. With the session still answering, that loop must
// give up rather than install a fresh subscription and keep pushing the key's
// events to a client that dropped it.
func TestUnsubscribe_EndsTheParkedPushLoop(t *testing.T) {
	hub, router := newTestHub(t, "")
	defer hub.Shutdown()
	hub.resubscribeInterval = time.Millisecond
	const key = "test:d:u:general"
	proc := session.NewTestProcess()
	router.InjectSession(key, proc)
	c := newTestWSClient()
	hub.register(c)
	loopGen := subscribeTest(hub, c, key, func() {})

	hub.handleUnsubscribe(c, node.ClientMsg{Type: "unsubscribe", Key: key})

	// The loop still holds the generation it was started with.
	var notify <-chan struct{}
	if ok, _ := hub.resubscribeEvents(c, key, loopGen, &notify); ok {
		t.Fatal("the push loop resubscribed a key the client unsubscribed from")
	}
	if n := proc.EventLog.SubscriberCount(); n != 0 {
		t.Errorf("the EventLog holds %d subscriptions after unsubscribe", n)
	}
	if isSubscribed(hub, c, key) {
		t.Error("the registry holds the key again after unsubscribe")
	}
}

// TestFailedResubscribe_NotFound_EndsTheParkedPushLoop: re-subscribing to a
// held key closes the old push loop's notify, parking it. When the session is
// gone the client is told "session not found"; a session created under the key
// afterwards must not let that loop install itself and push to the client.
func TestFailedResubscribe_NotFound_EndsTheParkedPushLoop(t *testing.T) {
	hub, router := newTestHub(t, "")
	defer hub.Shutdown()
	hub.resubscribeInterval = time.Millisecond
	const key = "test:d:u:general"
	router.InjectSession(key, session.NewTestProcess())
	c := newTestWSClient()
	hub.register(c)
	loopGen := subscribeTest(hub, c, key, func() {})

	router.Remove(key)
	hub.handleSubscribe(c, node.ClientMsg{Type: "subscribe", Key: key})
	if msg := readClientMsg(t, c, 2*time.Second); msg.Error != "session not found" {
		t.Fatalf("reply = %+v, want session not found", msg)
	}
	proc := session.NewTestProcess()
	router.InjectSession(key, proc)

	var notify <-chan struct{}
	if ok, _ := hub.resubscribeEvents(c, key, loopGen, &notify); ok {
		t.Fatal("the push loop resubscribed a key the client was told does not exist")
	}
	if n := proc.EventLog.SubscriberCount(); n != 0 {
		t.Errorf("the new session's EventLog holds %d subscriptions", n)
	}
	if isSubscribed(hub, c, key) {
		t.Error("the registry holds the key again after the not-found reply")
	}
}

// TestFailedResubscribe_Suspended_EndsTheParkedPushLoop: the same re-subscribe
// finding a session with no process is answered "subscribed, suspended" with
// nothing installed. The old loop must end there too; the client subscribes
// again itself once the session runs.
func TestFailedResubscribe_Suspended_EndsTheParkedPushLoop(t *testing.T) {
	hub, router := newTestHub(t, "")
	defer hub.Shutdown()
	hub.resubscribeInterval = time.Millisecond
	const key = "test:d:u:general"
	router.InjectSession(key, session.NewTestProcess())
	c := newTestWSClient()
	hub.register(c)
	loopGen := subscribeTest(hub, c, key, func() {})

	router.InjectSession(key, nil)
	hub.handleSubscribe(c, node.ClientMsg{Type: "subscribe", Key: key})
	if msg := readClientMsg(t, c, 2*time.Second); msg.Type != "subscribed" || msg.Reason != "suspended" {
		t.Fatalf("reply = %+v, want subscribed reason=suspended", msg)
	}
	proc := session.NewTestProcess()
	router.InjectSession(key, proc)

	var notify <-chan struct{}
	if ok, _ := hub.resubscribeEvents(c, key, loopGen, &notify); ok {
		t.Fatal("the push loop revived a subscription the suspended reply left uninstalled")
	}
	if n := proc.EventLog.SubscriberCount(); n != 0 {
		t.Errorf("the resumed session's EventLog holds %d subscriptions", n)
	}
	if isSubscribed(hub, c, key) {
		t.Error("the registry holds the key again after the suspended reply")
	}
}
