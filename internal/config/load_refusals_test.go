package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// captureSlog redirects the default slog logger to an in-memory buffer for
// the duration of fn, restoring the previous default afterwards. Returns the
// captured text output.
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

// A dashboard_token under minDashboardTokenLen is refused by Load, so the
// refusal reaches `config check` and every subcommand before the server has
// started anything. Tokens from 8 up load; the boundary is pinned both sides.
func TestLoad_DashboardTokenLength(t *testing.T) {
	cases := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{"empty loads", "", false},
		{"one char refused", "t", true},
		{"seven chars refused", "1234567", true},
		{"eight chars load", "12345678", false},
		{"sixteen chars load", "0123456789abcdef", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "server:\n  dashboard_token: \"" + tc.token + "\"\n"
			_, err := Load(writeCfg(t, body))
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Load accepted a %d-char dashboard_token", len(tc.token))
			}
			if !strings.Contains(err.Error(), "server.dashboard_token is too short") || !strings.Contains(err.Error(), "8") {
				t.Errorf("error = %q, want it to name the field and the 8-character minimum", err)
			}
		})
	}
}

// An unexpanded ${VAR} is shorter than 8 characters in the common case; it must
// still be reported as the placeholder it is, not as a short token.
func TestLoad_DashboardTokenPlaceholderBeatsLength(t *testing.T) {
	_, err := Load(writeCfg(t, "server:\n  dashboard_token: \"${X}\"\n"))
	if err == nil || !strings.Contains(err.Error(), "unexpanded ${VAR}") {
		t.Fatalf("err = %v, want the unexpanded-placeholder refusal", err)
	}
}

func TestLoad_AgentCommandsMustReferenceDefinedAgents(t *testing.T) {
	const agents = `
agents:
  general:
    model: "sonnet"
  planner:
    model: "opus"
`
	t.Run("all resolve", func(t *testing.T) {
		cfg, err := Load(writeCfg(t, agents+"agent_commands:\n  /ask: general\n  /plan: planner\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.AgentCommands) != 2 {
			t.Errorf("AgentCommands = %v, want both entries", cfg.AgentCommands)
		}
	})
	t.Run("no commands", func(t *testing.T) {
		if _, err := Load(writeCfg(t, agents)); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})
	t.Run("dangling target refused", func(t *testing.T) {
		_, err := Load(writeCfg(t, agents+"agent_commands:\n  /ask: general\n  /x: ghost\n"))
		if err == nil {
			t.Fatal("Load accepted an agent_commands entry naming an undefined agent")
		}
		if want := `agent_commands["/x"] references undefined agent "ghost"`; !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want %q", err, want)
		}
	})
	t.Run("no agents at all", func(t *testing.T) {
		_, err := Load(writeCfg(t, "agent_commands:\n  review: code-reviewer\n"))
		if err == nil || !strings.Contains(err.Error(), `"code-reviewer"`) {
			t.Fatalf("err = %v, want the undefined code-reviewer refusal", err)
		}
	})
	// Keys are lowercased before validation, so the error names the key the
	// router will match on; with two dangling entries the sorted-first wins.
	t.Run("lowercased key, deterministic pick", func(t *testing.T) {
		for range 20 {
			_, err := Load(writeCfg(t, agents+"agent_commands:\n  /Zed: nobody\n  /Bad: ghost\n"))
			if err == nil {
				t.Fatal("Load accepted two dangling agent_commands entries")
			}
			if want := `agent_commands["/bad"] references undefined agent "ghost"`; !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want %q", err, want)
			}
		}
	})
}

// Accepted tokens that are empty or under weakDashboardTokenLen warn; 16 and
// up are silent. Load logs the same warning through validateServer.
func TestWarnDashboardToken(t *testing.T) {
	cases := []struct {
		name, token, want string
	}{
		{"empty", "", "SECURITY: dashboard_token is empty"},
		{"eight chars", "12345678", "dashboard_token is short"},
		{"fifteen chars", "0123456789abcde", "dashboard_token is short"},
		{"sixteen chars", "0123456789abcdef", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			direct := captureSlog(t, func() { WarnDashboardToken(tc.token) })
			viaLoad := captureSlog(t, func() {
				if _, err := Load(writeCfg(t, "server:\n  dashboard_token: \""+tc.token+"\"\n")); err != nil {
					t.Fatalf("Load: %v", err)
				}
			})
			for src, out := range map[string]string{"WarnDashboardToken": direct, "Load": viaLoad} {
				if tc.want == "" {
					if strings.Contains(out, "dashboard_token") {
						t.Errorf("%s logged %q for a %d-char token, want no token warning", src, out, len(tc.token))
					}
				} else if !strings.Contains(out, tc.want) {
					t.Errorf("%s logged %q, want it to contain %q", src, out, tc.want)
				}
			}
		})
	}
}

// An enabled platform block without its credential, and a notify target that
// names an unconfigured platform, are both refused at Load instead of failing
// later at connect or notification time.
func TestLoad_PlatformRefusals(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"slack without bot_token", "platforms:\n  slack:\n    bot_token: \"\"\n", "slack bot_token is required"},
		{"notify_default unknown platform", "cron:\n  notify_default:\n    platform: \"nosuchplatform\"\n", "cron.notify_default.platform"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
