package dispatch

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// tuningKeyRouter records the key every /model, /effort and /backend write
// goes to; it holds no sessions, so each pick is recorded for the next spawn.
type tuningKeyRouter struct {
	fakeSessionRouter
	mu   sync.Mutex
	keys []string
}

func (r *tuningKeyRouter) record(key string) {
	r.mu.Lock()
	r.keys = append(r.keys, key)
	r.mu.Unlock()
}

func (r *tuningKeyRouter) SetSessionTuning(_ context.Context, key string, _, _ *string) (string, error) {
	r.record(key)
	return "", nil
}

func (r *tuningKeyRouter) SetSessionBackend(key, _ string) { r.record(key) }

func (r *tuningKeyRouter) VisitSessions(func(session.SessionSnapshot) bool) {}

// TestTuningCommands_TargetTheSendersSession: /model, /effort and /backend
// write to the session the sender's turns route to, so a thread or member
// scope gets its own pick; direct and unscoped group chats keep the chat key,
// and a project-bound chat's threads share the planner.
func TestTuningCommands_TargetTheSendersSession(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts []dispatcherTestOption
		msg  platform.IncomingMessage
		want string
	}{
		{"thread scope in a thread", nil,
			scopeMsg("1", "group", "u1", "T1", ""), "fake:group:g#tT1:general"},
		{"thread scope at top level", nil,
			scopeMsg("1", "group", "u1", "", ""), "fake:group:g:general"},
		{"thread auto open in a thread", []dispatcherTestOption{withThreadAutoOpen()},
			scopeMsg("1", "group", "u1", "T1", ""), "fake:group:g#tT1:general"},
		{"user scope", []dispatcherTestOption{withGroupScope(GroupScopeUser)},
			scopeMsg("1", "group", "u1", "T1", ""), "fake:group:g#uu1:general"},
		{"chat scope", []dispatcherTestOption{withGroupScope(GroupScopeChat)},
			scopeMsg("1", "group", "u1", "T1", ""), "fake:group:g:general"},
		{"direct chat", []dispatcherTestOption{withGroupScope(GroupScopeUser)},
			scopeMsg("1", "direct", "u1", "", ""), "fake:direct:g:general"},
		{"project-bound thread", []dispatcherTestOption{func(c *testDispatcherConfig) {
			c.Resolver = session.NewKeyResolver(map[string]session.AgentOpts{"general": {}}, boundChatData{})
		}}, scopeMsg("1", "group", "u1", "T1", ""), "project:demo:planner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &tuningKeyRouter{}
			opts := append([]dispatcherTestOption{func(c *testDispatcherConfig) {
				c.Router = r
				c.Capabilities = fakeCapabilities{backendIDs: []string{"claude", "kiro"}}
			}}, tc.opts...)
			d := newTestDispatcher(&fakePlatform{}, opts...)
			for _, text := range []string{"/model opus", "/effort high", "/backend kiro"} {
				msg := tc.msg
				msg.Text = text
				if !d.dispatchCommand(context.Background(), msg, text, slog.Default()) {
					t.Fatalf("%q was not recognised as a command", text)
				}
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if len(r.keys) != 3 {
				t.Fatalf("recorded keys = %q, want one per command", r.keys)
			}
			for i, k := range r.keys {
				if k != tc.want {
					t.Errorf("command %d wrote to %q, want %q", i, k, tc.want)
				}
			}
		})
	}
}
