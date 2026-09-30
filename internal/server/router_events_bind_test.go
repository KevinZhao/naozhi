package server

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/dispatch"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/routerrelay"
	"github.com/naozhi/naozhi/internal/session"
)

// The server binds both of the router's notification slots, and a retired key
// reaches the dashboard's retired-session ledger through the relay.
func TestServer_BindsRouterEvents(t *testing.T) {
	relay := &routerrelay.Relay{}
	stateDir := t.TempDir()
	_, hs := buildServerWithHandlers(ServerOptions{
		Addr:         ":0",
		Router:       session.NewRouter(session.RouterConfig{Observer: relay}),
		RouterEvents: relay,
		Platforms:    map[string]platform.Platform{"test": &mockPlatform{}},
		Backend:      "claude",
		StateDir:     stateDir,
		Queue:        QueueOptions{MaxDepth: 4},
	})

	const key = "feishu:direct:u:general"
	hs.wiring.msgQueue.Enqueue(key, dispatch.QueuedMsg{Text: "first"})
	hs.wiring.msgQueue.Enqueue(key, dispatch.QueuedMsg{Text: "queued"})
	if d := hs.wiring.msgQueue.Depth(key); d != 1 {
		t.Fatalf("precondition: depth %d, want 1", d)
	}
	relay.KeyRetired(key, "sid-retired")
	if d := hs.wiring.msgQueue.Depth(key); d != 0 {
		t.Errorf("a key retired through the relay kept %d queued messages", d)
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
