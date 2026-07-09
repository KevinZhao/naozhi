package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const baseConfigYAML = `server:
  addr: "127.0.0.1:8080"
cli:
  backend: claude
  path: /usr/bin/claude
`

func writeBaseConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(baseConfigYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAppendAccessProfile_AddsAndReloads(t *testing.T) {
	p := writeBaseConfig(t)
	ap := AccessProfile{
		DisplayName:  "Bedrock · Opus",
		ChipColor:    "#7c5cff",
		DefaultModel: "claude-opus-4-8",
		Env: map[string]string{
			"CLAUDE_CODE_USE_BEDROCK":    "1",
			"ANTHROPIC_BEDROCK_BASE_URL": "http://127.0.0.1:8889",
		},
	}
	if err := AppendAccessProfile(p, "bedrock-opus", ap); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Reload via the normal loader and confirm the profile round-trips + still
	// validates (Load runs validateConfig which includes validateAccessProfiles).
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := cfg.AccessProfiles["bedrock-opus"]
	if !ok {
		t.Fatal("profile not present after reload")
	}
	if got.DisplayName != "Bedrock · Opus" || got.DefaultModel != "claude-opus-4-8" {
		t.Errorf("fields wrong: %+v", got)
	}
	if got.Env["CLAUDE_CODE_USE_BEDROCK"] != "1" || got.Env["ANTHROPIC_BEDROCK_BASE_URL"] != "http://127.0.0.1:8889" {
		t.Errorf("env wrong: %v", got.Env)
	}
	// Original keys preserved.
	if cfg.CLI.Backend != "claude" {
		t.Errorf("unrelated key clobbered: cli.backend=%q", cfg.CLI.Backend)
	}
}

func TestAppendAccessProfile_RejectsDuplicate(t *testing.T) {
	p := writeBaseConfig(t)
	ap := AccessProfile{DisplayName: "X"}
	if err := AppendAccessProfile(p, "dup", ap); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := AppendAccessProfile(p, "dup", ap); err == nil {
		t.Fatal("second append should reject duplicate id")
	}
}

func TestAppendAccessProfile_RejectsBadEnv(t *testing.T) {
	p := writeBaseConfig(t)
	cases := map[string]map[string]string{
		"non-overlay key": {"AWS_PROFILE": "admin"},
		"SSRF base url":   {"ANTHROPIC_BASE_URL": "http://169.254.169.254"},
		"arbitrary env":   {"LD_PRELOAD": "/tmp/x.so"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if err := AppendAccessProfile(p, "bad", AccessProfile{Env: env}); err == nil {
				t.Errorf("expected env rejection for %s", name)
			}
		})
	}
}

func TestAppendAccessProfile_RejectsBadID(t *testing.T) {
	p := writeBaseConfig(t)
	for _, id := range []string{"", "-lead", "has space", "sha/slash", strings.Repeat("a", 65)} {
		if err := AppendAccessProfile(p, id, AccessProfile{}); err == nil {
			t.Errorf("id %q should be rejected", id)
		}
	}
}

func TestWriteSecretFile_Mode0600(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "tok.token")
	if err := WriteSecretFile(p, "sk-secret-123\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(p)
	if string(data) != "sk-secret-123\n" {
		t.Errorf("content mismatch: %q", data)
	}
}

func TestWriteSecretFile_RejectsRelative(t *testing.T) {
	if err := WriteSecretFile("relative/path", "x"); err == nil {
		t.Fatal("relative path should be rejected")
	}
}
