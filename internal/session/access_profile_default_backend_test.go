package session

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// access_profiles[].default_backend is the tier between agents[].backend and
// the router default, for a key with no session only. Every explicit statement
// (request, project pin, dashboard pick, resume continuity, agent config)
// outranks it.
func TestResolveSpawnParams_AccessProfileDefaultBackend(t *testing.T) {
	const key = "feishu:direct:bob:reviewer"
	profiles := map[string]AccessProfile{
		"viakiro":  {DefaultBackend: "kiro"},
		"viaghost": {DefaultBackend: "gemini"},
		"plain":    {DisplayName: "Plain"},
	}
	cases := []struct {
		name           string
		opts           AgentOpts
		defaultProfile string // RouterConfig.DefaultAccessProfile
		backendPick    string
		profilePick    string
		hasOld         bool // a dead session on key recorded oldBackend under viakiro
		oldBackend     string
		want           string
	}{
		{name: "profile pins kiro", opts: AgentOpts{AccessProfile: "viakiro"}, want: "kiro"},
		{name: "default access profile pins kiro", defaultProfile: "viakiro", want: "kiro"},
		{name: "profile without backend", opts: AgentOpts{AccessProfile: "plain"}, want: "claude"},
		{name: "agent backend wins", opts: AgentOpts{DefaultBackend: "claude", AccessProfile: "viakiro"}, want: "claude"},
		{name: "explicit backend wins", opts: AgentOpts{Backend: "claude", AccessProfile: "viakiro"}, want: "claude"},
		{name: "dashboard backend pick wins", opts: AgentOpts{AccessProfile: "viakiro"}, backendPick: "claude", want: "claude"},
		{name: "dashboard profile pick feeds the tier", profilePick: "viakiro", want: "kiro"},
		{name: "dashboard profile pick away from kiro", opts: AgentOpts{AccessProfile: "viakiro"}, profilePick: "plain", want: "claude"},
		{name: "resume continuity wins", opts: AgentOpts{AccessProfile: "viakiro"}, hasOld: true, oldBackend: "claude", want: "claude"},
		{name: "old session without backend", opts: AgentOpts{AccessProfile: "viakiro"}, hasOld: true, want: "claude"},
		{name: "unknown profile backend falls back", opts: AgentOpts{AccessProfile: "viaghost"}, want: "claude"},
		{name: "unknown profile id", opts: AgentOpts{AccessProfile: "ghost"}, want: "claude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := twoBackendRouter()
			setAccessProfiles(r, profiles)
			r.backends.defaultAccessProfile = tc.defaultProfile
			if tc.backendPick != "" {
				stateOf(r).picks.backend[key] = tc.backendPick
			}
			if tc.profilePick != "" {
				stateOf(r).picks.accessProfile[key] = tc.profilePick
			}
			if tc.hasOld {
				old := &ManagedSession{key: key}
				old.SetBackend(tc.oldBackend)
				old.SetAccessProfile("viakiro")
				putT(r, key, old)
			}
			sp := resolveT(r, key, "", tc.opts)
			if sp.BackendID != tc.want {
				t.Errorf("BackendID = %q, want %q", sp.BackendID, tc.want)
			}
		})
	}
}

// End to end through GetOrCreate, Takeover and RegisterForResume: a planner
// whose project names only a kiro-pinning profile spawns on kiro, a profile
// added at runtime takes effect without a restart, and neither an adopted
// external Claude CLI nor a history-pane resume moves onto the profile's
// backend.
func TestAccessProfileDefaultBackend_SpawnPaths(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	r := NewRouter(RouterConfig{
		MaxProcs: 4,
		BackendRuntimes: map[string]BackendRuntime{
			"claude": {Wrapper: cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")},
			"kiro":   {Wrapper: cli.NewWrapper("/nonexistent/kiro", &cli.ClaudeProtocol{}, "kiro")},
		},
		DefaultBackend:       "claude",
		AccessProfiles:       map[string]AccessProfile{"viakiro": {DefaultBackend: "kiro"}},
		DefaultAccessProfile: "viakiro",
	})
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) { return newIdleProc(), nil }
	t.Cleanup(r.Shutdown)
	ctx := context.Background()

	// Planner opts as the KeyResolver builds them for a project pinning only
	// an access profile: no Backend, no DefaultBackend.
	planner, _, err := r.GetOrCreate(ctx, "project:p1:planner", AgentOpts{Exempt: true, AccessProfile: "viakiro"})
	if err != nil {
		t.Fatalf("GetOrCreate planner: %v", err)
	}
	if got := planner.Backend(); got != "kiro" {
		t.Errorf("planner backend = %q, want the profile's kiro", got)
	}

	if err := r.backends.AddAccessProfile("later", AccessProfile{DefaultBackend: "claude"}); err != nil {
		t.Fatalf("AddAccessProfile: %v", err)
	}
	added, _, err := r.GetOrCreate(ctx, "feishu:direct:later:general", AgentOpts{AccessProfile: "later"})
	if err != nil {
		t.Fatalf("GetOrCreate under runtime profile: %v", err)
	}
	if got := added.Backend(); got != "claude" {
		t.Errorf("runtime-added profile backend = %q, want its claude over the default profile's kiro", got)
	}

	took, err := r.Takeover(ctx, "feishu:direct:adopt:general", "sess-external", t.TempDir(), AgentOpts{AccessProfile: "viakiro"})
	if err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	if got := took.Backend(); got != "claude" {
		t.Errorf("Takeover backend = %q, want claude (the external CLI's)", got)
	}

	const resumeKey = "dashboard:direct:rabc:general"
	r.RegisterForResume(resumeKey, "11111111-2222-3333-4444-555555555555", t.TempDir(), "")
	resumed, _, err := r.GetOrCreate(ctx, resumeKey, AgentOpts{})
	if err != nil {
		t.Fatalf("GetOrCreate after RegisterForResume: %v", err)
	}
	if got := resumed.Backend(); got != "claude" {
		t.Errorf("history resume backend = %q, want claude (the router default)", got)
	}
}
