package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
)

func TestSetupWriteConfig_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := setupWriteConfig(path, "test-token-123"); err != nil {
		t.Fatalf("setupWriteConfig: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "test-token-123") {
		t.Error("config should contain the token")
	}
	if !strings.Contains(content, "platforms:") {
		t.Error("config should contain platforms section")
	}
	if !strings.Contains(content, "weixin:") {
		t.Error("config should contain weixin section")
	}
	if !strings.Contains(content, "cli:") {
		t.Error("config should contain cli section")
	}

	// Check file permissions
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected 0600 permissions, got %o", info.Mode().Perm())
	}
}

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

	if err := setupWriteConfig(path, "new-token-456"); err != nil {
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

func TestSetupWriteConfig_AddWeixinSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	existing := `server:
  addr: ":8180"
platforms:
  feishu:
    app_id: "abc"
`
	if err := os.WriteFile(path, []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}

	if err := setupWriteConfig(path, "wx-token"); err != nil {
		t.Fatalf("setupWriteConfig: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	if !strings.Contains(content, "wx-token") {
		t.Error("token should be added")
	}
	if !strings.Contains(content, "weixin:") {
		t.Error("weixin section should be created")
	}
	if !strings.Contains(content, "feishu:") {
		t.Error("feishu section should be preserved")
	}
}

func TestSetupWriteConfig_CreateSubdirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "config.yaml")

	if err := setupWriteConfig(path, "token"); err != nil {
		t.Fatalf("setupWriteConfig: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Error("file should be created with parent dirs")
	}
}

func TestUpdateWeixinToken_NoPlatforms(t *testing.T) {
	input := []byte(`server:
  addr: ":8180"
log:
  level: "info"
`)
	result, err := updateWeixinToken(input, "my-token")
	if err != nil {
		t.Fatalf("updateWeixinToken: %v", err)
	}
	content := string(result)

	if !strings.Contains(content, "platforms:") {
		t.Error("should create platforms section")
	}
	if !strings.Contains(content, "weixin:") {
		t.Error("should create weixin section")
	}
	if !strings.Contains(content, "my-token") {
		t.Error("should contain token")
	}
}

// TestSetupWriteConfig_LoadableByConfig checks that the minimal template
// `naozhi setup weixin` emits parses cleanly through the production config
// pipeline (yaml.Unmarshal → applyDefaults). Without this regression, a
// future edit to defaultConfigTemplate that introduces invalid YAML, a
// removed struct field, or a required-but-missing value would only surface
// the first time an operator actually ran setup on a clean machine.
func TestSetupWriteConfig_LoadableByConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := setupWriteConfig(path, "wx-token"); err != nil {
		t.Fatalf("setupWriteConfig: %v", err)
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
	if cfg.CLI.Path != "claude" {
		t.Errorf("cli.path: got %q, want %q", cfg.CLI.Path, "claude")
	}
	if cfg.CLI.Model != "sonnet" {
		t.Errorf("cli.model: got %q, want %q", cfg.CLI.Model, "sonnet")
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

	if err := setupWriteConfig(path, "wx-token"); err != nil {
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
// adjacent YAML keys. All paths now funnel through updateWeixinToken
// (yaml.Node + DoubleQuotedStyle) which emits a correctly escaped
// scalar no matter what bytes the token carries.
func TestSetupWriteConfig_TokenWithYAMLSpecials(t *testing.T) {
	// NUL intentionally left out — yaml.v3 rejects C0 controls during
	// Encode. These are the YAML-meaningful specials that would break
	// the old fmt.Sprintf path.
	cases := map[string]string{
		"embedded_dquote": `tok"injected: "evil`,
		"hash_comment":    `tok#comment`,
		"colon":           `tok:evil`,
		"newline":         "tok\ninjected: true",
		"tab":             "tok\twith\ttabs",
		"backslash":       `tok\escape`,
	}
	for name, token := range cases {
		name, token := name, token
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")

			if err := setupWriteConfig(path, token); err != nil {
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

// TestDefaultConfigTemplate_TokenPlaceholderEmpty verifies that the
// minimal template is now a static YAML document (no %s anywhere) so
// `yaml.Unmarshal` can parse it directly. The token is injected
// exclusively through updateWeixinToken to guarantee safe escaping.
func TestDefaultConfigTemplate_TokenPlaceholderEmpty(t *testing.T) {
	if strings.Contains(defaultConfigTemplate, "%s") {
		t.Error("defaultConfigTemplate must not use fmt placeholder; " +
			"token must go through updateWeixinToken for YAML-safe escaping")
	}
	if !strings.Contains(defaultConfigTemplate, `token: ""`) {
		t.Error("defaultConfigTemplate should contain empty token anchor " +
			`token: "" for updateWeixinToken to overwrite`)
	}
}

// TestDefaultConfigTemplate_Minimal locks in the intent that the template is
// kept minimal. If someone re-adds a key that applyDefaults already handles,
// this test fails and forces the author to add it to the runtime-defaults
// test above instead of creating a second source of truth.
func TestDefaultConfigTemplate_Minimal(t *testing.T) {
	// The keys explicitly forbidden in the minimal template because
	// config.applyDefaults owns them.
	forbidden := []string{
		"max_procs:",
		"ttl:",
		"prune_ttl:",
		"store_path:",
	}
	for _, k := range forbidden {
		if strings.Contains(defaultConfigTemplate, k) {
			t.Errorf("defaultConfigTemplate contains %q — applyDefaults owns this key; remove from template to prevent drift", k)
		}
	}

	// Required anchors users will want to edit or that setup produces.
	required := []string{
		"cli:",
		"platforms:",
		"weixin:",
		"token:",
	}
	for _, k := range required {
		if !strings.Contains(defaultConfigTemplate, k) {
			t.Errorf("defaultConfigTemplate missing anchor %q", k)
		}
	}
}
