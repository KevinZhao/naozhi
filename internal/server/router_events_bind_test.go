package server

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/routerrelay"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// The server binds both of the router's notification slots, and a retired key
// reaches the dashboard's retired-session ledger through the relay.
func TestServer_BindsRouterEvents(t *testing.T) {
	relay := &routerrelay.Relay{}
	stateDir := t.TempDir()
	_, hs := buildServerWithHandlers(ServerOptions{
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
	if isOwner, _, _, _, _ := hs.wiring.msgQueue.Enqueue(key, turn.Msg{Text: "first"}); !isOwner {
		t.Fatal("precondition: first Enqueue did not become owner")
	}
	if isOwner, enqueued, _, _, _ := hs.wiring.msgQueue.Enqueue(key, turn.Msg{Text: "queued"}); isOwner || !enqueued {
		t.Fatalf("precondition: second Enqueue not queued behind owner (isOwner=%v enqueued=%v), want depth 1", isOwner, enqueued)
	}
	relay.KeyRetired(key, "sid-retired")
	if kept := hs.wiring.msgQueue.DiscardAndReturn(key); kept != nil {
		t.Errorf("a key retired through the relay kept %d queued messages", len(kept))
	}
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
