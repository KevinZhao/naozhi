package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/routerrelay"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// The server binds both of the router's notification slots, and a retired key
// reaches the dashboard's retired-session ledger through the relay; the send
// queued on it is told the key was removed (#3297).
func TestServer_BindsRouterEvents(t *testing.T) {
	relay := &routerrelay.Relay{}
	stateDir := t.TempDir()
	srv, hs := buildServerWithHandlers(ServerOptions{
		Addr:      ":0",
		Router:    session.NewRouter(session.RouterConfig{Observer: relay}),
		Platforms: map[string]platform.Platform{"test": &mockPlatform{}},
		Backend:   "claude",
		StateDir:  stateDir,
		Queue:     QueueOptions{MaxDepth: 4},
		Relays: RelayOptions{
			Router: relay,
		},
	})

	const key = "feishu:direct:u:general"
	turns, ctx := hs.wiring.turns, context.Background()
	if ack := turns.Submit(ctx, turn.Request{Key: key, Text: "first"}, neverRunAdmission{}); ack != turn.AckOwner {
		t.Fatalf("precondition: first request ack %d, want AckOwner", ack)
	}
	t.Cleanup(srv.hub.Shutdown)
	c, out := newCapturedClient(t, srv.hub)
	queued := turn.Request{Key: key, Text: "queued", Origin: hs.wiring.engine.wsOrigin(c, "w2", key)}
	if ack := turns.Submit(ctx, queued, neverRunAdmission{}); ack != turn.AckQueued {
		t.Fatalf("precondition: second request ack %d, want AckQueued behind the owner", ack)
	}
	relay.KeyRetired(key, "sid-retired")
	select {
	case msg := <-out:
		if msg.Type != "send_ack" || msg.ID != "w2" || msg.Error != removedSendMsg {
			t.Errorf("the queued send was answered %+v, want the removed-session error ack", msg)
		}
	case <-time.After(parityWait):
		t.Error("a key retired through the relay left its queued send unanswered")
	}
	// The retirement reached the Orchestrator's Retire: the key is free.
	if ack := turns.Submit(ctx, turn.Request{Key: key, Text: "after"}, neverRunAdmission{}); ack != turn.AckOwner {
		t.Errorf("a key retired through the relay kept its owner: next request ack %d, want AckOwner", ack)
	}
	turns.Retire(ctx, key)
	hs.sessionH.FlushRetiredStore()
	store, err := buildRetiredStoreWithErr(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if store.Get("sid-retired") == 0 {
		t.Error("a key retired through the relay did not reach the retired-session ledger")
	}

	for name, rebind := range map[string]func(){
		"SessionsChanged": func() { relay.BindSessionsChanged(func() {}) },
		"KeyRetired":      func() { relay.BindKeyRetired(func(string, string) {}) },
	} {
		func() {
			defer func() {
				if p, _ := recover().(string); !strings.Contains(p, "bound twice") {
					t.Errorf("%s: the server left the slot unbound", name)
				}
			}()
			rebind()
		}()
	}
}
