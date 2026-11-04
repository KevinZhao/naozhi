package cli

import (
	"slices"
	"testing"
)

// TestClaudeProtocol_BuildArgs_LaunchSpec pins the argv of the per-session
// launch fields. The zero value must leave argv untouched: any change there
// reads as drift for every surviving shim.
func TestClaudeProtocol_BuildArgs_LaunchSpec(t *testing.T) {
	t.Parallel()
	head := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--replay-user-messages",
	}
	userSources := []string{"--setting-sources", "user"}
	skip := []string{"--dangerously-skip-permissions"}
	cases := []struct {
		name string
		opts SpawnOptions
		want []string
	}{
		{"zero value", SpawnOptions{}, slices.Concat(head, userSources, skip)},
		{"tools", SpawnOptions{Tools: []string{"Read", "Grep", "Glob"}},
			slices.Concat(head, userSources, skip, []string{"--tools", "Read,Grep,Glob"})},
		{"flag-shaped tools fail closed", SpawnOptions{Tools: []string{"--mcp-config"}},
			slices.Concat(head, userSources, skip, []string{"--tools", ""})},
		{"strict mcp config", SpawnOptions{StrictMCPConfig: true},
			slices.Concat(head, userSources, skip, []string{"--strict-mcp-config"})},
		{"name", SpawnOptions{Name: "planner"},
			slices.Concat(head, userSources, skip, []string{"--name", "planner"})},
		{"flag-shaped name dropped", SpawnOptions{Name: "-x"}, slices.Concat(head, userSources, skip)},
		{"session settings over user sources", SpawnOptions{SessionSettingsFile: "/data/session-settings/k.json"},
			slices.Concat(head, userSources, []string{"--settings", "/data/session-settings/k.json"}, skip)},
		{"session settings replace naozhi settings", SpawnOptions{
			SettingsFile: "/data/naozhi-settings.json", SessionSettingsFile: "/data/session-settings/k.json",
		}, slices.Concat(head, []string{"--setting-sources", "", "--settings", "/data/session-settings/k.json"}, skip)},
		{"relative session settings ignored", SpawnOptions{
			SettingsFile: "/data/naozhi-settings.json", SessionSettingsFile: "k.json",
		}, slices.Concat(head, []string{"--setting-sources", "", "--settings", "/data/naozhi-settings.json"}, skip)},
		{"planner shape", SpawnOptions{
			PermissionMode: PermissionModeDontAsk, Tools: []string{"Read", "Grep", "Glob", "ListAgents", "SendMessage"},
			StrictMCPConfig: true, Name: "planner", Model: "opus",
		}, slices.Concat(head, userSources, []string{
			"--permission-mode", "dontAsk",
			"--tools", "Read,Grep,Glob,ListAgents,SendMessage",
			"--strict-mcp-config",
			"--name", "planner",
			"--model", "opus",
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &ClaudeProtocol{}
			if got := p.BuildArgs(tc.opts); !slices.Equal(got, tc.want) {
				t.Errorf("argv =\n %q\nwant\n %q", got, tc.want)
			}
			// The model manifest stays on the base settings source.
			if got, want := *p.settingsPath.Load(), usableSettingsFile(tc.opts.SettingsFile); got != want {
				t.Errorf("settingsPath = %q, want %q", got, want)
			}
		})
	}
}

// TestBuildArgs_LaunchFlagsDeniedInExtraArgs: the dedicated fields are the
// only argv site for --tools and --name, so agent args cannot widen a
// restricted tool set.
func TestBuildArgs_LaunchFlagsDeniedInExtraArgs(t *testing.T) {
	t.Parallel()
	args := (&ClaudeProtocol{}).BuildArgs(SpawnOptions{
		Tools:     []string{"Read"},
		ExtraArgs: []string{"--tools", "Bash", "--name=x", "--debug"},
	})
	if slices.Contains(args, "Bash") || slices.Contains(args, "--name=x") || !slices.Contains(args, "--debug") {
		t.Errorf("argv = %v", args)
	}
}

func TestPermissionModeByName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]PermissionMode{
		"bypass":  PermissionModeDefault,
		"auto":    PermissionModeAuto,
		"dontAsk": PermissionModeDontAsk,
		"dontask": PermissionModeStandard,
	} {
		if got := PermissionModeByName(name); got != want {
			t.Errorf("PermissionModeByName(%q) = %d, want %d", name, got, want)
		}
	}
}
