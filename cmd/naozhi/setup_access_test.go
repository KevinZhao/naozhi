package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
)

const scanUser = "o9cq800kum_4g8Py8Qw5G0a@im.wechat"

// writeSetup runs setupWriteConfig against existing (empty = no file yet)
// and loads the result through the production config pipeline.
func writeSetup(t *testing.T, existing, userID string) (weixinAccess, *config.Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if existing != "" {
		if err := os.WriteFile(path, []byte(existing), 0600); err != nil {
			t.Fatal(err)
		}
	}
	access, err := setupWriteConfig(path, "wx-token", userID)
	if err != nil {
		t.Fatalf("setupWriteConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load on setup output: %v\n%s", err, data)
	}
	return access, cfg, string(data)
}

func posture(t *testing.T, cfg *config.Config, name string) config.IMAccessPosture {
	t.Helper()
	for _, p := range cfg.IMAccessPostures() {
		if p.Platform == name {
			return p
		}
	}
	t.Fatalf("no posture for %s in %+v", name, cfg.IMAccessPostures())
	return config.IMAccessPosture{}
}

func hasOpenDiag(cfg *config.Config, name string) bool {
	return slices.ContainsFunc(cfg.Validate(), func(d config.ValidationDiag) bool {
		return d.Field == "platforms."+name && strings.Contains(d.Msg, "IM 入口无鉴权")
	})
}

func TestSetupWriteConfig_FreshFileAllowlistsScanningUser(t *testing.T) {
	access, cfg, data := writeSetup(t, "", scanUser)
	if access != weixinAccessAllowed {
		t.Fatalf("access = %d, want weixinAccessAllowed", access)
	}
	if got := cfg.IMAccess.Platforms["weixin"].AllowedUsers; !slices.Equal(got, []string{scanUser}) {
		t.Errorf("weixin allowed_users = %q, want [%q]\n%s", got, scanUser, data)
	}
	if cfg.IMAccess.DefaultDeny {
		t.Error("default_deny written alongside an allowlist")
	}
	if p := posture(t, cfg, "weixin"); p.Open || p.Users != 1 {
		t.Errorf("weixin posture = %+v, want restricted to 1 user", p)
	}
	if hasOpenDiag(cfg, "weixin") {
		t.Error("setup output still triggers the open-platform warning")
	}
}

func TestSetupWriteConfig_FreshFileWithoutIDDefaultDenies(t *testing.T) {
	access, cfg, _ := writeSetup(t, "", "")
	if access != weixinAccessDefaultDeny {
		t.Fatalf("access = %d, want weixinAccessDefaultDeny", access)
	}
	if !cfg.IMAccess.DefaultDeny || len(cfg.IMAccess.Platforms) != 0 {
		t.Errorf("im_access = %+v, want default_deny only", cfg.IMAccess)
	}
	if p := posture(t, cfg, "weixin"); p.Open {
		t.Errorf("weixin posture = %+v, want refused", p)
	}
	if hasOpenDiag(cfg, "weixin") {
		t.Error("setup output still triggers the open-platform warning")
	}
}

func TestSetupWriteConfig_UnsafeIDNotWritten(t *testing.T) {
	cases := map[string]string{
		"newline":     "o_a@im.wechat\ninjected: true",
		"esc":         "o_a\x1b[31m@im.wechat",
		"del":         "o_a\x7f@im.wechat",
		"c1":          "o_a\u0085@im.wechat",
		"bidi":        "o_a\u202e@im.wechat",
		"placeholder": "${WEIXIN_USER}",
		"blank":       "   ",
		"too_long":    strings.Repeat("a", 257),
		"bad_utf8":    "o_a\xff@im.wechat",
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			access, cfg, data := writeSetup(t, "", id)
			if access != weixinAccessDefaultDeny || !cfg.IMAccess.DefaultDeny {
				t.Errorf("access = %d, default_deny = %v; want the no-ID fallback", access, cfg.IMAccess.DefaultDeny)
			}
			if _, ok := cfg.IMAccess.Platforms["weixin"]; ok {
				t.Errorf("unsafe ID produced a weixin entry:\n%s", data)
			}
		})
	}
	if id, ok := setupAccessID(strings.Repeat("a", 256)); !ok || len(id) != 256 {
		t.Error("a 256-byte ID must be accepted")
	}
	if id, ok := setupAccessID("  " + scanUser + "\t"); !ok || id != scanUser {
		t.Errorf("setupAccessID trims: got %q, %v", id, ok)
	}
}

func TestSetupWriteConfig_YAMLSpecialIDStaysOneScalar(t *testing.T) {
	id := `a: b#c "q" - [x], {y}`
	access, cfg, _ := writeSetup(t, "", id)
	if access != weixinAccessAllowed {
		t.Fatalf("access = %d, want weixinAccessAllowed", access)
	}
	if got := cfg.IMAccess.Platforms["weixin"].AllowedUsers; !slices.Equal(got, []string{id}) {
		t.Errorf("allowed_users = %q, want [%q]", got, id)
	}
}

func TestSetupWriteConfig_ExistingFileAddsOnlyWeixin(t *testing.T) {
	existing := `platforms:
  feishu:
    app_id: "abc"
    app_secret: "s"
`
	access, cfg, _ := writeSetup(t, existing, scanUser)
	if access != weixinAccessAllowed {
		t.Fatalf("access = %d, want weixinAccessAllowed", access)
	}
	if cfg.IMAccess.DefaultDeny {
		t.Error("default_deny written into an existing config: it would lock out feishu")
	}
	if p := posture(t, cfg, "feishu"); !p.Open {
		t.Errorf("feishu posture = %+v, want unchanged (open)", p)
	}
	if p := posture(t, cfg, "weixin"); p.Open || p.Users != 1 {
		t.Errorf("weixin posture = %+v, want restricted to 1 user", p)
	}
}

func TestSetupWriteConfig_ExistingFileWithoutIDWritesNothing(t *testing.T) {
	existing := `platforms:
  feishu:
    app_id: "abc"
    app_secret: "s"
`
	access, cfg, data := writeSetup(t, existing, "")
	if access != weixinAccessUnset {
		t.Fatalf("access = %d, want weixinAccessUnset", access)
	}
	if strings.Contains(data, "im_access") {
		t.Errorf("im_access written into an existing config without an ID:\n%s", data)
	}
	if p := posture(t, cfg, "feishu"); !p.Open {
		t.Errorf("feishu posture = %+v, want open", p)
	}
}

func TestSetupWriteConfig_ExistingWeixinEntryUntouched(t *testing.T) {
	existing := `platforms:
  weixin:
    token: "old"
im_access:
  default_deny: true
  platforms:
    weixin:
      allowed_users: ["o_other@im.wechat"]
      admin_users: ["o_admin@im.wechat"]
`
	want := config.IMAccessRule{
		AllowedUsers: []string{"o_other@im.wechat"},
		AdminUsers:   []string{"o_admin@im.wechat"},
	}
	for _, tc := range []struct {
		name, userID string
		want         weixinAccess
	}{
		{"scanner not listed", scanUser, weixinAccessKeptWithout},
		{"scanner is an admin", "o_admin@im.wechat", weixinAccessKept},
		{"no ID", "", weixinAccessKept},
	} {
		t.Run(tc.name, func(t *testing.T) {
			access, cfg, _ := writeSetup(t, existing, tc.userID)
			if access != tc.want {
				t.Errorf("access = %d, want %d", access, tc.want)
			}
			got := cfg.IMAccess.Platforms["weixin"]
			if !slices.Equal(got.AllowedUsers, want.AllowedUsers) || !slices.Equal(got.AdminUsers, want.AdminUsers) {
				t.Errorf("weixin rule = %+v, want %+v", got, want)
			}
			if !cfg.IMAccess.DefaultDeny {
				t.Error("default_deny dropped")
			}
		})
	}
}

func TestSetupWriteConfig_ExistingIMAccessGainsWeixin(t *testing.T) {
	cases := map[string]string{
		"default_deny only": "im_access:\n  default_deny: true\n",
		"empty im_access":   "im_access:\n",
		"empty platforms":   "im_access:\n  platforms:\n",
		"other platform": `platforms:
  feishu:
    app_id: "abc"
    app_secret: "s"
im_access:
  platforms:
    feishu:
      allowed_users: ["ou_a"]
`,
	}
	for name, existing := range cases {
		t.Run(name, func(t *testing.T) {
			wantDeny := strings.Contains(existing, "default_deny: true")
			access, cfg, data := writeSetup(t, existing, scanUser)
			if access != weixinAccessAllowed {
				t.Fatalf("access = %d, want weixinAccessAllowed\n%s", access, data)
			}
			if got := cfg.IMAccess.Platforms["weixin"].AllowedUsers; !slices.Equal(got, []string{scanUser}) {
				t.Errorf("weixin allowed_users = %q\n%s", got, data)
			}
			if cfg.IMAccess.DefaultDeny != wantDeny {
				t.Errorf("default_deny = %v, want %v", cfg.IMAccess.DefaultDeny, wantDeny)
			}
			if strings.Contains(existing, "ou_a") && !slices.Equal(cfg.IMAccess.Platforms["feishu"].AllowedUsers, []string{"ou_a"}) {
				t.Errorf("feishu rule changed: %+v", cfg.IMAccess.Platforms["feishu"])
			}
		})
	}
}

func TestSetupWriteConfig_EmptyWeixinSectionGetsToken(t *testing.T) {
	_, cfg, data := writeSetup(t, "platforms:\n  weixin:\n", scanUser)
	if cfg.Platforms.Weixin == nil || cfg.Platforms.Weixin.Token != "wx-token" {
		t.Errorf("token not written under an empty weixin key:\n%s", data)
	}
}

func TestSetupStatusResp_DecodesILinkUserID(t *testing.T) {
	body := `{"status":"confirmed","bot_token":"tok","ilink_bot_id":"e06c@im.bot","ilink_user_id":"` + scanUser + `"}`
	var s setupStatusResp
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	if s.ILinkUserID != scanUser {
		t.Errorf("ILinkUserID = %q, want %q", s.ILinkUserID, scanUser)
	}
}

func TestPrintWeixinAccess(t *testing.T) {
	for _, tc := range []struct {
		access weixinAccess
		want   []string
	}{
		{weixinAccessAllowed, []string{"only accepts messages from " + scanUser}},
		{weixinAccessDefaultDeny, []string{"default_deny is on"}},
		{weixinAccessKept, []string{"kept the existing"}},
		{weixinAccessKeptWithout, []string{"does not list", scanUser}},
		{weixinAccessUnset, []string{"anyone can message", "allowed_users:"}},
	} {
		var buf bytes.Buffer
		printWeixinAccess(&buf, tc.access, scanUser)
		for _, w := range tc.want {
			if !strings.Contains(buf.String(), w) {
				t.Errorf("access %d: output %q lacks %q", tc.access, buf.String(), w)
			}
		}
	}
}
