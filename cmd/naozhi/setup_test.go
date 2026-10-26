package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
)

func TestSetupWriteConfig_UpdateExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	existing := `server:
  addr: ":9090"

# My comment
platforms:
  feishu:
    app_id: "feishu-123"
  weixin:
    token: "old-token"

log:
  level: "debug"
`
	if err := os.WriteFile(path, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := setupWriteConfig(path, "new-token-456", "", ""); err != nil {
		t.Fatalf("setupWriteConfig: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	// Token updated
	if !strings.Contains(content, "new-token-456") {
		t.Error("token should be updated")
	}
	if strings.Contains(content, "old-token") {
		t.Error("old token should be replaced")
	}

	// Existing config preserved
	if !strings.Contains(content, "feishu-123") {
		t.Error("feishu config should be preserved")
	}
	if !strings.Contains(content, ":9090") {
		t.Error("server addr should be preserved")
	}
	if !strings.Contains(content, "debug") {
		t.Error("log level should be preserved")
	}
}

// TestSetupWriteConfig_LoadableByConfig checks that the minimal template
// `naozhi setup weixin` emits parses cleanly through the production config
// pipeline (yaml.Unmarshal → applyDefaults). Without this regression, a
// future edit to defaultConfigTemplate that introduces invalid YAML, a
// removed struct field, or a required-but-missing value would only surface
// the first time an operator actually ran setup on a clean machine.
func TestSetupWriteConfig_LoadableByConfig(t *testing.T) {
	// A missing parent directory (fresh machine, no ~/.naozhi yet) is created.
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")

	if _, err := setupWriteConfig(path, "wx-token", "", "https://sh.ilinkai.weixin.qq.com"); err != nil {
		t.Fatalf("setupWriteConfig: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config file stat = %v, %v; want mode 0600", info, err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(%s) on setup-generated file: %v", path, err)
	}
	if cfg == nil {
		t.Fatal("config.Load returned nil cfg")
	}

	if cfg.Platforms.Weixin == nil || cfg.Platforms.Weixin.Token != "wx-token" {
		t.Errorf("weixin token round-trip: got %+v, want token=wx-token",
			cfg.Platforms.Weixin)
	}
	if cfg.Platforms.Weixin != nil && cfg.Platforms.Weixin.BaseURL != "https://sh.ilinkai.weixin.qq.com" {
		t.Errorf("weixin base_url round-trip: got %q", cfg.Platforms.Weixin.BaseURL)
	}
	if cfg.CLI.Path != "claude" {
		t.Errorf("cli.path: got %q, want %q", cfg.CLI.Path, "claude")
	}
	// cli.model is intentionally empty in the generated template so naozhi
	// inherits the model from ~/.claude/settings.json (via --setting-sources
	// user), matching the TUI claude. A non-empty value here would force a
	// model that overrides the user's settings.json for every naozhi session.
	if cfg.CLI.Model != "" {
		t.Errorf("cli.model: got %q, want %q (empty = inherit from settings.json)", cfg.CLI.Model, "")
	}
}

// TestSetupWriteConfig_AppliesRuntimeDefaults is the contract counterpart to
// the above test: all the keys that USED to live in defaultConfigTemplate
// (server.addr, session.{ttl,max_procs,prune_ttl,store_path}, log.level,
// session.queue.{max_depth,collect_delay,mode}) must still be populated by
// the time Load() returns. If applyDefaults stops filling one of these in
// (or someone re-adds it to the template creating a drift source again),
// the failing assertion points directly at the broken key.
func TestSetupWriteConfig_AppliesRuntimeDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if _, err := setupWriteConfig(path, "wx-token", "", ""); err != nil {
		t.Fatalf("setupWriteConfig: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	// Fields previously hard-coded in defaultConfigTemplate — must be
	// populated post-applyDefaults, not left at zero values.
	checks := []struct {
		name string
		got  any
		zero any
	}{
		{"server.addr", cfg.Server.Addr, ""},
		{"session.ttl", cfg.Session.TTL, ""},
		{"session.prune_ttl", cfg.Session.PruneTTL, ""},
		{"session.max_procs", cfg.Session.MaxProcs, 0},
		{"session.queue.collect_delay", cfg.Session.Queue.CollectDelay, ""},
		{"session.queue.mode", cfg.Session.Queue.Mode, ""},
		{"log.level", cfg.Log.Level, ""},
		{"workspace.id", cfg.Workspace.ID, ""},
		{"workspace.name", cfg.Workspace.Name, ""},
	}
	for _, c := range checks {
		if fmt.Sprintf("%v", c.got) == fmt.Sprintf("%v", c.zero) {
			t.Errorf("%s: got zero value %v, applyDefaults should have filled it", c.name, c.got)
		}
	}

	// Queue.MaxDepth is a *int; a nil pointer means applyDefaults did not run.
	if cfg.Session.Queue.MaxDepth == nil {
		t.Error("session.queue.max_depth: got nil pointer, applyDefaults should have set default 20")
	} else if *cfg.Session.Queue.MaxDepth != 20 {
		t.Errorf("session.queue.max_depth: got %d, want 20", *cfg.Session.Queue.MaxDepth)
	}
}

// TestSetupWriteConfig_TokenWithYAMLSpecials covers R172-SEC-M3: a
// compromised or buggy vendor QR endpoint could return a token
// containing YAML specials (`"`, `#`, `:`, `\n`). The old fast path
// substituted the token via fmt.Sprintf into a template quoted with
// raw double-quotes, so a `"` or newline would truncate / inject
// adjacent YAML keys. All paths now funnel through updateWeixinConfig
// (yaml.Node + DoubleQuotedStyle) which emits a correctly escaped
// scalar no matter what bytes the token carries.
func TestSetupWriteConfig_TokenWithYAMLSpecials(t *testing.T) {
	// NUL intentionally left out — yaml.v3 rejects C0 controls during
	// Encode. These are the YAML-meaningful specials that would break
	// the old fmt.Sprintf path.
	cases := map[string]string{
		"embedded_dquote": `tok"injected: "evil`,
		"newline":         "tok\ninjected: true",
	}
	for name, token := range cases {
		name, token := name, token
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")

			if _, err := setupWriteConfig(path, token, "", ""); err != nil {
				t.Fatalf("setupWriteConfig: %v", err)
			}

			// Round-trip through the production config loader — if the
			// token bytes produced an "injected: evil" key, the resulting
			// YAML would either fail to parse, decode to a different
			// token, or surface an unexpected `admin: true` style field.
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("config.Load(setup-generated): %v", err)
			}
			if cfg.Platforms.Weixin == nil {
				t.Fatal("weixin section missing after round-trip")
			}
			if cfg.Platforms.Weixin.Token != token {
				t.Errorf("token round-trip mismatch: got %q, want %q",
					cfg.Platforms.Weixin.Token, token)
			}
		})
	}
}
