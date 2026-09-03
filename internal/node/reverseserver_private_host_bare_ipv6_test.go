package node

import "testing"

// TestIsPrivateHost_BareIPv6 covers #2339: a bare (unbracketed) IPv6 Host
// header such as "fd00::1" has multiple colons, so the naive "strip after
// last ':'" port trim mangles it to "fd00:" and net.ParseIP fails, wrongly
// classifying a private/link-local peer as non-private. Mirror the
// single-colon guard isLoopbackHost already has.
func TestIsPrivateHost_BareIPv6(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"fd00::1", true},           // bare IPv6 unique-local, no port
		{"fe80::1", true},           // bare IPv6 link-local, no port
		{"fc00:dead:beef::1", true}, // bare IPv6 unique-local, several groups
		{"[fd00::1]:8080", true},    // bracketed + port still works
		{"[fe80::1]", true},         // bracketed, no port
		{"10.0.0.1:80", true},       // single colon → host:port
		{"10.0.0.1", true},
		{"8.8.8.8", false},
		{"2001:db8::1", false}, // bare IPv6 global — not private
		{"::1", false},         // loopback is isLoopbackHost's job
		{"example.com:80", false},
		{"example.com", false},
	}
	for _, tc := range cases {
		if got := isPrivateHost(tc.host); got != tc.want {
			t.Errorf("isPrivateHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
