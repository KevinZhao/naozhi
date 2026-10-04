package session

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session/backendstore"
)

// twoBackendRouter has claude (the router default) and kiro, so a test can
// tell which tier chose the backend.
func twoBackendRouter() *Router {
	r := &Router{ss: newSessionTable(), defaultCWD: "/default/ws"}
	r.setWrappersForTest(map[string]*cli.Wrapper{
		"claude": cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "claude"),
		"kiro":   cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "kiro"),
	})
	r.editBackendsForTest(func(c *backendstore.Config) { c.DefaultBackend = "claude" })
	stateOf(r).picks.backend = make(map[string]string)
	stateOf(r).picks.accessProfile = make(map[string]string)
	return r
}

// agents[].backend (AgentOpts.DefaultBackend) ranks below opts.Backend, the
// dashboard pick and the dead session's backend, and above the router
// default. Ranking it with opts.Backend would make the picker a no-op for the
// agent and respawn a resumable session on another CLI.
func TestResolveSpawnParams_AgentDefaultBackend(t *testing.T) {
	const key = "feishu:direct:bob:reviewer"
	cases := []struct {
		name       string
		opts       AgentOpts
		pick       string
		hasOld     bool // a dead session on key recorded oldBackend
		oldBackend string
		want       string
	}{
		{name: "no tier", want: "claude"},
		{name: "agent backend", opts: AgentOpts{DefaultBackend: "kiro"}, want: "kiro"},
		{name: "unknown agent backend falls back", opts: AgentOpts{DefaultBackend: "gemini"}, want: "claude"},
		{name: "explicit backend wins", opts: AgentOpts{Backend: "claude", DefaultBackend: "kiro"}, want: "claude"},
		{name: "dashboard pick wins", opts: AgentOpts{DefaultBackend: "kiro"}, pick: "claude", want: "claude"},
		{name: "resume continuity wins", opts: AgentOpts{DefaultBackend: "kiro"}, hasOld: true, oldBackend: "claude", want: "claude"},
		{name: "old session without backend", opts: AgentOpts{DefaultBackend: "kiro"}, hasOld: true, want: "kiro"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := twoBackendRouter()
			if tc.pick != "" {
				stateOf(r).picks.backend[key] = tc.pick
			}
			if tc.hasOld {
				old := &ManagedSession{key: key}
				old.SetBackend(tc.oldBackend)
				putT(r, key, old)
			}
			sp := resolveT(r, key, "", tc.opts)
			if sp.BackendID != tc.want {
				t.Errorf("BackendID = %q, want %q", sp.BackendID, tc.want)
			}
		})
	}
}

// End to end through GetOrCreate and Takeover: a new session of a kiro-pinned
// agent runs on kiro, but a takeover adopts an external Claude CLI and must
// not --resume it on the agent's backend.
func TestAgentDefaultBackend_SpawnAndTakeover(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	r := NewRouter(RouterConfig{
		MaxProcs: 4,
		BackendRuntimes: map[string]BackendRuntime{
			"claude": {Wrapper: cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")},
			"kiro":   {Wrapper: cli.NewWrapper("/nonexistent/kiro", &cli.ClaudeProtocol{}, "kiro")},
		},
		DefaultBackend: "claude",
	})
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) { return newIdleProc(), nil }
	t.Cleanup(r.Shutdown)
	opts := AgentOpts{DefaultBackend: "kiro"}

	s, _, err := r.GetOrCreate(context.Background(), "feishu:direct:spawn:reviewer", opts)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if got := s.Backend(); got != "kiro" {
		t.Errorf("GetOrCreate backend = %q, want the agent's kiro", got)
	}

	took, err := r.Takeover(context.Background(), "feishu:direct:adopt:reviewer", "sess-external", t.TempDir(), opts)
	if err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	if got := took.Backend(); got != "claude" {
		t.Errorf("Takeover backend = %q, want claude (the external CLI's)", got)
	}
}

// The resolver hands an agent key its agent's backend, but a planner's
// backend is its project's on every path: ResolveForPlannerKey (restart,
// resume) cannot see defaults["general"].
func TestKeyResolver_AgentDefaultBackend(t *testing.T) {
	ds := &fakeDataSource{
		byChat: map[string]ProjectBinding{
			"feishu:group:pinned":   {Bound: true, Name: "p1", WorkspaceDir: "/w/p1", Backend: "claude"},
			"feishu:group:unpinned": {Bound: true, Name: "p2", WorkspaceDir: "/w/p2"},
		},
		byName: map[string]ProjectBinding{
			"p1": {Bound: true, Name: "p1", WorkspaceDir: "/w/p1", Backend: "claude"},
			"p2": {Bound: true, Name: "p2", WorkspaceDir: "/w/p2"},
		},
	}
	agents := map[string]AgentOpts{"general": {DefaultBackend: "kiro"}, "coder": {DefaultBackend: "kiro"}}
	r := NewKeyResolver(agents, ds)

	if _, opts := r.ResolveForChat("feishu", "direct", "bob", "coder"); opts.DefaultBackend != "kiro" || opts.Backend != "" {
		t.Errorf("unbound chat: Backend=%q DefaultBackend=%q, want \"\"/kiro", opts.Backend, opts.DefaultBackend)
	}
	if opts, ok := r.ResolveForKey("dashboard:direct:1-x:coder"); !ok || opts.DefaultBackend != "kiro" {
		t.Errorf("dashboard key: DefaultBackend=%q ok=%v, want kiro", opts.DefaultBackend, ok)
	}
	// The project pin is explicit, so it outranks the agent tier at spawn.
	if _, opts := r.ResolveForChat("feishu", "group", "pinned", "coder"); opts.Backend != "claude" {
		t.Errorf("bound non-general: Backend=%q, want the project's claude", opts.Backend)
	}

	for _, tc := range []struct{ chatID, want string }{{"pinned", "claude"}, {"unpinned", ""}} {
		t.Run("planner "+tc.chatID, func(t *testing.T) {
			key, chat := r.ResolveForChat("feishu", "group", tc.chatID, "general")
			if !isPlannerKey(key) {
				t.Fatalf("ResolveForChat key = %q, want a planner key", key)
			}
			resume, ok := r.ResolveForKey(key)
			if !ok {
				t.Fatalf("ResolveForKey(%q) not found", key)
			}
			for path, o := range map[string]AgentOpts{"ResolveForChat": chat, "ResolveForKey": resume} {
				if o.Backend != tc.want || o.DefaultBackend != "" {
					t.Errorf("%s: Backend=%q DefaultBackend=%q, want %q/\"\"", path, o.Backend, o.DefaultBackend, tc.want)
				}
			}
		})
	}
}
