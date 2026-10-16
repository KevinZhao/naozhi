package cli

import (
	"slices"
	"testing"
)

// permissionModeArgs extracts every --permission-mode value from a BuildArgs
// result, so a test can assert the flag appears exactly once.
func permissionModeArgs(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "--permission-mode" && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// TestClaudeProtocol_BuildArgs_PermissionModes pins the argv of each mode. The
// zero value must stay byte-identical to the argv every deployment ran before
// cli.permission_mode existed: a change there reads as arg-drift and respawns
// every surviving shim on the next restart.
func TestClaudeProtocol_BuildArgs_PermissionModes(t *testing.T) {
	t.Parallel()
	head := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--replay-user-messages",
		"--setting-sources", "user",
	}
	tail := []string{"--model", "sonnet"}
	cases := []struct {
		name string
		mode PermissionMode
		mid  []string
	}{
		{"default (bypass)", PermissionModeDefault, []string{"--dangerously-skip-permissions"}},
		{"auto", PermissionModeAuto, []string{"--permission-mode", "auto"}},
		{"standard", PermissionModeStandard, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := (&ClaudeProtocol{}).BuildArgs(SpawnOptions{Model: "sonnet", PermissionMode: tc.mode})
			want := slices.Concat(head, tc.mid, tail)
			if !slices.Equal(got, want) {
				t.Errorf("argv =\n %q\nwant\n %q", got, want)
			}
		})
	}
}

// TestClaudeProtocol_BuildArgs_AutoModeDropsSkipFlag: auto mode must never
// carry the skip flag too, or the CLI bypasses the classifier it was asked for.
func TestClaudeProtocol_BuildArgs_AutoModeDropsSkipFlag(t *testing.T) {
	t.Parallel()
	args := (&ClaudeProtocol{}).BuildArgs(SpawnOptions{
		Model:          "opus",
		PermissionMode: PermissionModeAuto,
		SettingsFile:   "/data/naozhi/settings.json",
		MCPConfigFile:  "/data/naozhi/mcp.json",
	})
	if slices.Contains(args, "--dangerously-skip-permissions") {
		t.Errorf("auto mode emitted --dangerously-skip-permissions: %v", args)
	}
	if got := permissionModeArgs(args); !slices.Equal(got, []string{"auto"}) {
		t.Errorf("--permission-mode values = %v, want [auto]; args=%v", got, args)
	}
}

// TestBuildArgs_PermissionModeStillDeniedInExtraArgs: the dedicated field is
// the only argv site for --permission-mode, so cli.args or agents[].args
// cannot switch an auto-mode spawn to bypassPermissions.
func TestBuildArgs_PermissionModeStillDeniedInExtraArgs(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	for _, extra := range [][]string{
		{"--permission-mode", "bypassPermissions"},
		{"--permission-mode=bypassPermissions"},
	} {
		args := p.BuildArgs(SpawnOptions{Model: "opus", PermissionMode: PermissionModeAuto, ExtraArgs: extra})
		if got := permissionModeArgs(args); !slices.Equal(got, []string{"auto"}) {
			t.Errorf("ExtraArgs %v: --permission-mode values = %v, want [auto]; args=%v", extra, got, args)
		}
		if slices.Contains(args, "bypassPermissions") || slices.Contains(args, "--permission-mode=bypassPermissions") {
			t.Errorf("ExtraArgs %v leaked into argv: %v", extra, args)
		}
	}
}

// TestNonClaudeBuildArgs_IgnorePermissionMode: cli.permission_mode is
// router-global, so it reaches kiro and codex spawns too; their argv must not
// change with it.
func TestNonClaudeBuildArgs_IgnorePermissionMode(t *testing.T) {
	t.Parallel()
	for _, p := range []Protocol{&ACPProtocol{BackendID: "kiro"}, &CodexProtocol{}} {
		base := p.BuildArgs(SpawnOptions{Model: "m"})
		for _, mode := range []PermissionMode{PermissionModeAuto, PermissionModeStandard} {
			if got := p.BuildArgs(SpawnOptions{Model: "m", PermissionMode: mode}); !slices.Equal(got, base) {
				t.Errorf("%s with PermissionMode %d: argv = %v, want %v", p.Name(), mode, got, base)
			}
		}
	}
}
