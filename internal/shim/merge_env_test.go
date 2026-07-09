package shim

import (
	"strings"
	"testing"
)

// envMap parses a "KEY=value" slice into a map for order-independent assertions.
func envMap(kvs []string) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

func TestMergeShimEnv_NilOverlayReturnsBaseline(t *testing.T) {
	baseline := []string{"ANTHROPIC_BASE_URL=https://api.anthropic.com", "AWS_REGION=us-east-1"}
	got := mergeShimEnv(baseline, nil)
	// Byte-identical: nil overlay must be a no-op (same backing slice is fine).
	if len(got) != len(baseline) {
		t.Fatalf("nil overlay changed length: got %d want %d", len(got), len(baseline))
	}
	got = mergeShimEnv(baseline, map[string]string{})
	if len(got) != len(baseline) {
		t.Fatalf("empty overlay changed length: got %d want %d", len(got), len(baseline))
	}
}

func TestMergeShimEnv_OverlayOverridesBaselineValue(t *testing.T) {
	baseline := []string{
		"CLAUDE_CODE_USE_BEDROCK=1",
		"ANTHROPIC_BEDROCK_BASE_URL=http://127.0.0.1:8889",
		"LD_PRELOAD=/tmp/x.so", // not in allowlist — dropped by filterShimEnv
	}
	overlay := map[string]string{
		"CLAUDE_CODE_USE_BEDROCK": "0",
		"ANTHROPIC_BASE_URL":      "https://api.anthropic.com",
	}
	m := envMap(mergeShimEnv(baseline, overlay))
	if m["CLAUDE_CODE_USE_BEDROCK"] != "0" {
		t.Errorf("overlay did not override selector: got %q", m["CLAUDE_CODE_USE_BEDROCK"])
	}
	if m["ANTHROPIC_BASE_URL"] != "https://api.anthropic.com" {
		t.Errorf("overlay-introduced key missing: got %q", m["ANTHROPIC_BASE_URL"])
	}
	// The 1P switch: overlay set BEDROCK=0 and added the direct base URL; the
	// baseline bedrock URL survives only because it's allowlisted, but the
	// selector is now off.
	if _, ok := m["LD_PRELOAD"]; ok {
		t.Errorf("non-allowlisted baseline key LD_PRELOAD leaked through filterShimEnv")
	}
}

func TestMergeShimEnv_OverlayStillGated(t *testing.T) {
	baseline := []string{"AWS_REGION=us-east-1"}
	tests := []struct {
		name       string
		overlay    map[string]string
		wantAbsent string // key that must NOT appear in the result
	}{
		{
			name:       "non-allowlisted key dropped",
			overlay:    map[string]string{"LD_PRELOAD": "/tmp/evil.so"},
			wantAbsent: "LD_PRELOAD",
		},
		{
			name:       "IMDS base url dropped",
			overlay:    map[string]string{"ANTHROPIC_BASE_URL": "http://169.254.169.254"},
			wantAbsent: "ANTHROPIC_BASE_URL",
		},
		{
			name:       "plain-http non-loopback base url dropped",
			overlay:    map[string]string{"ANTHROPIC_BASE_URL": "http://evil.example.com"},
			wantAbsent: "ANTHROPIC_BASE_URL",
		},
		{
			name:       "unsafe AWS profile value dropped",
			overlay:    map[string]string{"AWS_PROFILE": "../../etc/passwd"},
			wantAbsent: "AWS_PROFILE",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := envMap(mergeShimEnv(baseline, tt.overlay))
			if _, ok := m[tt.wantAbsent]; ok {
				t.Errorf("overlay bypassed filterShimEnv: %q present in result %v", tt.wantAbsent, m)
			}
			// Baseline safe key survives.
			if m["AWS_REGION"] != "us-east-1" {
				t.Errorf("baseline AWS_REGION lost: got %q", m["AWS_REGION"])
			}
		})
	}
}

func TestMergeShimEnv_LoopbackBedrockOverlayAllowed(t *testing.T) {
	// The company-Bedrock profile: overlay points at the local proxy port.
	m := envMap(mergeShimEnv(nil, map[string]string{
		"CLAUDE_CODE_USE_BEDROCK":    "1",
		"ANTHROPIC_BEDROCK_BASE_URL": "http://127.0.0.1:8890",
		"AWS_REGION":                 "us-west-2",
	}))
	if m["ANTHROPIC_BEDROCK_BASE_URL"] != "http://127.0.0.1:8890" {
		t.Errorf("loopback bedrock url should pass: got %q", m["ANTHROPIC_BEDROCK_BASE_URL"])
	}
	if m["CLAUDE_CODE_USE_BEDROCK"] != "1" || m["AWS_REGION"] != "us-west-2" {
		t.Errorf("bedrock selector/region dropped: %v", m)
	}
}
