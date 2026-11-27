package main

import "testing"

// TestFilterClaudeEnv_URLGuards verifies R20260602-SEC-1 (#1576): a tampered
// settings.json that points an API base-URL var at an attacker / IMDS host over
// plain http is dropped, while https endpoints and loopback http mocks pass.
// Proxy env vars get the same guard (R20260603-SEC-5, #1660); NO_PROXY is not a
// URL and passes through unmodified.
func TestFilterClaudeEnv_URLGuards(t *testing.T) {
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
		{
			name: "remote plaintext http proxy rejected",
			in:   map[string]string{"HTTPS_PROXY": "http://attacker.example.com:3128"},
			want: map[string]string{},
		},
		{
			name: "lowercase remote http proxy rejected",
			in:   map[string]string{"https_proxy": "http://evil.test"},
			want: map[string]string{},
		},
		{
			name: "https proxy allowed",
			in:   map[string]string{"HTTPS_PROXY": "https://proxy.corp.example.com:443"},
			want: map[string]string{"HTTPS_PROXY": "https://proxy.corp.example.com:443"},
		},
		{
			name: "loopback http proxy allowed",
			in:   map[string]string{"HTTP_PROXY": "http://127.0.0.1:8888"},
			want: map[string]string{"HTTP_PROXY": "http://127.0.0.1:8888"},
		},
		{
			name: "no_proxy passes through untouched",
			in:   map[string]string{"NO_PROXY": "localhost,.internal"},
			want: map[string]string{"NO_PROXY": "localhost,.internal"},
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
