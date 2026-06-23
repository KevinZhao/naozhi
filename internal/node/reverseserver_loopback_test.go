package node

import "testing"

func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		// Canonical literals.
		{"localhost", true},
		{"localhost:8080", true},
		{"127.0.0.1", true},
		{"127.0.0.1:443", true},
		{"::1", true},
		{"[::1]", true},
		{"[::1]:8080", true},
		// Full 127.0.0.0/8 range — R202606f-SEC-002.
		{"127.0.0.2", true},
		{"127.0.0.2:9000", true},
		{"127.255.255.254", true},
		{"127.1.2.3", true},
		// Non-loopback must be rejected.
		{"8.8.8.8", false},
		{"8.8.8.8:80", false},
		{"192.168.1.1", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"example.com", false},
		{"example.com:443", false},
		{"[2001:db8::1]", false},
		{"[2001:db8::1]:8080", false},
	}
	for _, tc := range cases {
		if got := isLoopbackHost(tc.host); got != tc.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
