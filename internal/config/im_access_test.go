package config

import (
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/imauth"
)

const imAccessSlack = "platforms:\n  slack:\n    bot_token: \"xoxb-test\"\n"

// Each malformed im_access shape is fatal: loading it anyway would leave a
// platform open, or shut, without the operator knowing.
func TestLoad_IMAccessRefusals(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"unknown platform", "im_access:\n  platforms:\n    feishu_:\n      allowed_users: [ou_1]\n", `unknown platform "feishu_"`},
		{"entry with no ids", "im_access:\n  platforms:\n    slack: {}\n", "im_access.platforms.slack lists no allowed_users or admin_users"},
		{"null entry", "im_access:\n  platforms:\n    slack:\n", "im_access.platforms.slack lists no allowed_users or admin_users"},
		{"blank id", "im_access:\n  platforms:\n    slack:\n      allowed_users: [U1, \"  \"]\n", "im_access.platforms.slack.allowed_users[1] is empty"},
		{"blank admin id", "im_access:\n  platforms:\n    discord:\n      admin_users: [\"\"]\n", "im_access.platforms.discord.admin_users[0] is empty"},
		{"placeholder", "im_access:\n  platforms:\n    feishu:\n      admin_users: [\"${NAOZHI_IMACCESS_UNSET_VAR}\"]\n", "admin_users[0] contains unexpanded ${VAR}"},
		{"control byte", "im_access:\n  platforms:\n    weixin:\n      allowed_users: [\"a\\u0007b\"]\n", "allowed_users[0] contains control characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeCfg(t, tc.body))
			if err == nil {
				t.Fatalf("Load accepted:\n%s", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoad_IMAccessPolicy(t *testing.T) {
	cfg, err := Load(writeCfg(t, `im_access:
  default_deny: true
  deny_reply: "  ask ops  "
  platforms:
    feishu:
      allowed_users: [" ou_alice ", ou_bob]
      admin_users: [ou_root]
    slack:
      admin_users: [U1]
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := cfg.IMAccessPolicy()
	if !p.DefaultDeny || p.DenyReply != "ask ops" {
		t.Errorf("DefaultDeny=%v DenyReply=%q, want true and the trimmed text", p.DefaultDeny, p.DenyReply)
	}
	for _, c := range []struct {
		platform, user string
		class          imauth.Class
		want           bool
	}{
		{"feishu", "ou_alice", imauth.Chat, true},
		{"feishu", "ou_alice", imauth.Admin, false},
		{"feishu", "ou_root", imauth.Admin, true},
		{"feishu", "ou_eve", imauth.Chat, false},
		{"slack", "U1", imauth.Admin, true},
		{"discord", "anyone", imauth.Chat, false},
	} {
		if ok, why := p.Decide(c.platform, c.user, c.class); ok != c.want {
			t.Errorf("Decide(%s, %s, %v) = %v (%s), want %v", c.platform, c.user, c.class, ok, why, c.want)
		}
	}
}

// With no im_access block the policy decides nothing: every platform stays
// open, which is the behaviour before im_access existed.
func TestIMAccessPolicy_AbsentAllowsAll(t *testing.T) {
	cfg, err := Load(writeCfg(t, imAccessSlack))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ok, why := cfg.IMAccessPolicy().Decide("slack", "", imauth.Admin); !ok {
		t.Errorf("absent im_access denied (%s)", why)
	}
}

func imAccessWarns(cfg *Config) []ValidationDiag {
	var out []ValidationDiag
	for _, d := range cfg.Validate() {
		if strings.HasPrefix(d.Field, "platforms.") {
			out = append(out, d)
		}
	}
	return out
}

// A configured platform with no rule warns in Validate (startup log and
// `config check`); a rule or default_deny clears it.
func TestValidate_IMAccessWarn(t *testing.T) {
	cases := []struct {
		name, extra string
		wantWarn    bool
	}{
		{"no im_access", "", true},
		{"rule for another platform", "im_access:\n  platforms:\n    feishu:\n      allowed_users: [ou_1]\n", true},
		{"rule set", "im_access:\n  platforms:\n    slack:\n      allowed_users: [U1]\n", false},
		{"default_deny", "im_access:\n  default_deny: true\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeCfg(t, imAccessSlack+tc.extra))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := imAccessWarns(cfg)
			if !tc.wantWarn {
				if len(got) != 0 {
					t.Errorf("unexpected diags: %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("diags = %+v, want one for platforms.slack", got)
			}
			d := got[0]
			if d.Level != "warn" || d.Field != "platforms.slack" || !strings.Contains(d.Msg, "IM 入口无鉴权") ||
				!strings.Contains(d.Hint, "im_access.platforms.slack.allowed_users") {
				t.Errorf("diag = %+v", d)
			}
		})
	}
}

// An unconfigured platform is not reachable, so it gets no warning.
func TestValidate_IMAccessNoPlatformNoWarn(t *testing.T) {
	cfg, err := Load(writeCfg(t, "server:\n  addr: \":0\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := imAccessWarns(cfg); len(got) != 0 {
		t.Errorf("diags = %+v, want none without a platform", got)
	}
}
