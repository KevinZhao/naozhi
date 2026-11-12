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

// The dashboard's Isengard template writes aws_profile through the same
// validation as load, and it reads back.
func TestAppendAccessProfile_AWSProfile(t *testing.T) {
	p := writeBaseConfig(t)
	if err := AppendAccessProfile(p, "iso", AccessProfile{AWSProfile: "team-iso", Env: map[string]string{"AWS_REGION": "us-west-2"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := cfg.AccessProfiles["iso"].AWSProfile; got != "team-iso" {
		t.Errorf("aws_profile after reload = %q, want team-iso", got)
	}
	before, _ := os.ReadFile(p)
	if err := AppendAccessProfile(p, "bad", AccessProfile{AWSProfile: "$(id)"}); err == nil {
		t.Error("append accepted an unsafe aws_profile")
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Error("a rejected aws_profile still changed config.yaml")
	}
}

// An edit sets only the dashboard's fields: env, a key it does not know and
// the file's comments stay, the old file is backed up and the mode is kept.
func TestUpdateAccessProfile_KeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	body := baseConfigYAML + `# the team account
access_profiles:
  team:
    display_name: "Team"
    chip_color: "#123456"
    future_key: "kept"
    env:
      AWS_REGION: "us-west-2"
`
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	err := UpdateAccessProfile(p, "team", AccessProfileFields{DisplayName: "Team B", AWSProfile: "team-iso", DefaultModel: "claude-opus-5-5[1m]"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	data, _ := os.ReadFile(p)
	out := string(data)
	for _, want := range []string{"# the team account", `future_key: "kept"`, `AWS_REGION: "us-west-2"`, `display_name: "Team B"`, `aws_profile: "team-iso"`} {
		if !strings.Contains(out, want) {
			t.Errorf("updated config lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "chip_color") {
		t.Errorf("an emptied field is still there:\n%s", out)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640 kept", fi.Mode().Perm())
	}
	baks, _ := filepath.Glob(p + ".bak-*")
	if len(baks) != 1 {
		t.Fatalf("backups = %v, want one", baks)
	}
	if old, _ := os.ReadFile(baks[0]); string(old) != body {
		t.Error("the backup is not the old file")
	}
	if err := UpdateAccessProfile(p, "nope", AccessProfileFields{}); err == nil {
		t.Error("editing a profile that is not in the file succeeded")
	}
}

// Removing a profile drops it; one default_access_profile names is refused.
func TestRemoveAccessProfile(t *testing.T) {
	p := writeBaseConfig(t)
	for _, id := range []string{"a", "b"} {
		if err := AppendAccessProfile(p, id, AccessProfile{DisplayName: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveAccessProfile(p, "a"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.AccessProfiles["a"]; ok || len(cfg.AccessProfiles) != 1 {
		t.Errorf("profiles after remove = %v", cfg.AccessProfiles)
	}
	data, _ := os.ReadFile(p)
	if err := os.WriteFile(p, append(data, []byte("default_access_profile: b\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAccessProfile(p, "b"); err == nil || !strings.Contains(err.Error(), "default_access_profile") {
		t.Errorf("removing the default profile: err = %v", err)
	}
}
