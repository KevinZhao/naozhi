package dispatch

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/turn"
)

// scopeMsg is an @mention in group chat "g" (or, with chatType "direct", a
// direct message in chat "g").
func scopeMsg(id, chatType, user, thread, text string) platform.IncomingMessage {
	return platform.IncomingMessage{
		Platform: "fake", EventID: "ev-" + id, MessageID: id, UserID: user,
		ChatID: "g", ChatType: chatType, MentionMe: true, ThreadID: thread, Text: text,
	}
}

func withGroupScope(s GroupScope) dispatcherTestOption {
	return func(cfg *testDispatcherConfig) { cfg.GroupScope = s }
}

// keyRecorder is a testSender that records the key of every session a turn
// asks for and every reset, and answers each turn "ok".
type keyRecorder struct {
	mu            sync.Mutex
	turns, resets []string
	turned        chan struct{} // one per recorded turn; nil = no signal
}

// waitTurns waits until n turns have been recorded.
func (k *keyRecorder) waitTurns(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-k.turned:
		case <-time.After(5 * time.Second):
			t.Fatalf("fewer than %d turns asked for a session", n)
		}
	}
}

func (k *keyRecorder) sender() *testSender {
	return &testSender{
		getOrCreate: func(_ context.Context, key string, _ session.AgentOpts) (turn.Session, session.SessionStatus, error) {
			k.mu.Lock()
			k.turns = append(k.turns, key)
			k.mu.Unlock()
			if k.turned != nil {
				k.turned <- struct{}{}
			}
			return fakeSession{}, session.SessionExisting, nil
		},
		reset: func(key string, _ bool) {
			k.mu.Lock()
			k.resets = append(k.resets, key)
			k.mu.Unlock()
		},
	}
}

func (k *keyRecorder) got() (turns, resets []string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.turns), slices.Clone(k.resets)
}

// TestGroupScope_SessionKeys: the session key a message routes to, by scope.
// Threads and members narrow only a group chat; a message outside any thread
// keeps the chat's key, and a direct chat is never narrowed.
func TestGroupScope_SessionKeys(t *testing.T) {
	msgs := []platform.IncomingMessage{
		scopeMsg("top", "group", "u1", "", "hi"),
		scopeMsg("t1", "group", "u1", "T1", "hi"),
		scopeMsg("t2", "group", "u2", "T2", "hi"),
		scopeMsg("dm", "direct", "u1", "T1", "hi"),
	}
	for _, tc := range []struct {
		scope GroupScope
		want  []string
	}{
		{"", []string{"fake:group:g:general", "fake:group:g#tT1:general", "fake:group:g#tT2:general", "fake:direct:g:general"}},
		{GroupScopeThread, []string{"fake:group:g:general", "fake:group:g#tT1:general", "fake:group:g#tT2:general", "fake:direct:g:general"}},
		{GroupScopeChat, []string{"fake:group:g:general", "fake:group:g:general", "fake:group:g:general", "fake:direct:g:general"}},
		{GroupScopeUser, []string{"fake:group:g#uu1:general", "fake:group:g#uu1:general", "fake:group:g#uu2:general", "fake:direct:g:general"}},
	} {
		t.Run(string(tc.scope), func(t *testing.T) {
			d := newTestDispatcher(&fakePlatform{}, withGroupScope(tc.scope))
			var got []string
			for _, m := range msgs {
				p, ok := d.prepareInbound(context.Background(), m)
				if !ok {
					t.Fatalf("prepareInbound dropped %+v", m)
				}
				got = append(got, p.key)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("keys = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGroupScope_ThreadCommandsActOnTheThread: /new and /urgent in a thread
// reset and run the thread's session, not the channel's; the same commands at
// the top level still act on the channel's. An /urgent turn runs detached, so
// its two turns may start in either order.
func TestGroupScope_ThreadCommandsActOnTheThread(t *testing.T) {
	rec := &keyRecorder{turned: make(chan struct{}, 4)}
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, withSender(rec.sender()))
	h := d.BuildHandler()
	ctx := context.Background()
	h(ctx, scopeMsg("1", "group", "u1", "T1", "/new"))
	h(ctx, scopeMsg("2", "group", "u1", "T1", "/urgent now"))
	h(ctx, scopeMsg("3", "group", "u1", "", "/new"))
	h(ctx, scopeMsg("4", "group", "u1", "", "/urgent now"))
	rec.waitTurns(t, 2)

	turns, resets := rec.got()
	slices.Sort(turns)
	if want := []string{"fake:group:g#tT1:general", "fake:group:g:general"}; !slices.Equal(resets, want) {
		t.Errorf("/new resets = %q, want %q", resets, want)
	}
	if want := []string{"fake:group:g#tT1:general", "fake:group:g:general"}; !slices.Equal(turns, want) {
		t.Errorf("/urgent turns = %q, want %q", turns, want)
	}
}

// TestGroupScope_StopInAThreadInterruptsOnlyThatThread: /stop probes the
// thread's sessions, not the channel's.
func TestGroupScope_StopInAThreadInterruptsOnlyThatThread(t *testing.T) {
	var mu sync.Mutex
	var probed []string
	d := newTestDispatcher(&fakePlatform{})
	d.router = &fakeSessionRouter{interruptViaControl: func(key string) session.InterruptOutcome {
		mu.Lock()
		probed = append(probed, key)
		mu.Unlock()
		return session.InterruptNoSession
	}}
	d.handleStopCommand(context.Background(), scopeMsg("1", "group", "u1", "T1", "/stop"), slog.Default())

	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(probed, "fake:group:g#tT1:general") {
		t.Errorf("/stop probed %q, want the thread's general session among them", probed)
	}
	for _, k := range probed {
		if !strings.HasPrefix(k, "fake:group:g#tT1:") {
			t.Errorf("/stop in thread T1 probed %q, outside the thread", k)
		}
	}
}

// TestGroupScope_CdInAThreadMovesTheChat: /cd and /pwd stay chat-level, so a
// /cd in a thread changes the workspace the whole channel and its threads use.
func TestGroupScope_CdInAThreadMovesTheChat(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakePlatform{}
	d := newTestDispatcher(fp)
	d.handleCdCommand(context.Background(), scopeMsg("1", "group", "u1", "T1", "/cd "+dir), "/cd "+dir, slog.Default())
	if !strings.Contains(fp.lastReply(), "已切换") {
		t.Fatalf("/cd reply = %q, want the workspace-changed reply", fp.lastReply())
	}
	if got := d.router.Workspace(session.ChatKey("fake", "group", "g")); got != dir {
		t.Errorf("channel workspace after /cd in a thread = %q, want %q", got, dir)
	}
}

// boundChatData binds exactly chat "g" (not a thread of it) to project demo.
type boundChatData struct{}

var demoBinding = session.ProjectBinding{Bound: true, Name: "demo", WorkspaceDir: "/proj"}

func (boundChatData) ProjectBinding(_, _, chatID string) session.ProjectBinding {
	if chatID == "g" {
		return demoBinding
	}
	return session.ProjectBinding{}
}

func (boundChatData) ProjectByName(name string) (session.ProjectBinding, bool) {
	return demoBinding, name == "demo"
}

// TestGroupScope_ProjectBoundThreadUsesThePlanner: in a project-bound chat a
// thread's general message goes to the project's one planner session, and
// the answer goes back to the thread.
func TestGroupScope_ProjectBoundThreadUsesThePlanner(t *testing.T) {
	rec := &keyRecorder{}
	fp := &fakePlatform{}
	d := newTestDispatcher(fp, withSender(rec.sender()), func(cfg *testDispatcherConfig) {
		cfg.Resolver = session.NewKeyResolver(map[string]session.AgentOpts{"general": {}}, boundChatData{})
	})
	d.BuildHandler()(context.Background(), scopeMsg("1", "group", "u1", "T1", "hello"))

	if turns, _ := rec.got(); !slices.Equal(turns, []string{"project:demo:planner"}) {
		t.Errorf("turn keys = %q, want the planner", turns)
	}
	var answered bool
	for _, r := range fp.replies {
		if r.Text != "" && strings.Contains(r.Text, "ok") {
			answered = true
			if r.ThreadID != "T1" {
				t.Errorf("answer %q went to thread %q, want T1", r.Text, r.ThreadID)
			}
		}
	}
	if !answered {
		t.Errorf("no answer among replies %+v", fp.replies)
	}
}

// TestGroupScope_ProjectBoundThreadNew: in a project-bound chat /new in a
// thread still resets the shared planner, and /new <agent> resets the
// thread's own session of that agent.
func TestGroupScope_ProjectBoundThreadNew(t *testing.T) {
	rec := &keyRecorder{}
	d := newTestDispatcher(&fakePlatform{}, withSender(rec.sender()), func(cfg *testDispatcherConfig) {
		cfg.AgentCommands = map[string]string{"review": "code-reviewer"}
		cfg.Resolver = session.NewKeyResolver(map[string]session.AgentOpts{"general": {}, "code-reviewer": {}}, boundChatData{})
	})
	h := d.BuildHandler()
	h(context.Background(), scopeMsg("1", "group", "u1", "T1", "/new"))
	h(context.Background(), scopeMsg("2", "group", "u1", "T1", "/new review"))

	if _, resets := rec.got(); !slices.Equal(resets, []string{"project:demo:planner", "fake:group:g#tT1:code-reviewer"}) {
		t.Errorf("resets = %q, want the planner then the thread's code-reviewer", resets)
	}
}

// TestGroupScope_OnlyChatSharedSessionsTakeOver: a first turn offers the
// chat's external CLI for takeover only on a session the whole chat shares
// (its own, or a project's planner); a thread's or member's session starts
// fresh instead of stopping the terminal CLI for every new thread.
func TestGroupScope_OnlyChatSharedSessionsTakeOver(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope GroupScope
		bound bool
		msgs  []platform.IncomingMessage
		want  []string
	}{
		{"thread", GroupScopeThread, false, []platform.IncomingMessage{
			scopeMsg("1", "group", "u1", "T1", "hi"), scopeMsg("2", "group", "u1", "", "hi"),
		}, []string{"fake:group:g:general"}},
		{"user", GroupScopeUser, false, []platform.IncomingMessage{scopeMsg("1", "group", "u1", "", "hi")}, nil},
		{"chat", GroupScopeChat, false, []platform.IncomingMessage{scopeMsg("1", "group", "u1", "T1", "hi")}, []string{"fake:group:g:general"}},
		{"planner", GroupScopeThread, true, []platform.IncomingMessage{scopeMsg("1", "group", "u1", "T1", "hi")}, []string{"project:demo:planner"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &keyRecorder{turned: make(chan struct{}, len(tc.msgs))}
			var mu sync.Mutex
			var offered []string
			d := newTestDispatcher(&fakePlatform{}, withSender(rec.sender()), withGroupScope(tc.scope), func(cfg *testDispatcherConfig) {
				cfg.Capabilities = fakeCapabilities{takeover: func(_ context.Context, chatKey, key string, _ session.AgentOpts) bool {
					mu.Lock()
					defer mu.Unlock()
					if chatKey != "fake:group:g" {
						t.Errorf("takeover of %q offered chat key %q, want the chat's", key, chatKey)
					}
					offered = append(offered, key)
					return false
				}}
				if tc.bound {
					cfg.Resolver = session.NewKeyResolver(map[string]session.AgentOpts{"general": {}}, boundChatData{})
				}
			})
			for _, m := range tc.msgs {
				d.BuildHandler()(context.Background(), m)
			}
			rec.waitTurns(t, len(tc.msgs))
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(offered, tc.want) {
				t.Errorf("takeover offered for %q, want %q", offered, tc.want)
			}
		})
	}
}
