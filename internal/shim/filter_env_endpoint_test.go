package shim

import "testing"

// TestFilterShimEnv_EndpointSSRFGuard pins R20260602-SEC-1 (#1576, shim
// sibling): endpoint base-URL env vars pointing at a plain-http non-loopback
// host (attacker / IMDS) must be dropped before reaching the CLI subprocess,
// while https endpoints and loopback http mocks pass through. Non-endpoint
// allowlisted vars are unaffected.
func TestFilterShimEnv_EndpointSSRFGuard(t *testing.T) {
	t.Parallel()
	input := []string{
		"HOME=/home/user",                              // allowed, non-URL
		"ANTHROPIC_API_KEY=sk-test",                    // allowed, non-URL
		"ANTHROPIC_BASE_URL=http://169.254.169.254",    // IMDS plain http — drop
		"ANTHROPIC_BEDROCK_BASE_URL=http://evil.test",  // attacker plain http — drop
		"AWS_ENDPOINT_URL=https://gw.corp.example.com", // https — keep
		"AWS_BEDROCK_ENDPOINT=http://127.0.0.1:8080",   // loopback http — keep
	}
	got := filterShimEnv(input)
	gotSet := map[string]bool{}
	for _, kv := range got {
		gotSet[kv] = true
	}

	mustKeep := []string{
		"HOME=/home/user",
		"ANTHROPIC_API_KEY=sk-test",
		"AWS_ENDPOINT_URL=https://gw.corp.example.com",
		"AWS_BEDROCK_ENDPOINT=http://127.0.0.1:8080",
	}
	for _, kv := range mustKeep {
		if !gotSet[kv] {
			t.Errorf("expected %q to be kept, but it was dropped; got=%v", kv, got)
		}
	}
	mustDrop := []string{
		"ANTHROPIC_BASE_URL=http://169.254.169.254",
		"ANTHROPIC_BEDROCK_BASE_URL=http://evil.test",
	}
	for _, kv := range mustDrop {
		if gotSet[kv] {
			t.Errorf("expected %q to be dropped (SSRF guard), but it leaked through", kv)
		}
	}
}

func TestValidateShimEndpointURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v       string
		wantErr bool
	}{
		{"https://api.anthropic.com", false},
		{"http://127.0.0.1:9000", false},
		{"http://[::1]:9000", false},
		{"http://localhost", false},
		{"http://169.254.169.254", true},
		{"http://attacker.test", true},
		{"ftp://host", true},
	}
	for _, tc := range cases {
		err := validateShimEndpointURL(tc.v)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateShimEndpointURL(%q) err=%v, wantErr=%v", tc.v, err, tc.wantErr)
		}
	}
}
