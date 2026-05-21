package sysession

import (
	"strings"
	"testing"
)

// envContains returns true iff the "KEY=value" slice has key=want.
func envContains(env []string, key, want string) bool {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):] == want
		}
	}
	return false
}

// envHasKey returns true iff the slice has any value for key.
func envHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

func TestFilterEnv_AlwaysPassthrough(t *testing.T) {
	t.Setenv("PATH", "/foo")
	t.Setenv("HOME", "/home/x")
	t.Setenv("SECRET_TOKEN", "should-not-leak")

	env := filterEnv(nil)

	if !envContains(env, "PATH", "/foo") {
		t.Errorf("PATH must always pass through")
	}
	if !envContains(env, "HOME", "/home/x") {
		t.Errorf("HOME must always pass through")
	}
	if envHasKey(env, "SECRET_TOKEN") {
		t.Errorf("SECRET_TOKEN leaked into filtered env")
	}
}

func TestFilterEnv_ExactAllowlist(t *testing.T) {
	t.Setenv("PATH", "/foo")
	t.Setenv("HOME", "/h")
	t.Setenv("ALLOWED", "yes")
	t.Setenv("ALLOWED_PREFIXED", "extra")
	t.Setenv("OTHER", "no")

	env := filterEnv([]string{"ALLOWED"})

	if !envContains(env, "ALLOWED", "yes") {
		t.Errorf("exact-name allowlist entry must pass")
	}
	if envHasKey(env, "ALLOWED_PREFIXED") {
		t.Errorf("exact-name allowlist must NOT match by prefix")
	}
	if envHasKey(env, "OTHER") {
		t.Errorf("non-allowlisted key leaked")
	}
}

// TestFilterEnv_PrefixMatchesTrailingUnderscore covers the Bedrock /
// Anthropic / AWS plumbing case main.go relies on:  daemon Runner
// inherits everything matching "ANTHROPIC_*", "CLAUDE_*", "AWS_*" from
// the parent process so claude -p subprocesses can find the gateway
// URL + region the way the main session-spawn path does (cf.
// applyClaudeEnvSettings).
func TestFilterEnv_PrefixMatchesTrailingUnderscore(t *testing.T) {
	t.Setenv("PATH", "/foo")
	t.Setenv("HOME", "/h")
	t.Setenv("ANTHROPIC_BEDROCK_BASE_URL", "http://gateway")
	t.Setenv("ANTHROPIC_DEFAULT_OPUS_MODEL", "global.anthropic.claude-opus-4-7")
	t.Setenv("ANTHROPIC", "bare-key-should-not-match-prefix")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("UNRELATED_VAR", "should-not-leak")

	env := filterEnv([]string{"ANTHROPIC_", "CLAUDE_", "AWS_"})

	for _, want := range []struct{ k, v string }{
		{"ANTHROPIC_BEDROCK_BASE_URL", "http://gateway"},
		{"ANTHROPIC_DEFAULT_OPUS_MODEL", "global.anthropic.claude-opus-4-7"},
		{"CLAUDE_CODE_USE_BEDROCK", "1"},
		{"AWS_REGION", "us-west-2"},
	} {
		if !envContains(env, want.k, want.v) {
			t.Errorf("prefix-allowlist must propagate %s", want.k)
		}
	}
	// Bare "ANTHROPIC" without underscore should NOT match the
	// "ANTHROPIC_" prefix (trailing underscore is intentional — a
	// future bare-key collision would be a real regression).
	if envHasKey(env, "ANTHROPIC") {
		t.Errorf("bare ANTHROPIC must NOT match prefix ANTHROPIC_")
	}
	if envHasKey(env, "UNRELATED_VAR") {
		t.Errorf("UNRELATED_VAR leaked")
	}
}

func TestFilterEnv_MixedExactAndPrefix(t *testing.T) {
	t.Setenv("PATH", "/foo")
	t.Setenv("HOME", "/h")
	t.Setenv("HTTP_PROXY", "http://proxy")
	t.Setenv("ANTHROPIC_BEDROCK_BASE_URL", "http://gateway")
	t.Setenv("FOO_BAR", "no")

	env := filterEnv([]string{"HTTP_PROXY", "ANTHROPIC_"})

	if !envContains(env, "HTTP_PROXY", "http://proxy") {
		t.Errorf("exact HTTP_PROXY must pass")
	}
	if !envContains(env, "ANTHROPIC_BEDROCK_BASE_URL", "http://gateway") {
		t.Errorf("ANTHROPIC_ prefix must pass")
	}
	if envHasKey(env, "FOO_BAR") {
		t.Errorf("FOO_BAR leaked")
	}
}
