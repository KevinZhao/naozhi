package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

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
			bare.Tools, bare.PermissionMode, bare.Name, bare.StrictMCPConfig = nil, "", "", false
			bare.CrossSessionInbound, bare.PermissionsDeny = "", nil
			state.SpawnOverlay = &bare
			if drift, _, _ := driftArgsFor(r).shimArgsDrift(wrapper, backendID, state, s); !drift {
				t.Fatal("overlay without the launch fields still matched: the drift rebuild is not reading them")
			}
		})
	}
}

// TestSessionSettingsFile_WrittenOnSpawn: a session with settings overrides
// spawns on its own settings file holding the naozhi-owned settings plus the
// overrides; a session without them keeps the shared file.
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

	if _, _, err := r.GetOrCreate(context.Background(), "feishu:direct:plain:general", AgentOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.GetOrCreate(context.Background(), "feishu:direct:held:general", AgentOpts{
		CrossSessionInbound: "hold", PermissionsDeny: []string{"Read(~/.aws/**)", "Bash"},
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
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Model != "opus" || doc.CrossSessionInbound != "hold" ||
		!slices.Equal(doc.Permissions.Deny, []string{"Read(~/.aws/**)", "Bash"}) {
		t.Errorf("session settings = %s", data)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("session settings mode = %v, %v; want 0600", fi.Mode().Perm(), err)
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
