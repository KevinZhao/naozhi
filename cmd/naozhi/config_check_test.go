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

const openPlatformCheckConfig = `
platforms:
  weixin:
    token: "wx-token"
`

const cleanCheckConfig = openPlatformCheckConfig + `
im_access:
  platforms:
    weixin:
      allowed_users: ["wx-user"]
`

// TestConfigCheck_ExitCodes covers the three exit codes: clean config → 0,
// an unrestricted IM platform or a backend arg the argv denylist strips
// (#2412 shape) → 1 with the finding named on stdout, unparsable YAML → 2.
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

	// An enabled platform no im_access rule restricts is a warning: anyone who
	// can message the bot there runs commands on the host.
	t.Run("open_platform_exit1", func(t *testing.T) {
		var out bytes.Buffer
		code := configCheck([]string{"-config", writeCheckConfig(t, openPlatformCheckConfig)}, &out)
		if code != 1 {
			t.Fatalf("exit = %d, want 1; output:\n%s", code, out.String())
		}
		s := out.String()
		if !strings.Contains(s, "platforms.weixin") || !strings.Contains(s, "im_access") {
			t.Errorf("output must name the open platform and im_access:\n%s", s)
		}
		// Nothing is dropped: the platform runs open, so the summary must not
		// claim the value had no effect.
		if !strings.Contains(s, "1 config warning(s)") || strings.Contains(s, "drops the value") {
			t.Errorf("summary must count a config warning, not a dropped input:\n%s", s)
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

// TestConfigCheck_UnknownKeyExit1: a misspelled key exits 1 and names the
// dotted path and line (#2639). This is the case the command could not see
// before — yaml.Unmarshal dropped unknown keys silently, so `config check`
// reported OK on a config whose keys did nothing. The two spellings here are
// the keys from #2553, the bug that made the gap visible.
func TestConfigCheck_UnknownKeyExit1(t *testing.T) {
	cfg := cleanCheckConfig + `
server:
  debug_moed: true
projects:
  publc_tmp: true
`
	var out bytes.Buffer
	code := configCheck([]string{"-config", writeCheckConfig(t, cfg)}, &out)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; output:\n%s", code, out.String())
	}
	s := out.String()
	for _, want := range []string{"config-unknown", "server.debug_moed", "projects.publc_tmp", "ignored"} {
		if !strings.Contains(s, want) {
			t.Errorf("output must contain %q:\n%s", want, s)
		}
	}
	// The correctly spelled keys must NOT be reported, or the report is noise.
	for _, unwanted := range []string{"server.debug_mode ", "projects.public_tmp "} {
		if strings.Contains(s, unwanted) {
			t.Errorf("real key %q reported as unknown:\n%s", unwanted, s)
		}
	}
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

// A value naozhi replaces with a default is configured input that has no
// effect, so check exits 1 on it rather than printing OK (#2897 C4).
func TestConfigCheck_ReplacedValuesExit1(t *testing.T) {
	for name, tc := range map[string]struct{ body, key string }{
		"timezone":          {"cron:\n  timezone: Mars/Olympus\n", "cron.timezone"},
		"update mode":       {"update:\n  mode: dowload\n", "update.mode"},
		"tick timeout":      {"sysession:\n  tick_timeout: 5 minutes\n", "sysession.tick_timeout"},
		"shim idle timeout": {"session:\n  shim:\n    idle_timeout: 4 hours\n", "session.shim.idle_timeout"},
		"shim buffer size":  {"session:\n  shim:\n    max_buffer_bytes: 50 megs\n", "session.shim.max_buffer_bytes"},
		"jsonl max age":     {"sysession:\n  runner:\n    jsonl_max_age: 7d\n", "sysession.runner.jsonl_max_age"},
		"stdio max size":    {"log:\n  stdio_max_size: 64 megs\n", "log.stdio_max_size"},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if code := configCheck([]string{"-config", writeCheckConfig(t, cleanCheckConfig+tc.body)}, &out); code != 1 {
				t.Errorf("exit %d, want 1\n%s", code, out.String())
			}
			if !strings.Contains(out.String(), "config-invalid") || !strings.Contains(out.String(), tc.key) {
				t.Errorf("output does not name the replaced value %s:\n%s", tc.key, out.String())
			}
		})
	}
}

// TestConfigCheck_StartupRefusalsAreFatal: an agent_commands entry naming an
// undefined agent, a dashboard_token under 8 characters and a config with no
// registered backend id are configs the server refuses to run, so `config
// check` must report them as Fatal (exit 2) rather than OK.
func TestConfigCheck_StartupRefusalsAreFatal(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"undefined agent command", cleanCheckConfig + "agent_commands:\n  /x: ghost\n",
			`agent_commands["/x"] references undefined agent "ghost"`},
		{"short dashboard token", cleanCheckConfig + "server:\n  dashboard_token: \"short\"\n",
			"server.dashboard_token is too short"},
		{"single unknown backend", cleanCheckConfig + "cli:\n  backend: nope\n",
			"no usable cli backend configured"},
		{"every listed backend unknown", cleanCheckConfig + "cli:\n  backends:\n    - id: nope\n    - id: nada\n",
			"no usable cli backend configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			code := configCheck([]string{"-config", writeCheckConfig(t, tc.body), "-json"}, &out)
			if code != 2 {
				t.Fatalf("exit = %d, want 2; output:\n%s", code, out.String())
			}
			var doc struct {
				Fatal []string `json:"fatal"`
			}
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
				t.Fatalf("-json output not parsable: %v\n%s", err, out.String())
			}
			if len(doc.Fatal) != 1 || !strings.Contains(doc.Fatal[0], tc.want) {
				t.Errorf("fatal = %q, want one entry containing %q", doc.Fatal, tc.want)
			}
		})
	}
}

// TestConfigCheck_ReportsValidateFindings: what startup logs through
// cfg.Validate() reaches the report, each finding once — a cli.backend that
// is not listed (startup silently binds kiro, #3298) and an unknown entry,
// which the per-backend loop also skips but must not report again.
func TestConfigCheck_ReportsValidateFindings(t *testing.T) {
	cfg := cleanCheckConfig + `
cli:
  backend: claude
  backends:
    - id: kiro
      path: /nonexistent/kiro
    - id: definitely-not-a-backend
      path: /nonexistent/x
`
	var out bytes.Buffer
	code := configCheck([]string{"-json", "-config", writeCheckConfig(t, cfg)}, &out)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; output:\n%s", code, out.String())
	}
	var res checkResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.String())
	}
	byKey := map[string][]backendDiag{}
	for _, d := range res.Diags {
		byKey[d.Key] = append(byKey[d.Key], d)
	}
	def := byKey["cli.backend"]
	if len(def) != 1 || def[0].Layer != "config-validate" || def[0].Action != "warn" ||
		!strings.Contains(def[0].Reason, `falls back to "kiro"`) {
		t.Errorf("cli.backend diags = %+v, want one config-validate warn naming the kiro fallback", def)
	}
	if got := byKey["cli.backends[definitely-not-a-backend]"]; len(got) != 1 || got[0].Action != "error" {
		t.Errorf("unknown entry diags = %+v, want exactly one error", got)
	}
}
