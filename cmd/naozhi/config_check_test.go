package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCheckConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const cleanCheckConfig = `
platforms:
  weixin:
    token: "wx-token"
`

// TestConfigCheck_ExitCodes covers the three exit codes: clean config → 0,
// a backend arg the argv denylist strips (#2412 shape) → 1 with the drop
// named on stdout, unparsable YAML → 2.
func TestConfigCheck_ExitCodes(t *testing.T) {
	t.Run("clean_config_exit0", func(t *testing.T) {
		var out bytes.Buffer
		code := configCheck([]string{"-config", writeCheckConfig(t, cleanCheckConfig)}, &out)
		if code != 0 {
			t.Fatalf("exit = %d, want 0; output:\n%s", code, out.String())
		}
		if !strings.Contains(out.String(), "config check: OK") {
			t.Errorf("missing OK line:\n%s", out.String())
		}
	})

	t.Run("denied_flag_exit1", func(t *testing.T) {
		cfg := cleanCheckConfig + `
cli:
  backends:
    - id: claude
      path: /usr/bin/claude
      args: ["--effort", "high"]
`
		var out bytes.Buffer
		code := configCheck([]string{"-config", writeCheckConfig(t, cfg)}, &out)
		if code != 1 {
			t.Fatalf("exit = %d, want 1; output:\n%s", code, out.String())
		}
		s := out.String()
		if !strings.Contains(s, "argv-denylist") || !strings.Contains(s, "--effort") || !strings.Contains(s, "dropped") {
			t.Errorf("output must name the argv-denylist --effort drop:\n%s", s)
		}
	})

	t.Run("fatal_exit2", func(t *testing.T) {
		var out bytes.Buffer
		code := configCheck([]string{"-config", writeCheckConfig(t, ":\tnot yaml [")}, &out)
		if code != 2 {
			t.Fatalf("exit = %d, want 2; output:\n%s", code, out.String())
		}
		if !strings.Contains(out.String(), "FATAL") {
			t.Errorf("missing FATAL line:\n%s", out.String())
		}
	})
}

// TestConfigCheck_EffortCapsGate: an effort tier on a backend without
// EffortTier (codex) exits 1 and says why.
func TestConfigCheck_EffortCapsGate(t *testing.T) {
	cfg := cleanCheckConfig + `
cli:
  backends:
    - id: codex
      path: /usr/bin/codex
      effort: high
`
	var out bytes.Buffer
	code := configCheck([]string{"-config", writeCheckConfig(t, cfg)}, &out)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; output:\n%s", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "caps") || !strings.Contains(s, "effort") || !strings.Contains(s, "ignored") {
		t.Errorf("output must name the caps effort ignore:\n%s", s)
	}
}

// TestConfigCheck_EffectiveMasksSecrets: -effective prints argv and env, and
// a token-bearing env value never appears in full.
func TestConfigCheck_EffectiveMasksSecrets(t *testing.T) {
	const secret = "sk-ant-oat01-full-secret-value-1234567890"
	t.Setenv("ANTHROPIC_AUTH_TOKEN", secret)

	cfg := cleanCheckConfig + `
cli:
  backends:
    - id: claude
      path: /usr/bin/claude
      model: claude-opus-4.7
`
	var out bytes.Buffer
	code := configCheck([]string{"-config", writeCheckConfig(t, cfg), "-effective"}, &out)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; output:\n%s", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "backend claude argv:") || !strings.Contains(s, "--model") {
		t.Errorf("-effective must print the backend argv:\n%s", s)
	}
	if strings.Contains(s, secret) {
		t.Errorf("full token leaked into -effective output")
	}
	if !strings.Contains(s, "ANTHROPIC_AUTH_TOKEN=sk-a…(len=") {
		t.Errorf("masked token (first 4 + length) missing:\n%s", s)
	}
}

// TestConfigCheck_JSONShape: -json is jq-parsable and carries fatal[],
// diags[] and effective{}.
func TestConfigCheck_JSONShape(t *testing.T) {
	cfg := cleanCheckConfig + `
cli:
  backends:
    - id: claude
      path: /usr/bin/claude
      args: ["--effort=high"]
`
	var out bytes.Buffer
	code := configCheck([]string{"-config", writeCheckConfig(t, cfg), "-effective", "-json"}, &out)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; output:\n%s", code, out.String())
	}
	var doc struct {
		Fatal     []string                  `json:"fatal"`
		Diags     []map[string]any          `json:"diags"`
		Effective map[string]map[string]any `json:"effective"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("-json output not parsable: %v\n%s", err, out.String())
	}
	if len(doc.Fatal) != 0 {
		t.Errorf("fatal = %v, want empty", doc.Fatal)
	}
	if len(doc.Diags) == 0 {
		t.Error("diags empty, want the --effort drop")
	}
	if _, ok := doc.Effective["claude"]; !ok {
		t.Errorf("effective lacks claude entry: %v", doc.Effective)
	}
}

// TestConfigCheck_RegisteredSubcommand: the registry routes `naozhi config`.
func TestConfigCheck_RegisteredSubcommand(t *testing.T) {
	if findSubcmd("config") == nil {
		t.Fatal(`registry has no "config" entry`)
	}
}
