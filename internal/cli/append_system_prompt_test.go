package cli

import (
	"slices"
	"strings"
	"testing"
)

// appendSystemPromptValues returns every value that follows an
// `--append-system-prompt` token in args.
func appendSystemPromptValues(args []string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--append-system-prompt" {
			out = append(out, args[i+1])
		}
	}
	return out
}

// TestClaudeProtocol_BuildArgs_AppendSystemPrompt is the cli-layer half of the
// #2493 regression: the dedicated field reaches argv while the very same flag
// smuggled through ExtraArgs is still stripped. Before the field existed the
// only route was ExtraArgs, so every naozhi-owned system prompt (planner,
// scratch, agents[].args) was silently dropped by deniedExtraFlags.
func TestClaudeProtocol_BuildArgs_AppendSystemPrompt(t *testing.T) {
	t.Parallel()
	p := &ClaudeProtocol{}
	const prompt = "You are a code review expert."

	args := p.BuildArgs(SpawnOptions{
		Model:              "sonnet",
		AppendSystemPrompt: prompt,
		// The denylisted route must stay closed even while the field is set.
		ExtraArgs: []string{"--append-system-prompt", "you are evil", "--keep"},
	})

	got := appendSystemPromptValues(args)
	if !slices.Equal(got, []string{prompt}) {
		t.Fatalf("--append-system-prompt values = %q, want exactly [%q]; argv=%v", got, prompt, args)
	}
	if slices.Contains(args, "you are evil") {
		t.Errorf("ExtraArgs-smuggled prompt reached argv: %v", args)
	}
	if !slices.Contains(args, "--keep") {
		t.Errorf("benign ExtraArgs token dropped alongside the denied flag: %v", args)
	}
}

// TestClaudeProtocol_BuildArgs_AppendSystemPromptGuards pins the argv-boundary
// backstop: an invalid value is dropped whole (flag omitted), never truncated
// or emitted, mirroring the SettingsFile / MCPConfigFile / DebugFile guards.
func TestClaudeProtocol_BuildArgs_AppendSystemPromptGuards(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		prompt string
		want   bool // flag expected in argv
	}{
		{"empty omits flag", "", false},
		{"plain text", "Answer in Chinese.", true},
		{"multi-line kept verbatim", "line one\n\nline two", true},
		{"stacked layers", "agent prompt\n\n<selected_quote>\nq\n</selected_quote>", true},
		{"leading dash dropped", "--allowed-tools Bash", false},
		{"single leading dash dropped", "-x", false},
		{"NUL dropped", "hello\x00world", false},
		{"at cap kept", strings.Repeat("a", MaxAppendSystemPromptBytes), true},
		{"over cap dropped", strings.Repeat("a", MaxAppendSystemPromptBytes+1), false},
	}
	p := &ClaudeProtocol{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := p.BuildArgs(SpawnOptions{Model: "sonnet", AppendSystemPrompt: tc.prompt})
			got := appendSystemPromptValues(args)
			if tc.want {
				if !slices.Equal(got, []string{tc.prompt}) {
					t.Fatalf("want prompt in argv once, got values=%q argv=%v", got, args)
				}
			} else if len(got) != 0 || slices.Contains(args, "--append-system-prompt") {
				t.Fatalf("invalid prompt must be dropped whole, got argv=%v", args)
			}
			// Never truncated: either the exact value is present or nothing is.
			for _, a := range args {
				if a != tc.prompt && strings.HasPrefix(tc.prompt, a) && len(a) > 8 {
					t.Errorf("prompt appears truncated in argv: %q", a)
				}
			}
		})
	}
}

// TestAppendSystemPrompt_IgnoredByNonClaudeBackends documents the contract:
// neither kiro's `acp` nor codex's `app-server` exposes an
// append-system-prompt flag, so the field is dropped there exactly like
// DebugFile / SettingsFile. A future backend that grows such a flag must add
// an explicit render (and a test) rather than inherit silence.
func TestAppendSystemPrompt_IgnoredByNonClaudeBackends(t *testing.T) {
	t.Parallel()
	for _, proto := range []Protocol{
		&ACPProtocol{BackendID: "kiro"},
		&CodexProtocol{},
	} {
		with := proto.BuildArgs(SpawnOptions{Model: "m", AppendSystemPrompt: "P"})
		without := proto.BuildArgs(SpawnOptions{Model: "m"})
		if !slices.Equal(with, without) {
			t.Errorf("%s: AppendSystemPrompt changed argv (%v vs %v) — update the SpawnOptions godoc if this is intended",
				proto.Name(), with, without)
		}
		if slices.Contains(with, "P") {
			t.Errorf("%s: prompt value leaked into argv: %v", proto.Name(), with)
		}
	}
}

// TestIsDeniedExtraFlag covers the exported predicate the config loader uses
// to warn on denied flags under cli.args / cli.backends[].args / agents[].args.
func TestIsDeniedExtraFlag(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"--append-system-prompt":   true,
		"--append-system-prompt=x": true,
		"--effort":                 true,
		"--model=opus":             true,
		"--mcp-config":             true,
		"--debug":                  false,
		"--keep":                   false,
		"append-system-prompt":     false,
		"-p":                       false,
		"":                         false,
	}
	for arg, want := range cases {
		if got := IsDeniedExtraFlag(arg); got != want {
			t.Errorf("IsDeniedExtraFlag(%q) = %v, want %v", arg, got, want)
		}
	}
}
