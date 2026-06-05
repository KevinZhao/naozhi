package envpolicy

import (
	"slices"
	"testing"
)

// TestEnvTruthy covers the loose-truthiness selector boundary (RFC §4).
func TestEnvTruthy(t *testing.T) {
	off := []string{"", "0", "false", "no", "off", "FALSE", " 0 ", "Off", "  "}
	for _, v := range off {
		if EnvTruthy(v) {
			t.Errorf("EnvTruthy(%q) = true, want false", v)
		}
	}
	on := []string{"1", "true", "yes", "on", "anything", "TRUE", " 1 "}
	for _, v := range on {
		if !EnvTruthy(v) {
			t.Errorf("EnvTruthy(%q) = false, want true", v)
		}
	}
}

// TestDetectBackendFromEnv covers the three-state detection plus Bedrock>Vertex
// precedence and the disabled-selector fallback (RFC §4).
func TestDetectBackendFromEnv(t *testing.T) {
	cases := []struct {
		name   string
		parent []string
		want   BackendMode
	}{
		{"no selectors -> anthropic", nil, BackendAnthropic},
		{"empty selectors -> anthropic", []string{"CLAUDE_CODE_USE_BEDROCK=", "CLAUDE_CODE_USE_VERTEX="}, BackendAnthropic},
		{"bedrock truthy", []string{"CLAUDE_CODE_USE_BEDROCK=1"}, BackendBedrock},
		{"vertex truthy", []string{"CLAUDE_CODE_USE_VERTEX=true"}, BackendVertex},
		{"bedrock wins over vertex", []string{"CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_USE_VERTEX=1"}, BackendBedrock},
		{"bedrock=0 falls back to anthropic", []string{"CLAUDE_CODE_USE_BEDROCK=0"}, BackendAnthropic},
		{"bedrock=0 vertex=1 -> vertex", []string{"CLAUDE_CODE_USE_BEDROCK=0", "CLAUDE_CODE_USE_VERTEX=1"}, BackendVertex},
		{"ignores unrelated keys", []string{"PATH=/bin", "CLAUDE_CODE_USE_VERTEX=yes"}, BackendVertex},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectBackendFromEnv(tc.parent); got != tc.want {
				t.Errorf("DetectBackendFromEnv(%v) = %d, want %d", tc.parent, got, tc.want)
			}
		})
	}
}

// TestEnvCredsForBackend pins each backend's credential key set (RFC §4).
func TestEnvCredsForBackend(t *testing.T) {
	cases := []struct {
		mode BackendMode
		want []string
	}{
		{BackendAnthropic, []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}},
		{BackendBedrock, []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"}},
		{BackendVertex, []string{"GOOGLE_APPLICATION_CREDENTIALS"}},
	}
	for _, tc := range cases {
		got := EnvCredsForBackend(tc.mode)
		if !slices.Equal(got, tc.want) {
			t.Errorf("EnvCredsForBackend(%d) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

// TestAllCredKeys verifies AllCredKeys is the union of every backend's keys.
func TestAllCredKeys(t *testing.T) {
	want := []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		"GOOGLE_APPLICATION_CREDENTIALS",
	}
	if !slices.Equal(AllCredKeys, want) {
		t.Errorf("AllCredKeys = %v, want %v", AllCredKeys, want)
	}
	// Every backend's creds must appear in the union.
	for _, mode := range []BackendMode{BackendAnthropic, BackendBedrock, BackendVertex} {
		for _, k := range EnvCredsForBackend(mode) {
			if !slices.Contains(AllCredKeys, k) {
				t.Errorf("AllCredKeys missing %q from backend %d", k, mode)
			}
		}
	}
}
