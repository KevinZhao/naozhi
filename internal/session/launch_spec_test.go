package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
)

// TestLaunchSpecDrift drives the production spawn and drift paths for each
// per-session launch field: the argv a shim recorded must rebuild from its
// (JSON round-tripped) overlay with no drift, and the same shim whose overlay
// lost the launch fields must read as drift — proving the overlay, not the
// global config, is what carries them.
func TestLaunchSpecDrift(t *testing.T) {
	r := mkOverlayRouter(t)
	r.spawn.sessionSettingsDir = t.TempDir()
	wrapper, backendID := r.backends.wrapperFor("claude")

	cases := []struct {
		name string
		opts AgentOpts
		want []string // argv tokens the spawn must carry
	}{
		{"default name only", AgentOpts{}, []string{"--name", "ws"}},
		{"tools", AgentOpts{Tools: []string{"Read", "Grep"}}, []string{"--tools", "Read,Grep"}},
		{"dontAsk", AgentOpts{PermissionMode: "dontAsk"}, []string{"--permission-mode", "dontAsk"}},
		{"auto", AgentOpts{PermissionMode: "auto"}, []string{"--permission-mode", "auto"}},
		{"name", AgentOpts{Name: "planner"}, []string{"--name", "planner"}},
		{"strict mcp", AgentOpts{StrictMCPConfig: true}, []string{"--strict-mcp-config"}},
		{"own mcp config", AgentOpts{StrictMCPConfig: true, MCPConfig: "/data/scout-mcp.json"},
			[]string{"--strict-mcp-config", "--mcp-config", "/data/scout-mcp.json"}},
		{"settings overrides", AgentOpts{CrossSessionInbound: "hold", PermissionsDeny: []string{"Bash"}}, []string{"--settings"}},
		{"planner shape", AgentOpts{
			Tools: []string{"Read", "Grep", "Glob", "ListAgents", "SendMessage"}, PermissionMode: "dontAsk",
			Name: "planner", StrictMCPConfig: true, CrossSessionInbound: "accept", SystemPrompt: "be brief",
		}, []string{"--permission-mode", "dontAsk", "--strict-mcp-config", "--settings", "--append-system-prompt"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := "dashboard:direct:launch-" + tc.name + ":general"
			s := newSessionWithID(key, "sess-launch")
			s.SetBackend("claude")
			putT(r, key, s)
			tc.opts.Backend, tc.opts.Workspace = "claude", "/ws"
			state, _ := spawnShimState(t, r, key, "", tc.opts)
			for _, tok := range tc.want {
				if !slices.Contains(state.CLIArgs, tok) {
					t.Fatalf("spawn argv lacks %q: %v", tok, state.CLIArgs)
				}
			}

			enc, err := shim.EncodeSpawnOverlay(state.SpawnOverlay)
			if err != nil {
				t.Fatal(err)
			}
			if state.SpawnOverlay, err = shim.DecodeSpawnOverlay(enc); err != nil {
				t.Fatal(err)
			}
			if drift, stored, current := driftArgsFor(r).shimArgsDrift(wrapper, backendID, state, s); drift {
				t.Fatalf("launch spec misread as drift\n  stored:  %v\n  current: %v", stored, current)
			}

			bare := *state.SpawnOverlay
			bare.Tools, bare.PermissionMode, bare.Name, bare.StrictMCPConfig, bare.MCPConfig = nil, "", "", false, ""
			bare.CrossSessionInbound, bare.PermissionsDeny, bare.PermissionsAllow = "", nil, nil
			state.SpawnOverlay = &bare
			if drift, _, _ := driftArgsFor(r).shimArgsDrift(wrapper, backendID, state, s); !drift {
				t.Fatal("overlay without the launch fields still matched: the drift rebuild is not reading them")
			}
		})
	}
}

// settingSourcesIndex is the index of argv's --setting-sources value.
func settingSourcesIndex(t *testing.T, argv []string) int {
	t.Helper()
	i := slices.Index(argv, "--setting-sources")
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("argv lacks --setting-sources: %v", argv)
	}
	return i + 1
}

// TestSettingSources: every session but planner:main loads its workspace's
// project sources on top of the base the settings mode picks.
func TestSettingSources(t *testing.T) {
	cases := []struct {
		key, settingsFile, want string
	}{
		{"dashboard:direct:ss:general", "/data/naozhi-settings.json", "project,local"},
		{"cron:job1", "/data/naozhi-settings.json", "project,local"},
		{"task:abc", "/data/naozhi-settings.json", "project,local"},
		{"planner:main", "/data/naozhi-settings.json", ""},
		{"dashboard:direct:ss:general", "", "user,project,local"},
		{"planner:main", "", "user"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"/"+tc.settingsFile, func(t *testing.T) {
			r := mkOverlayRouter(t)
			r.spawn.naozhiSettingsFile = tc.settingsFile
			state, _ := spawnShimState(t, r, tc.key, "", AgentOpts{Backend: "claude", Workspace: "/ws"})
			if got := state.CLIArgs[settingSourcesIndex(t, state.CLIArgs)]; got != tc.want {
				t.Errorf("--setting-sources = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestShimArgsDrift_ProjectSettingSources: a shim spawned before sessions
// loaded project settings is kept, not shut down as drifted; any other
// --setting-sources change still drifts.
func TestShimArgsDrift_ProjectSettingSources(t *testing.T) {
	cases := []struct {
		name, key, settingsFile, stored string
		drift                           bool
	}{
		{"naozhi settings, legacy", "dashboard:direct:ss:general", "/data/naozhi-settings.json", "", false},
		{"user settings, legacy", "dashboard:direct:ss:general", "", "user", false},
		{"settings mode changed", "dashboard:direct:ss:general", "/data/naozhi-settings.json", "user", true},
		{"planner gained project sources", "planner:main", "/data/naozhi-settings.json", "project,local", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mkOverlayRouter(t)
			r.spawn.naozhiSettingsFile = tc.settingsFile
			s := newSessionWithID(tc.key, "sess-sources")
			s.SetBackend("claude")
			putT(r, tc.key, s)
			state, _ := spawnShimState(t, r, tc.key, "", AgentOpts{Backend: "claude", Workspace: "/ws"})
			state.CLIArgs[settingSourcesIndex(t, state.CLIArgs)] = tc.stored
			wrapper, backendID := r.backends.wrapperFor("claude")
			if drift, stored, current := driftArgsFor(r).shimArgsDrift(wrapper, backendID, state, s); drift != tc.drift {
				t.Errorf("drift = %v, want %v\n  stored:  %v\n  current: %v", drift, tc.drift, stored, current)
			}
		})
	}
}

// TestSessionSettingsFile_WrittenOnSpawn: a session with settings overrides
// spawns on its own settings file holding the naozhi-owned settings plus the
// overrides; a session without them (a cron one, which the inbound policy
// leaves alone) keeps the shared file.
func TestSessionSettingsFile_WrittenOnSpawn(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir := t.TempDir()
	base := filepath.Join(dir, "naozhi-settings.json")
	if err := os.WriteFile(base, []byte(`{"model":"opus","permissions":{"deny":["Read(~/.aws/**)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewRouter(RouterConfig{
		MaxProcs:           4,
		StorePath:          filepath.Join(dir, "sessions.json"),
		NaozhiSettingsFile: base,
		BackendRuntimes: map[string]BackendRuntime{
			"claude": {Wrapper: cli.NewWrapperLazy("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")},
		},
		DefaultBackend: "claude",
	})
	var got []cli.SpawnOptions
	r.spawn.hook = func(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
		got = append(got, opts)
		return newIdleProc(), nil
	}
	t.Cleanup(r.Shutdown)

	if _, _, err := r.GetOrCreate(context.Background(), "cron:plain", AgentOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.GetOrCreate(context.Background(), "feishu:direct:held:general", AgentOpts{
		CrossSessionInbound: "hold", PermissionsDeny: []string{"Read(~/.aws/**)", "Bash"},
		PermissionsAllow: []string{"Read(//srv/state/**)"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("spawns = %d, want 2", len(got))
	}
	if got[0].SessionSettingsFile != "" {
		t.Errorf("plain session got a session settings file %q", got[0].SessionSettingsFile)
	}
	path := got[1].SessionSettingsFile
	if filepath.Dir(path) != filepath.Join(dir, "session-settings") {
		t.Fatalf("session settings path = %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Model               string `json:"model"`
		CrossSessionInbound string `json:"crossSessionInbound"`
		Permissions         struct {
			Deny  []string `json:"deny"`
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Model != "opus" || doc.CrossSessionInbound != "hold" ||
		!slices.Equal(doc.Permissions.Deny, []string{"Read(~/.aws/**)", "Bash"}) ||
		!slices.Equal(doc.Permissions.Allow, []string{"Read(//srv/state/**)"}) {
		t.Errorf("session settings = %s", data)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("session settings mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	if strings.Contains(string(data), "ultracode") {
		t.Errorf("session without ultracode got the key: %s", data)
	}
}

// Ultracode alone puts a session on its own settings file with the CLI's
// ultracode key on: the CLI ignores the keyword in a stream-json message
// that carries no human origin, so this is how a session gets the mode.
func TestSessionSettingsFile_Ultracode(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir := t.TempDir()
	r := NewRouter(RouterConfig{
		MaxProcs:  4,
		StorePath: filepath.Join(dir, "sessions.json"),
		BackendRuntimes: map[string]BackendRuntime{
			"claude": {Wrapper: cli.NewWrapperLazy("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")},
		},
		DefaultBackend: "claude",
	})
	var got []cli.SpawnOptions
	r.spawn.hook = func(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
		got = append(got, opts)
		return newIdleProc(), nil
	}
	t.Cleanup(r.Shutdown)
	if _, _, err := r.GetOrCreate(context.Background(), "cron:uc", AgentOpts{Ultracode: true}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionSettingsFile == "" {
		t.Fatalf("spawns = %+v, want one on a session settings file", got)
	}
	data, err := os.ReadFile(got[0].SessionSettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["ultracode"] != true {
		t.Errorf("session settings = %s, want ultracode true", data)
	}
	args := (&cli.ClaudeProtocol{}).BuildArgs(got[0])
	if i := slices.Index(args, "--settings"); i < 0 || args[i+1] != got[0].SessionSettingsFile {
		t.Errorf("argv %q does not pass the session settings file", args)
	}
}

func TestCLISessionName(t *testing.T) {
	t.Parallel()
	cases := []struct{ key, label, workspace, want string }{
		{"feishu:direct:alice:general", "", "/w", "alice"},
		{"feishu:group:oc_1:code-reviewer", "", "/w", "oc_1-code-reviewer"},
		{"dashboard:pj:0123456789abcdef:general", "", "/Users/me/workspace/naozhi", "naozhi"},
		{"local:takeover:Users-me-ws:general", "", "/Users/me/ws", "ws"},
		{"cron:job1", "", "/w", "cron-job1"},
		{"project:naozhi:planner", "", "/w", "project-naozhi-planner"},
		{"feishu:direct:alice:general", "  fix\nthe  build ", "/w", "fix the build"},
		{"feishu:direct:alice:general", "--resume x", "/w", "resume x"},
	}
	for _, tc := range cases {
		if got := cliSessionName(tc.key, tc.label, tc.workspace); got != tc.want {
			t.Errorf("cliSessionName(%q, %q, %q) = %q, want %q", tc.key, tc.label, tc.workspace, got, tc.want)
		}
	}
}

func TestResumedCLIName(t *testing.T) {
	t.Parallel()
	const ph = "ws" // the key-derived placeholder
	cases := []struct {
		name, def, held, aiTitle, want string
	}{
		{"held name re-passed", ph, "my-feature", "", "my-feature"},
		{"held name outlives a relabel", "New title", "Old title", "", "Old title"},
		{"held placeholder, still untitled", ph, ph, "AI title", ph},
		{"held placeholder gives way to the title", "Fix build", ph, "", "Fix build"},
		{"no held name keeps the ai-title", ph, "", "AI title", "AI title"},
		{"label over ai-title", "Fix build", "", "AI title", "Fix build"},
		{"nothing recorded", ph, "", "", ph},
		{"held name cleaned", ph, "--resume\tx", "", "resume x"},
	}
	for _, tc := range cases {
		if got := resumedCLIName(tc.def, ph, tc.held, tc.aiTitle); got != tc.want {
			t.Errorf("%s: resumedCLIName(%q, %q, %q, %q) = %q, want %q", tc.name, tc.def, ph, tc.held, tc.aiTitle, got, tc.want)
		}
	}
}

// TestTakeoverKeepsTranscriptName: a takeover resumes with the name the
// external session holds (here set by a terminal /rename), records it in the
// overlay and on the session, and falls back to the ai-title, then the
// placeholder.
func TestTakeoverKeepsTranscriptName(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	claudeDir := t.TempDir()
	ws := filepath.Join(t.TempDir(), "ws")
	r := NewRouter(RouterConfig{
		MaxProcs:  4,
		StorePath: filepath.Join(t.TempDir(), "sessions.json"),
		ClaudeDir: claudeDir,
		BackendRuntimes: map[string]BackendRuntime{
			"claude": {Wrapper: cli.NewWrapperLazy("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")},
		},
		DefaultBackend: "claude",
	})
	var got cli.SpawnOptions
	r.spawn.hook = func(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
		got = opts
		return newIdleProc(), nil
	}
	t.Cleanup(r.Shutdown)

	cases := []struct{ name, transcript, want string }{
		{"custom title", `{"type":"custom-title","customTitle":"my-feature","sessionId":"%s"}`, "my-feature"},
		{"ai title", `{"type":"ai-title","aiTitle":"Fix the build","sessionId":"%s"}`, "Fix the build"},
		{"none", `{"type":"last-prompt","lastPrompt":"go","sessionId":"%s"}`, "ws"},
	}
	for i, tc := range cases {
		sid := fmt.Sprintf("0000000%d-0000-4000-8000-000000000000", i)
		path := claudefs.SessionJSONL(claudeDir, ws, sid)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(fmt.Sprintf(tc.transcript, sid)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprintf("local:takeover:t%d:general", i)
		s, err := reserveAndTakeover(context.Background(), r, key, sid, ws, AgentOpts{})
		if err != nil {
			t.Fatalf("%s: Takeover: %v", tc.name, err)
		}
		if got.ResumeID != sid || got.Name != tc.want || got.SpawnOverlay.Name != tc.want || s.CLISessionName() != tc.want {
			t.Errorf("%s: resume %q, name %q, overlay %q, session %q; want %q", tc.name,
				got.ResumeID, got.Name, got.SpawnOverlay.Name, s.CLISessionName(), tc.want)
		}
	}
}

// The Planner and the scout sessions load no project settings: their rules
// are the gateway's alone.
func TestLaunchSpecProjectSettings(t *testing.T) {
	for key, want := range map[string]bool{
		"planner:main":               false,
		"scout:20261105T100000-ab12": false,
		"dashboard:direct:x:general": true,
		"task:0a1b2c3d4e5f":          true,
	} {
		if got := (&spawnConfig{}).launchSpecFor(key, shim.SpawnOverlay{}).ProjectSettings; got != want {
			t.Errorf("%s: ProjectSettings = %v, want %v", key, got, want)
		}
	}
}

// TestLaunchSpecDrift_TaskDeny: a dispatched session's deny rules (the
// Outlook enable_write_tools one every task: session carries,
// wireup.TaskOpts) ride in the overlay the shim state records, survive its
// JSON round trip, are written into the session's settings file from it,
// and do not read as drift on reattach.
func TestLaunchSpecDrift_TaskDeny(t *testing.T) {
	const rule = "mcp__aws-outlook-mcp__enable_write_tools"
	r := mkOverlayRouter(t)
	r.spawn.sessionSettingsDir = t.TempDir()
	wrapper, backendID := r.backends.wrapperFor("claude")
	key := "task:deny01"
	s := newSessionWithID(key, "sess-task-deny")
	s.SetBackend("claude")
	putT(r, key, s)
	state, _ := spawnShimState(t, r, key, "", AgentOpts{
		Backend: "claude", Workspace: "/ws", Name: "只读", CrossSessionInbound: "accept",
		PermissionsDeny: []string{rule},
	})
	enc, err := shim.EncodeSpawnOverlay(state.SpawnOverlay)
	if err != nil {
		t.Fatal(err)
	}
	if state.SpawnOverlay, err = shim.DecodeSpawnOverlay(enc); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(state.SpawnOverlay.PermissionsDeny, []string{rule}) {
		t.Fatalf("recorded overlay deny = %v", state.SpawnOverlay.PermissionsDeny)
	}
	if drift, stored, current := driftArgsFor(r).shimArgsDrift(wrapper, backendID, state, s); drift {
		t.Fatalf("task deny misread as drift\n  stored:  %v\n  current: %v", stored, current)
	}
	path := r.spawn.launchSpecFor(key, *state.SpawnOverlay).SettingsFile
	if path == "" || !slices.Contains(state.CLIArgs, path) {
		t.Fatalf("settings file %q not on argv %v", path, state.CLIArgs)
	}
	if err := writeSessionSettings(path, "", *state.SpawnOverlay); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var doc struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || !slices.Contains(doc.Permissions.Deny, rule) {
		t.Errorf("settings = %s (%v)", data, err)
	}
}

// The Planner's settings file carries promptCacheTtl, and its shim from a
// release before the field (overlay without it) rebuilds the same argv: the
// key lives in the settings file, whose path is the session key's, so an
// upgrade leaves the running Planner alone and its next spawn gets the hour.
func TestLaunchSpecPromptCacheTTL(t *testing.T) {
	r := mkOverlayRouter(t)
	r.spawn.sessionSettingsDir = t.TempDir()
	wrapper, backendID := r.backends.wrapperFor("claude")
	key := "planner:main"
	s := newSessionWithID(key, "sess-planner-ttl")
	s.SetBackend("claude")
	putT(r, key, s)
	state, _ := spawnShimState(t, r, key, "", AgentOpts{
		Backend: "claude", Workspace: "/ws", Name: "planner-abc123", PermissionMode: "dontAsk",
		CrossSessionInbound: "accept", PermissionsAllow: []string{"Read(//state/**)"}, PromptCacheTTL: "1h",
	})
	if state.SpawnOverlay == nil || state.SpawnOverlay.PromptCacheTTL != "1h" {
		t.Fatalf("recorded overlay = %+v, want prompt_cache_ttl 1h", state.SpawnOverlay)
	}
	path := r.spawn.launchSpecFor(key, *state.SpawnOverlay).SettingsFile
	if err := writeSessionSettings(path, "", *state.SpawnOverlay); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil || doc["promptCacheTtl"] != "1h" {
		t.Errorf("settings = %s (%v), want promptCacheTtl 1h", data, err)
	}

	old := *state.SpawnOverlay
	old.PromptCacheTTL = ""
	state.SpawnOverlay = &old
	if drift, stored, current := driftArgsFor(r).shimArgsDrift(wrapper, backendID, state, s); drift {
		t.Fatalf("a shim from before promptCacheTtl reads as drift\n  stored:  %v\n  current: %v", stored, current)
	}
}
