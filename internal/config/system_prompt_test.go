package config

import (
	"os"
	"path/filepath"
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

// TestLoad_AgentSystemPrompt is the operator-facing end to end: a YAML block
// scalar system_prompt loads verbatim and a bad one is rejected with its field
// path.
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
