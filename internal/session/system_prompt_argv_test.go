package session

// system_prompt_argv_test.go — cross-seam regression tests for #2493.
//
// The bug survived because every existing test stopped at one side of the
// AgentOpts → resolveSpawnParamsLocked → BuildArgs seam: upstream tests
// asserted the prompt was placed in AgentOpts.ExtraArgs, downstream tests
// asserted `--append-system-prompt` in ExtraArgs MUST be stripped. Both were
// green while the prompt never reached argv. The tests here run the
// production chain end to end and assert on the final argv.
//
// TestSystemPrompt_PlannerPath_ReachesArgv and
// TestSystemPrompt_ScratchPath_ReachesArgv use only APIs that predate the fix
// (KeyResolver.ResolveForChat / ScratchPool.Open / resolveSpawnParamsLocked /
// BuildArgs) and fail on the pre-fix tree. Verified against 49bffb15 (the
// master this fix was branched from) with only the argvSpawnOptions call
// adapted to its 4-arg signature: both failed with `argv values = []`, and
// the spawn logged `ExtraArgs contained denied flags; stripped dropped=2`.

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// mkSystemPromptRouter builds the minimal Router resolveSpawnParamsLocked
// needs, defaulting to the claude backend (the only one that renders the
// prompt).
func mkSystemPromptRouter(t *testing.T) *Router {
	t.Helper()
	r := &Router{
		ss:         sessionStore{sessions: make(map[string]*ManagedSession)},
		defaultCWD: "/default/ws",
	}
	r.bkStore.wrappers = map[string]*cli.Wrapper{
		"claude": cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "claude"),
		"kiro":   cli.NewWrapper("/bin/false", &cli.ACPProtocol{BackendID: "kiro"}, "kiro"),
	}
	r.bkStore.defaultBackend = "claude"
	r.bkStore.backendOverrides = make(map[string]string)
	r.claudeDir = t.TempDir()
	r.kiroSessionsDir = t.TempDir()
	return r
}

// spawnArgvFor reproduces exactly what spawnSession hands to the shim for a
// fresh session: the production resolver feeding the production argv
// constructor feeding the backend's BuildArgs.
func spawnArgvFor(r *Router, key string, opts AgentOpts) []string {
	sp := r.resolveSpawnParamsLocked(key, "", opts)
	return sp.Wrapper.Protocol.BuildArgs(
		r.argvSpawnOptions(sp.Model, sp.Effort, r.cliDebugFileFor(key), sp.SystemPrompt, sp.Args))
}

// appendSystemPromptValues returns every value following an
// `--append-system-prompt` token.
func appendSystemPromptValues(args []string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--append-system-prompt" {
			out = append(out, args[i+1])
		}
	}
	return out
}

// TestSystemPrompt_AgentOpts_ReachesArgv is the direct seam test the issue
// asked for: AgentOpts.SystemPrompt → resolveSpawnParamsLocked → BuildArgs
// must put the prompt in the final argv, exactly once.
func TestSystemPrompt_AgentOpts_ReachesArgv(t *testing.T) {
	t.Parallel()
	r := mkSystemPromptRouter(t)
	const prompt = "You are a code review expert."

	args := spawnArgvFor(r, "feishu:direct:alice:reviewer", AgentOpts{SystemPrompt: prompt})

	if got := appendSystemPromptValues(args); !slices.Equal(got, []string{prompt}) {
		t.Fatalf("argv --append-system-prompt values = %q, want [%q]\nargv=%v", got, prompt, args)
	}
}

// TestSystemPrompt_ExtraArgsRouteStillStripped pins the other half of the
// contract: the denylist is intact, so a prompt that (wrongly) travels via
// ExtraArgs still never reaches argv. This is what makes SystemPrompt the ONLY
// channel and keeps the R219-SEC-1 injection guard meaningful.
func TestSystemPrompt_ExtraArgsRouteStillStripped(t *testing.T) {
	t.Parallel()
	r := mkSystemPromptRouter(t)

	args := spawnArgvFor(r, "feishu:direct:alice:general", AgentOpts{
		ExtraArgs: []string{"--append-system-prompt", "smuggled"},
	})

	if slices.Contains(args, "--append-system-prompt") || slices.Contains(args, "smuggled") {
		t.Fatalf("ExtraArgs route must stay denylisted, got argv=%v", args)
	}
}

// TestSystemPrompt_PlannerPath_ReachesArgv follows the chat-view planner path
// (ResolveForChat with a bound project) all the way to argv. On origin/master
// the prompt lands in ExtraArgs and BuildArgs strips it: values == nil.
//
// It also fixes the stacking order: agents[general].system_prompt first, the
// project planner prompt after, "\n\n"-joined, one flag.
func TestSystemPrompt_PlannerPath_ReachesArgv(t *testing.T) {
	t.Parallel()
	r := mkSystemPromptRouter(t)
	defaults := map[string]AgentOpts{
		"general": {Model: "sonnet", SystemPrompt: "AGENT", ExtraArgs: []string{"--keep"}},
	}
	res := NewKeyResolver(defaults, &fakeDataSource{
		byChat: map[string]ProjectBinding{
			"feishu:group:c1": {Bound: true, Name: "proj", WorkspaceDir: t.TempDir(), PlannerPrompt: "PLAN"},
		},
	})

	key, opts := res.ResolveForChat("feishu", "group", "c1", "general")
	args := spawnArgvFor(r, key, opts)

	if got := appendSystemPromptValues(args); !slices.Equal(got, []string{"AGENT\n\nPLAN"}) {
		t.Fatalf("planner path: argv values = %q, want [\"AGENT\\n\\nPLAN\"]\nargv=%v", got, args)
	}
	if !slices.Contains(args, "--keep") {
		t.Errorf("agent ExtraArgs lost on the planner path: %v", args)
	}
	// The registry entry must be untouched — layering copies, never mutates.
	if defaults["general"].SystemPrompt != "AGENT" {
		t.Errorf("ResolveForChat mutated the agent registry SystemPrompt: %q", defaults["general"].SystemPrompt)
	}
}

// TestSystemPrompt_PlannerRestartPath_ReachesArgv covers the administrative
// planner-restart route (ResolveForPlannerKey), which starts from blank opts
// so the planner prompt is the sole layer.
func TestSystemPrompt_PlannerRestartPath_ReachesArgv(t *testing.T) {
	t.Parallel()
	r := mkSystemPromptRouter(t)
	res := NewKeyResolver(map[string]AgentOpts{"general": {SystemPrompt: "AGENT"}}, &fakeDataSource{
		byName: map[string]ProjectBinding{
			"proj": {Bound: true, Name: "proj", WorkspaceDir: t.TempDir(), PlannerPrompt: "PLAN"},
		},
	})

	key, opts, ok := res.ResolveForPlannerKey("proj")
	if !ok {
		t.Fatal("ResolveForPlannerKey: ok=false")
	}
	args := spawnArgvFor(r, key, opts)
	if got := appendSystemPromptValues(args); !slices.Equal(got, []string{"PLAN"}) {
		t.Fatalf("planner-restart path: argv values = %q, want [\"PLAN\"] (no defaults inheritance)\nargv=%v", got, args)
	}
}

// TestSystemPrompt_ScratchPath_ReachesArgv follows ScratchPool.Open →
// OptsForKey → spawn argv. On origin/master the quoted context is appended
// to ExtraArgs and stripped: the aside CLI never saw the quote.
//
// Stacking: the source agent's own system prompt comes first, the scratch
// quote block after, and the registry value handed in as BaseOpts is left
// untouched.
func TestSystemPrompt_ScratchPath_ReachesArgv(t *testing.T) {
	t.Parallel()
	r := mkSystemPromptRouter(t)
	p := NewScratchPool(nil, 5, time.Minute)
	base := AgentOpts{Model: "opus", SystemPrompt: "AGENT", ExtraArgs: []string{"--keep"}}
	baseBefore := base
	baseBefore.ExtraArgs = slices.Clone(base.ExtraArgs)

	sc, err := p.Open(OpenOptions{
		SourceKey: "feishu:direct:alice:general",
		AgentID:   "general",
		Quote:     "what does this mean?",
		BaseOpts:  base,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	opts, ok := p.OptsForKey(sc.Key)
	if !ok {
		t.Fatal("OptsForKey miss")
	}
	args := spawnArgvFor(r, sc.Key, opts)

	got := appendSystemPromptValues(args)
	if len(got) != 1 {
		t.Fatalf("scratch path: want exactly one --append-system-prompt, got %d\nargv=%v", len(got), args)
	}
	if !strings.HasPrefix(got[0], "AGENT\n\n") {
		t.Errorf("agent prompt must come first, got %q", got[0])
	}
	if !strings.Contains(got[0], "<selected_quote>\nwhat does this mean?") {
		t.Errorf("scratch quote block missing from argv prompt: %q", got[0])
	}
	if !slices.Contains(args, "--keep") {
		t.Errorf("agent ExtraArgs lost on the scratch path: %v", args)
	}
	if !reflect.DeepEqual(base, baseBefore) {
		t.Errorf("ScratchPool.Open mutated the caller's BaseOpts: %+v", base)
	}
}

// TestSystemPrompt_ScratchWithoutAgentPrompt pins the no-base case of the
// stacking rule: a lone layer must not grow a leading separator.
func TestSystemPrompt_ScratchWithoutAgentPrompt(t *testing.T) {
	t.Parallel()
	p := NewScratchPool(nil, 5, time.Minute)
	sc, err := p.Open(OpenOptions{SourceKey: "feishu:direct:alice:general", Quote: "q"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if strings.HasPrefix(sc.BaseOpts.SystemPrompt, "\n") {
		t.Errorf("lone scratch layer has a stray leading separator: %q", sc.BaseOpts.SystemPrompt[:8])
	}
}

// TestJoinSystemPrompts is the table for the single stacking rule.
func TestJoinSystemPrompts(t *testing.T) {
	t.Parallel()
	cases := []struct{ base, extra, want string }{
		{"", "", ""},
		{"A", "", "A"},
		{"", "B", "B"},
		{"A", "B", "A\n\nB"},
		{"A\n\nB", "C", "A\n\nB\n\nC"}, // re-joining keeps order
	}
	for _, tc := range cases {
		if got := JoinSystemPrompts(tc.base, tc.extra); got != tc.want {
			t.Errorf("JoinSystemPrompts(%q, %q) = %q, want %q", tc.base, tc.extra, got, tc.want)
		}
	}
}

// TestSystemPrompt_NoDriftRestart guards the highest-consequence side effect
// of rendering the prompt into argv: on restart the drift check re-merges the
// overlay the shim persisted (shim.SpawnOverlay.AppendSystemPrompt, #2494
// plumbing) against current config, so a prompted session must compare
// equal — otherwise every planner / scratch / agents[].system_prompt session
// would be restarted on each naozhi restart. Both sides run production code
// (spawnShimState → shimArgsDrift).
func TestSystemPrompt_NoDriftRestart(t *testing.T) {
	t.Parallel()
	r := mkSystemPromptRouter(t)
	key := "project:proj:planner"
	sess := newSessionWithID(key, "sess-sp-1")
	sess.SetBackend("claude")
	r.ss.sessions[key] = sess

	state, sp := spawnShimState(t, r, key, "", AgentOpts{
		Backend: "claude", SystemPrompt: "AGENT\n\nPLAN", Exempt: true, Workspace: t.TempDir(),
	})
	if got := appendSystemPromptValues(state.CLIArgs); !slices.Equal(got, []string{"AGENT\n\nPLAN"}) {
		t.Fatalf("premise broken: prompt not in spawn argv: %v", state.CLIArgs)
	}
	if sp.Overlay.AppendSystemPrompt != "AGENT\n\nPLAN" || state.SpawnOverlay.AppendSystemPrompt != "AGENT\n\nPLAN" {
		t.Fatalf("prompt not recorded in the spawn overlay: %+v", sp.Overlay)
	}

	wrapper, backendID := r.wrapperFor(state.Backend)
	if drift, stored, current := r.shimArgsDrift(wrapper, backendID, state, sess); drift {
		t.Fatalf("prompted session misread as arg-drift — every naozhi restart would kill it\n"+
			"  stored:  %v\n  current: %v", stored, current)
	}

	// A genuinely changed prompt (operator edited agents[].system_prompt or
	// the project planner prompt) is a real config change and MUST read as
	// drift — the overlay records what was requested, not a free pass.
	changed := state
	ov := *state.SpawnOverlay
	ov.AppendSystemPrompt = "AGENT\n\nPLAN v2"
	changed.SpawnOverlay = &ov
	if drift, _, _ := r.shimArgsDrift(wrapper, backendID, changed, sess); !drift {
		t.Fatal("changed system prompt not detected as drift")
	}
}

// TestSystemPrompt_IgnoredOnACPBackend documents that kiro sessions get no
// prompt (kiro's acp has no equivalent flag) — the field is dropped, not
// mis-rendered, and the spawn is otherwise unaffected.
func TestSystemPrompt_IgnoredOnACPBackend(t *testing.T) {
	t.Parallel()
	r := mkSystemPromptRouter(t)
	args := spawnArgvFor(r, "feishu:direct:alice:general", AgentOpts{Backend: "kiro", SystemPrompt: "P"})
	if slices.Contains(args, "--append-system-prompt") || slices.Contains(args, "P") {
		t.Errorf("ACP backend must ignore SystemPrompt, got %v", args)
	}
	if args[0] != "acp" {
		t.Errorf("expected kiro acp argv, got %v", args)
	}
}
