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
	hub, router := newTestHub("")
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
