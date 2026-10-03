package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestValidateSystemPrompt pins the agents[].system_prompt character policy:
// multi-line allowed, argv-corrupting and log-injecting bytes rejected, a
// leading '-' rejected at load rather than dropped at spawn.
func TestValidateSystemPrompt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		prompt  string
		wantErr string // substring; "" = accept
	}{
		{"empty accepted", "", ""},
		{"plain", "You are a code review expert.", ""},
		{"multi-line with tab", "line one\n\tindented\n\nline three", ""},
		{"unicode", "你是代码评审专家。", ""},
		{"at cap", strings.Repeat("a", MaxAgentSystemPromptBytes), ""},
		{"over cap", strings.Repeat("a", MaxAgentSystemPromptBytes+1), "exceeds"},
		{"leading dash", "--allowed-tools Bash", "must not start with '-'"},
		{"CR rejected", "a\r\nb", "control characters"},
		{"NUL rejected", "a\x00b", "control characters"},
		{"DEL rejected", "a\x7fb", "control characters"},
		{"bidi rejected", "a\u202eb", "unicode controls"},
		{"C1 rejected", "a\u0085b", "unicode controls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateSystemPrompt("agents[x].system_prompt", tc.prompt)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateConfig_AgentSystemPrompt wires the validator into validateConfig
// with the field path an operator will see.
func TestValidateConfig_AgentSystemPrompt(t *testing.T) {
	t.Parallel()
	ok := &Config{Agents: map[string]AgentConfig{"reviewer": {SystemPrompt: "Be terse.\nCite lines."}}}
	if err := validateConfig(ok); err != nil {
		t.Fatalf("valid system_prompt rejected: %v", err)
	}
	bad := &Config{Agents: map[string]AgentConfig{"reviewer": {SystemPrompt: "-x"}}}
	err := validateConfig(bad)
	if err == nil || !strings.Contains(err.Error(), "agents[reviewer].system_prompt") {
		t.Fatalf("error = %v, want field path agents[reviewer].system_prompt", err)
	}
}

// TestSplitLegacySystemPromptArgs covers both flag shapes. The indices are the
// contract: the migration keeps the nodes at keptIdx and re-homes the comments
// of those at liftedIdx, so every item must land in exactly one of them.
func TestSplitLegacySystemPromptArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		in         []string
		wantKept   []int
		wantLifted []int
		wantLift   string
		wantFound  bool
	}{
		{"absent", []string{"--keep", "x"}, []int{0, 1}, nil, "", false},
		{"bare form", []string{"--append-system-prompt", "P", "--keep"}, []int{2}, []int{0, 1}, "P", true},
		{"equals form", []string{"--keep", "--append-system-prompt=P"}, []int{0}, []int{1}, "P", true},
		{"both forms join in order", []string{"--append-system-prompt", "A", "--append-system-prompt=B"}, nil, []int{0, 1, 2}, "A\n\nB", true},
		{"trailing bare flag no value", []string{"--keep", "--append-system-prompt"}, []int{0}, []int{1}, "", true},
		{"next token is a flag so no value", []string{"--append-system-prompt", "--keep"}, []int{1}, []int{0}, "", true},
		{"only the flag leaves nothing kept", []string{"--append-system-prompt", "P"}, nil, []int{0, 1}, "P", true},
		{"value-looking item after the value is kept", []string{"--append-system-prompt", "P", "3"}, []int{2}, []int{0, 1}, "P", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kept, liftedIdx, lifted, found := splitLegacySystemPromptArgs(tc.in)
			if found != tc.wantFound || lifted != tc.wantLift || !slices.Equal(kept, tc.wantKept) || !slices.Equal(liftedIdx, tc.wantLifted) {
				t.Fatalf("got (kept=%v liftedIdx=%v lifted=%q found=%v), want (kept=%v liftedIdx=%v lifted=%q found=%v)",
					kept, liftedIdx, lifted, found, tc.wantKept, tc.wantLifted, tc.wantLift, tc.wantFound)
			}
			if len(kept)+len(liftedIdx) != len(tc.in) {
				t.Errorf("%d kept + %d lifted indices for %d args: every item must be accounted for once", len(kept), len(liftedIdx), len(tc.in))
			}
		})
	}
}

// TestLoad_AgentSystemPrompt is the operator-facing end to end: a YAML block
// scalar system_prompt loads verbatim, and a legacy `--append-system-prompt`
// under args is lifted by Load so the config that never worked (#2493) now
// yields the intended prompt.
func TestLoad_AgentSystemPrompt(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("block scalar", func(t *testing.T) {
		t.Parallel()
		cfg, err := Load(write(t, `
agents:
  reviewer:
    model: sonnet
    system_prompt: |-
      You are a code review expert.
      Answer in Chinese.
`))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.Agents["reviewer"].SystemPrompt; got != "You are a code review expert.\nAnswer in Chinese." {
			t.Errorf("SystemPrompt = %q", got)
		}
	})

	t.Run("legacy args flag lifted", func(t *testing.T) {
		t.Parallel()
		cfg, err := Load(write(t, `
agents:
  reviewer:
    model: sonnet
    args: ["--append-system-prompt", "You are a code review expert.", "--keep"]
`))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		got := cfg.Agents["reviewer"]
		if got.SystemPrompt != "You are a code review expert." {
			t.Errorf("legacy flag not lifted: SystemPrompt = %q", got.SystemPrompt)
		}
		if !slices.Equal(got.Args, []string{"--keep"}) {
			t.Errorf("legacy flag left in args: %q", got.Args)
		}
	})

	t.Run("legacy flag plus explicit field is rejected", func(t *testing.T) {
		t.Parallel()
		_, err := Load(write(t, `
agents:
  reviewer:
    system_prompt: explicit
    args: ["--append-system-prompt", "legacy"]
`))
		if err == nil || !strings.Contains(err.Error(), "system_prompt") {
			t.Fatalf("Load error = %v, want conflict mentioning system_prompt", err)
		}
	})

	t.Run("bad system_prompt rejected with field path", func(t *testing.T) {
		t.Parallel()
		_, err := Load(write(t, `
agents:
  reviewer:
    system_prompt: "-not a prompt"
`))
		if err == nil || !strings.Contains(err.Error(), "agents[reviewer].system_prompt") {
			t.Fatalf("Load error = %v, want agents[reviewer].system_prompt", err)
		}
	})
}
