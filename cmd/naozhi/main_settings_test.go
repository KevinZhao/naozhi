package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
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
	data, err := readJSONWithRetry(context.Background(), path, 3, 1*time.Millisecond)
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
	_, err := readJSONWithRetry(context.Background(), path, 3, 1*time.Millisecond)
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
	_, err := readJSONWithRetry(context.Background(), path, 5, 500*time.Millisecond)
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
	data, err := readJSONWithRetry(context.Background(), path, 5, 100*time.Millisecond)
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
	if err := applyClaudeEnvSettings(context.Background()); err != nil {
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
	if err := applyClaudeEnvSettings(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("ANTHROPIC_MODEL"); got != "from-shell" {
		t.Errorf("shell value should win, got %q", got)
	}
}

func TestApplyClaudeEnvSettings_invalidJSONReturnsError(t *testing.T) {
	seedClaudeHome(t, `{"env": {"ANTHROPIC_MODEL":`)
	err := applyClaudeEnvSettings(context.Background())
	if err == nil {
		t.Fatal("expected error on invalid JSON, got nil")
	}
}

func TestApplyClaudeEnvSettings_missingFileReturnsError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	err := applyClaudeEnvSettings(context.Background())
	if err == nil {
		t.Fatal("expected error when settings.json missing")
	}
}

// TestApplyClaudeEnvSettings_errorTypeDistinguishesMissingVsCorrupt is the
// regression test for R236-QA-13. main() previously logged both file-missing
// and file-corrupt as Warn, so operators couldn't tell the difference between
// "first-run, settings file not yet created" (legitimate, no action) and
// "somebody hand-edited settings.json and broke it" (operator-actionable).
// The fix differentiates at the call site by checking errors.Is(fs.ErrNotExist).
// This test pins the error-type contract that makes that branching reliable
// regardless of how readJSONWithRetry wraps the underlying error in future.
func TestApplyClaudeEnvSettings_errorTypeDistinguishesMissingVsCorrupt(t *testing.T) {
	t.Run("missing file returns fs.ErrNotExist-compatible error", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		err := applyClaudeEnvSettings(context.Background())
		if err == nil {
			t.Fatal("expected error for missing file")
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing-file error should be fs.ErrNotExist-compatible (so main can route to Warn); got %v", err)
		}
	})

	t.Run("corrupt JSON returns non-NotExist error", func(t *testing.T) {
		seedClaudeHome(t, `{"env":`)
		err := applyClaudeEnvSettings(context.Background())
		if err == nil {
			t.Fatal("expected error for corrupt JSON")
		}
		if errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("corrupt-JSON error must NOT match fs.ErrNotExist (so main can route to Error); got %v", err)
		}
	})
}

func TestApplyClaudeEnvSettings_emptyEnvSectionNoError(t *testing.T) {
	seedClaudeHome(t, `{}`)
	if err := applyClaudeEnvSettings(context.Background()); err != nil {
		t.Fatalf("empty env section should not error: %v", err)
	}
}

// TestReadJSONWithRetry_ctxCancelAbortsRetry verifies that cancelling the context
// during a retry sleep returns ctx.Err() promptly — well before all retry sleeps
// would have elapsed — and does not block until the timer fires.
func TestReadJSONWithRetry_ctxCancelAbortsRetry(t *testing.T) {
	// Write invalid JSON so every read attempt fails and the retry sleep is entered.
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"truncated":`), 0600); err != nil {
		t.Fatal(err)
	}

	const attempts = 5
	const sleep = 500 * time.Millisecond // long enough to make timing visible

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel the context shortly after the first attempt fails and enters sleep.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := readJSONWithRetry(ctx, path, attempts, sleep)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// Without early abort, attempts×sleep = 2500ms. Allow a generous 300ms
	// to keep the test robust on slow CI while still proving early exit.
	if elapsed >= 300*time.Millisecond {
		t.Fatalf("ctx cancel should abort retry sleep early, took %v (want < 300ms)", elapsed)
	}
}
