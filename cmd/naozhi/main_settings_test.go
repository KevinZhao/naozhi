package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// seedClaudeHome creates a fake ~/.claude/settings.json under t.TempDir and
// points HOME at the tempdir for the duration of the test.
func seedClaudeHome(t *testing.T, contents string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	if contents != "" {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(contents), 0600); err != nil {
			t.Fatalf("write settings.json: %v", err)
		}
	}
	t.Setenv("HOME", home)
	return home
}

func TestReadJSONWithRetry_validFirstTry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.json")
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := readJSONWithRetry(path, 3, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("expected valid JSON, got %q", data)
	}
}

func TestReadJSONWithRetry_allAttemptsInvalidReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.json")
	// Write a truncated/invalid JSON string.
	if err := os.WriteFile(path, []byte(`{"ok":`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := readJSONWithRetry(path, 3, 1*time.Millisecond)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("want 'invalid JSON' in err, got %v", err)
	}
}

func TestReadJSONWithRetry_missingFileNoRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	start := time.Now()
	_, err := readJSONWithRetry(path, 5, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	// Should NOT retry on missing-file — 5 retries × 500ms would be ≥2s.
	// Allow generous headroom (100ms) to stay robust on slow CI.
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("missing-file path should skip retries, took %v", elapsed)
	}
	if !os.IsNotExist(err) {
		t.Fatalf("expected os.IsNotExist, got %v", err)
	}
}

// TestReadJSONWithRetry_eventuallyValid simulates the race: first read sees a
// truncated view, a concurrent writer finishes before the retry, second read
// sees valid JSON.
func TestReadJSONWithRetry_eventuallyValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.json")
	// Seed with invalid JSON.
	if err := os.WriteFile(path, []byte(`{"partial":`), 0600); err != nil {
		t.Fatal(err)
	}
	// Flip to valid JSON before second attempt. 50ms gives the first attempt
	// room to run while still undercutting the 100ms retry sleep.
	fixed := atomic.Bool{}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = os.WriteFile(path, []byte(`{"ok":true}`), 0600)
		fixed.Store(true)
	}()
	data, err := readJSONWithRetry(path, 5, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !fixed.Load() {
		t.Fatalf("test setup flake: retry succeeded before writer ran")
	}
	if !json.Valid(data) {
		t.Fatalf("expected valid JSON, got %q", data)
	}
}

func TestApplyClaudeEnvSettings_injectsOnlyAllowedPrefixes(t *testing.T) {
	seedClaudeHome(t, `{
  "env": {
    "CLAUDE_CODE_USE_BEDROCK": "1",
    "AWS_REGION": "ap-northeast-1",
    "ANTHROPIC_MODEL": "global.anthropic.claude-opus-4-7[1m]",
    "ENABLE_PROMPT_CACHING_1H": "1",
    "ECC_HOOK_PROFILE": "minimal"
  }
}`)
	// Make sure we start clean.
	for _, k := range []string{"CLAUDE_CODE_USE_BEDROCK", "AWS_REGION", "ANTHROPIC_MODEL", "ENABLE_PROMPT_CACHING_1H", "ECC_HOOK_PROFILE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if err := applyClaudeEnvSettings(); err != nil {
		t.Fatalf("applyClaudeEnvSettings: %v", err)
	}
	if got := os.Getenv("CLAUDE_CODE_USE_BEDROCK"); got != "1" {
		t.Errorf("CLAUDE_CODE_USE_BEDROCK = %q, want 1", got)
	}
	if got := os.Getenv("AWS_REGION"); got != "ap-northeast-1" {
		t.Errorf("AWS_REGION = %q", got)
	}
	if got := os.Getenv("ANTHROPIC_MODEL"); got == "" {
		t.Errorf("ANTHROPIC_MODEL should be set")
	}
	// Disallowed prefixes: must NOT leak into the process env.
	if got, ok := os.LookupEnv("ENABLE_PROMPT_CACHING_1H"); ok {
		t.Errorf("ENABLE_PROMPT_CACHING_1H leaked = %q", got)
	}
	if got, ok := os.LookupEnv("ECC_HOOK_PROFILE"); ok {
		t.Errorf("ECC_HOOK_PROFILE leaked = %q", got)
	}
}

func TestApplyClaudeEnvSettings_shellVarTakesPrecedence(t *testing.T) {
	seedClaudeHome(t, `{"env": {"ANTHROPIC_MODEL": "from-settings"}}`)
	t.Setenv("ANTHROPIC_MODEL", "from-shell")
	if err := applyClaudeEnvSettings(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("ANTHROPIC_MODEL"); got != "from-shell" {
		t.Errorf("shell value should win, got %q", got)
	}
}

func TestApplyClaudeEnvSettings_invalidJSONReturnsError(t *testing.T) {
	seedClaudeHome(t, `{"env": {"ANTHROPIC_MODEL":`)
	err := applyClaudeEnvSettings()
	if err == nil {
		t.Fatal("expected error on invalid JSON, got nil")
	}
}

func TestApplyClaudeEnvSettings_missingFileReturnsError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	err := applyClaudeEnvSettings()
	if err == nil {
		t.Fatal("expected error when settings.json missing")
	}
}

func TestApplyClaudeEnvSettings_emptyEnvSectionNoError(t *testing.T) {
	seedClaudeHome(t, `{}`)
	if err := applyClaudeEnvSettings(); err != nil {
		t.Fatalf("empty env section should not error: %v", err)
	}
}

// TestWriteClaudeSettingsOverride_preservesPreviousOnParseFail is the
// regression test for the observed bug: concurrent rewriter from Claude CLI
// makes readClaudeSettingsRaw fail, and the override file used to be
// overwritten with `{}` which stripped the `env` block and broke Bedrock auth.
// The fix must keep the prior good copy when the read fails.
func TestWriteClaudeSettingsOverride_preservesPreviousOnParseFail(t *testing.T) {
	home := seedClaudeHome(t, `{"env": {"CLAUDE_CODE_USE_BEDROCK":"1"}}`)

	// First run: settings.json is good. Override gets written.
	path := writeClaudeSettingsOverride(":8180")
	if path == "" {
		t.Fatal("first run: empty path")
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "CLAUDE_CODE_USE_BEDROCK") {
		t.Fatalf("first override missing env: %q", first)
	}

	// Corrupt settings.json mid-write (simulate race).
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"env":`), 0600); err != nil {
		t.Fatal(err)
	}

	// Second run: read fails after retries. Override must NOT be overwritten.
	path2 := writeClaudeSettingsOverride(":8180")
	if path2 == "" {
		t.Fatal("second run: empty path")
	}
	second, err := os.ReadFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(second), "CLAUDE_CODE_USE_BEDROCK") {
		t.Fatalf("override was overwritten with empty JSON: %q", second)
	}
}

func TestWriteClaudeSettingsOverride_firstRunWithCorruptSettingsFallsBackToEmpty(t *testing.T) {
	// No prior override, and settings.json is corrupt. Expected behavior: write
	// "{}" so --settings has a readable target; operator will see the warn log
	// and the "Not logged in" error from claude.
	seedClaudeHome(t, `{"env":`)
	path := writeClaudeSettingsOverride(":8180")
	if path == "" {
		t.Fatal("expected non-empty path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{}" {
		t.Fatalf("want {}, got %q", data)
	}
}

func TestWriteClaudeSettingsOverride_filtersNaozhiHooks(t *testing.T) {
	seedClaudeHome(t, `{
  "hooks": {
    "Stop": [
      {
        "matcher": "",
        "hooks": [
          {"type": "command", "command": "curl http://localhost:8180/api/naozhi-cb"}
        ]
      }
    ]
  },
  "env": {"CLAUDE_CODE_USE_BEDROCK": "1"}
}`)
	path := writeClaudeSettingsOverride(":8180")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Hook targeting naozhi port should be stripped.
	if strings.Contains(string(data), "naozhi-cb") {
		t.Errorf("naozhi callback hook not filtered: %q", data)
	}
	// env must still be present.
	if !strings.Contains(string(data), "CLAUDE_CODE_USE_BEDROCK") {
		t.Errorf("env section dropped: %q", data)
	}
}
