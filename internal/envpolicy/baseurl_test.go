package envpolicy

import "testing"

// TestValidateBaseURLValue covers the allow/deny boundary for base-URL env
// values (RFC §4). R090031-SEC-1 (#1687) / R20260602-SEC-1 (#1576).
func TestValidateBaseURLValue(t *testing.T) {
	cases := []struct {
		name    string
		v       string
		wantErr bool
	}{
		{"empty clears var", "", false},
		{"https allowed", "https://api.anthropic.com", false},
		{"https with path", "https://bedrock.example.com/v1", false},
		{"loopback http localhost", "http://localhost:8080", false},
		{"loopback http 127.0.0.1", "http://127.0.0.1:9000", false},
		{"loopback http ::1", "http://[::1]:8080", false},
		{"localhost no port", "http://localhost", false},
		{"imds rejected", "http://169.254.169.254/latest/meta-data/", true},
		{"https imds rejected", "https://169.254.169.254/latest/meta-data/", true},
		{"https link-local v6 rejected", "https://[fe80::1]/", true},
		{"http link-local v6 rejected", "http://[fe80::1]/", true},
		{"https normal", "https://api.anthropic.com", false},
		{"private net plaintext rejected", "http://10.0.0.1", true},
		{"remote plaintext rejected", "http://example.com", true},
		{"remote plaintext attacker", "http://attacker.test", true},
		{"ftp scheme rejected", "ftp://example.com", true},
		{"file scheme rejected", "file:///etc/passwd", true},
		{"unparseable rejected", "://bad", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBaseURLValue(tc.v)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateBaseURLValue(%q) err=%v, wantErr=%v", tc.v, err, tc.wantErr)
			}
		})
	}
}

// TestValidateBaseURLValue_MatchesLegacyTable replays the exact input tables
// that lived in the two pre-#891 callers (sysession TestValidateBaseURLValue
// and cmd TestValidateClaudeBaseURLEnv) to pin that the consolidated function
// makes the identical allow/deny decision case-for-case.
func TestValidateBaseURLValue_MatchesLegacyTable(t *testing.T) {
	// From sysession/env_test.go TestValidateBaseURLValue.
	legacyOK := []string{
		"",
		"https://api.anthropic.com",
		"https://bedrock.example.com/v1",
		"http://localhost:8080",
		"http://127.0.0.1:9000",
		"http://[::1]:8080",
	}
	legacyBad := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1",
		"http://example.com",
		"ftp://example.com",
		"file:///etc/passwd",
	}
	for _, v := range legacyOK {
		if err := ValidateBaseURLValue(v); err != nil {
			t.Errorf("legacy-ok %q: got err=%v, want nil", v, err)
		}
	}
	for _, v := range legacyBad {
		if err := ValidateBaseURLValue(v); err == nil {
			t.Errorf("legacy-bad %q: got nil, want error", v)
		}
	}

	// From cmd/naozhi/main_claude_baseurl_env_test.go TestValidateClaudeBaseURLEnv.
	cmdTable := []struct {
		v       string
		wantErr bool
	}{
		{"", false},
		{"https://api.anthropic.com", false},
		{"http://127.0.0.1:9000", false},
		{"http://[::1]:9000", false},
		{"http://localhost", false},
		{"http://169.254.169.254", true},
		{"http://attacker.test", true},
		{"ftp://host", true},
		{"://bad", true},
	}
	for _, tc := range cmdTable {
		if got := ValidateBaseURLValue(tc.v) != nil; got != tc.wantErr {
			t.Errorf("cmd-legacy %q: gotErr=%v, wantErr=%v", tc.v, got, tc.wantErr)
		}
	}
}
