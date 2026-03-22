package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
