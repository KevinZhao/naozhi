package main

import "testing"

// TestIsNaozhiCallbackHook_NoFalsePositiveOn127Substring pins R236-QA-20 (#544):
// the original substring check `strings.Contains(lower, "127.")` matched any
// command containing the literal "127." even if it wasn't a 127/8 IPv4 address
// (e.g. a hostname starting with "127" but in a different TLD, or a "version
// 127." marketing string), provided the command also mentioned ":<port>"
// elsewhere. The regex-based check requires a real dotted-quad shape next to
// a host:port boundary so legitimate hooks survive while real loopback URLs
// are still flagged.
func TestIsNaozhiCallbackHook_NoFalsePositiveOn127Substring(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		port string
		want bool
	}{
		// Real positives that must keep firing.
		{
			name: "127.0.0.1:port positive",
			cmd:  "curl http://127.0.0.1:8180/hook",
			port: "8180",
			want: true,
		},
		{
			name: "loopback 127.5.6.7:port still loopback",
			cmd:  "curl http://127.5.6.7:8180/hook",
			port: "8180",
			want: true,
		},
		{
			name: "literal naozhi mention positive",
			cmd:  "/opt/naozhi/bin/foo --flag",
			port: "",
			want: true,
		},
		{
			name: "[::1]:port positive",
			cmd:  "curl -X POST http://[::1]:8180/notify",
			port: "8180",
			want: true,
		},

		// False-positive cases that the substring-only check used to flunk.
		{
			name: "hostname containing 127 not loopback",
			cmd:  "curl https://foo127.example.com:8180/api",
			port: "8180",
			want: false,
		},
		{
			name: "marketing string mentioning 127 then port elsewhere",
			cmd:  "echo 'version 127. installed' && curl https://api.example.com:8180",
			port: "8180",
			want: false,
		},
		{
			name: "1270 prefix not loopback",
			cmd:  "curl https://1270.example.com:8180/x",
			port: "8180",
			want: false,
		},
		{
			name: "no port set never matches loopback",
			cmd:  "curl http://127.0.0.1:8180/x",
			port: "",
			want: false,
		},
		{
			name: "different port no match",
			cmd:  "curl http://127.0.0.1:9999/x",
			port: "8180",
			want: false,
		},
		{
			name: "empty cmd",
			cmd:  "",
			port: "8180",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isNaozhiCallbackHook(c.cmd, c.port)
			if got != c.want {
				t.Errorf("isNaozhiCallbackHook(%q, %q) = %v, want %v", c.cmd, c.port, got, c.want)
			}
		})
	}
}
