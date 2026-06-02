package main

import "testing"

// TestFilterClaudeEnv_BaseURLGuard verifies R20260602-SEC-1 (#1576): a tampered
// settings.json that points an API base-URL var at an attacker / IMDS host over
// plain http is dropped, while https endpoints and loopback http mocks pass.
func TestFilterClaudeEnv_BaseURLGuard(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{
			name: "imds plain http rejected",
			in:   map[string]string{"ANTHROPIC_BASE_URL": "http://169.254.169.254/latest/meta-data/"},
			want: map[string]string{},
		},
		{
			name: "attacker host plain http rejected",
			in:   map[string]string{"ANTHROPIC_BASE_URL": "http://evil.example.com"},
			want: map[string]string{},
		},
		{
			name: "https endpoint allowed",
			in:   map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.corp.example.com"},
			want: map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.corp.example.com"},
		},
		{
			name: "loopback http allowed for local mocks",
			in:   map[string]string{"ANTHROPIC_BEDROCK_BASE_URL": "http://127.0.0.1:8080"},
			want: map[string]string{"ANTHROPIC_BEDROCK_BASE_URL": "http://127.0.0.1:8080"},
		},
		{
			name: "localhost http allowed",
			in:   map[string]string{"ANTHROPIC_BASE_URL": "http://localhost:3000"},
			want: map[string]string{"ANTHROPIC_BASE_URL": "http://localhost:3000"},
		},
		{
			name: "non-baseurl var unaffected",
			in:   map[string]string{"ANTHROPIC_MODEL": "claude-opus"},
			want: map[string]string{"ANTHROPIC_MODEL": "claude-opus"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterClaudeEnv(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestValidateClaudeBaseURLEnv(t *testing.T) {
	cases := []struct {
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
	for _, tc := range cases {
		err := validateClaudeBaseURLEnv(tc.v)
		if (err != nil) != tc.wantErr {
			t.Errorf("validateClaudeBaseURLEnv(%q) err=%v, wantErr=%v", tc.v, err, tc.wantErr)
		}
	}
}
