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

// TestSplitLegacySystemPromptArgs covers both flag shapes and the aliasing
// contract of the no-op path.
func TestSplitLegacySystemPromptArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		in        []string
		wantKept  []string
		wantLift  string
		wantFound bool
	}{
		{"absent", []string{"--keep", "x"}, []string{"--keep", "x"}, "", false},
		{"bare form", []string{"--append-system-prompt", "P", "--keep"}, []string{"--keep"}, "P", true},
		{"equals form", []string{"--keep", "--append-system-prompt=P"}, []string{"--keep"}, "P", true},
		{"both forms join in order", []string{"--append-system-prompt", "A", "--append-system-prompt=B"}, nil, "A\n\nB", true},
		{"trailing bare flag no value", []string{"--keep", "--append-system-prompt"}, []string{"--keep"}, "", true},
		{"next token is a flag so no value", []string{"--append-system-prompt", "--keep"}, []string{"--keep"}, "", true},
		{"only the flag leaves nil args", []string{"--append-system-prompt", "P"}, nil, "P", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kept, lifted, found := splitLegacySystemPromptArgs(tc.in)
			if found != tc.wantFound || lifted != tc.wantLift || !slices.Equal(kept, tc.wantKept) {
				t.Fatalf("got (kept=%q lifted=%q found=%v), want (kept=%q lifted=%q found=%v)",
					kept, lifted, found, tc.wantKept, tc.wantLift, tc.wantFound)
			}
			if !found && len(tc.in) > 0 && &kept[0] != &tc.in[0] {
				t.Error("no-op path must return the input slice itself")
			}
		})
	}
}

// TestLiftLegacySystemPromptArgs exercises the config-level migration on a
// Config value: the legacy flag moves into system_prompt, other agents are
// untouched, and an explicit conflict is rejected.
func TestLiftLegacySystemPromptArgs(t *testing.T) {
	t.Parallel()
	t.Run("lifts and cleans args", func(t *testing.T) {
		t.Parallel()
		cfg := &Config{Agents: map[string]AgentConfig{
			"reviewer": {Model: "sonnet", Args: []string{"--append-system-prompt", "You are a code review expert.", "--keep"}},
			"other":    {Args: []string{"--keep"}},
		}}
		if err := liftLegacySystemPromptArgs(cfg); err != nil {
			t.Fatalf("lift: %v", err)
		}
		got := cfg.Agents["reviewer"]
		if got.SystemPrompt != "You are a code review expert." || !slices.Equal(got.Args, []string{"--keep"}) || got.Model != "sonnet" {
			t.Errorf("reviewer after lift = %+v", got)
		}
		if other := cfg.Agents["other"]; !slices.Equal(other.Args, []string{"--keep"}) || other.SystemPrompt != "" {
			t.Errorf("unrelated agent changed: %+v", other)
		}
		// The lifted value must then pass the system_prompt validator so the
		// migrated config loads.
		if err := validateConfig(cfg); err != nil {
			t.Errorf("migrated config fails validation: %v", err)
		}
	})
	t.Run("conflict with explicit system_prompt is an error", func(t *testing.T) {
		t.Parallel()
		cfg := &Config{Agents: map[string]AgentConfig{
			"reviewer": {SystemPrompt: "explicit", Args: []string{"--append-system-prompt", "legacy"}},
		}}
		err := liftLegacySystemPromptArgs(cfg)
		if err == nil || !strings.Contains(err.Error(), "agents[reviewer]") || !strings.Contains(err.Error(), "system_prompt") {
			t.Fatalf("error = %v, want conflict naming agents[reviewer] and system_prompt", err)
		}
	})
	t.Run("nothing to lift is a no-op", func(t *testing.T) {
		t.Parallel()
		cfg := &Config{Agents: map[string]AgentConfig{"a": {Args: []string{"--keep"}, SystemPrompt: "S"}}}
		if err := liftLegacySystemPromptArgs(cfg); err != nil {
			t.Fatal(err)
		}
		if a := cfg.Agents["a"]; a.SystemPrompt != "S" || !slices.Equal(a.Args, []string{"--keep"}) {
			t.Errorf("no-op changed the agent: %+v", a)
		}
	})
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
