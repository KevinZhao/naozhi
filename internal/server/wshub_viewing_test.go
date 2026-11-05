package server

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/planner/custody"
	"github.com/naozhi/naozhi/internal/session"
)

type fakeViewing struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeViewing) SetViewing(key, _ string, viewing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s=%v", key, viewing))
}

func (f *fakeViewing) DropViewer(string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "drop")
}

func (f *fakeViewing) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// TestWS_Viewing: a viewing frame reaches custody as the user at that
// session; switching, leaving and a remote node's session end it, the same
// session again renews it, and a closed connection drops the viewer.
func TestWS_Viewing(t *testing.T) {
	const a, b = "dashboard:direct:a:general", "dashboard:direct:b:general"
	for _, c := range []struct {
		name   string
		frames []node.ClientMsg
		auth   bool
		want   []string // before the connection closes
	}{
		{name: "open", auth: true, frames: []node.ClientMsg{{Key: a}}, want: []string{a + "=true"}},
		{name: "renew", auth: true, frames: []node.ClientMsg{{Key: a}, {Key: a}}, want: []string{a + "=true", a + "=true"}},
		{name: "switch", auth: true, frames: []node.ClientMsg{{Key: a}, {Key: b}}, want: []string{a + "=true", a + "=false", b + "=true"}},
		{name: "leave", auth: true, frames: []node.ClientMsg{{Key: a}, {}}, want: []string{a + "=true", a + "=false"}},
		{name: "local node", auth: true, frames: []node.ClientMsg{{Key: a, Node: "local"}}, want: []string{a + "=true"}},
		{name: "remote node", auth: true, frames: []node.ClientMsg{{Key: a}, {Key: a, Node: "mini"}}, want: []string{a + "=true", a + "=false"}},
		{name: "bad key", auth: true, frames: []node.ClientMsg{{Key: a}, {Key: "x\x00"}}, want: []string{a + "=true", a + "=false"}},
		{name: "not authenticated", frames: []node.ClientMsg{{Key: a}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sink := &fakeViewing{}
			hub := newHubForTest(t, HubOptions{Router: session.NewRouter(session.RouterConfig{}), DashToken: "secret",
				CookieMAC: testCookieMAC("secret"), Viewing: sink}, sendEngineOpts{})
			url, cleanup := startWSServer(t, hub)
			defer cleanup()
			conn := dialWS(t, url)
			if c.auth {
				wsWrite(t, conn, node.ClientMsg{Type: "auth", Token: "secret"})
				if resp := wsRead(t, conn); resp.Type != "auth_ok" {
					t.Fatalf("auth = %q", resp.Type)
				}
			}
			for _, f := range c.frames {
				f.Type = "viewing"
				wsWrite(t, conn, f)
			}
			// A ping's pong comes after every frame before it was handled.
			wsWrite(t, conn, node.ClientMsg{Type: "ping"})
			if resp := wsRead(t, conn); resp.Type != "pong" {
				t.Fatalf("ping = %q", resp.Type)
			}
			if got := sink.snapshot(); !slices.Equal(got, c.want) {
				t.Fatalf("calls = %v, want %v", got, c.want)
			}
			conn.Close()
			deadline := time.Now().Add(5 * time.Second)
			for !slices.Contains(sink.snapshot(), "drop") {
				if time.Now().After(deadline) {
					t.Fatalf("closed connection not dropped: %s", strings.Join(sink.snapshot(), " "))
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// TestHub_ViewingWiring: the Hub takes the viewing frames to the server's
// custody, and without custody holds no sink, not one wrapping a nil
// *custody.Engine (whose SetViewing would panic).
func TestHub_ViewingWiring(t *testing.T) {
	for _, c := range []struct {
		name    string
		custody *custody.Engine
	}{
		{name: "custody", custody: custody.New(custody.Config{})},
		{name: "no custody"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := NewWithOptions(ServerOptions{Addr: ":0", Router: session.NewRouter(session.RouterConfig{}), Custody: c.custody})
			t.Cleanup(srv.appCancel)
			if got, want := srv.hub.viewing != nil, c.custody != nil; got != want {
				t.Fatalf("hub has a viewing sink = %v, want %v", got, want)
			}
			if c.custody != nil && srv.hub.viewing.(*custody.Engine) != c.custody {
				t.Fatal("hub's viewing sink is not the server's custody")
			}
		})
	}
}
