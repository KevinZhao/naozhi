package server

import "testing"

func TestStartupWarnings(t *testing.T) {
	t.Parallel()
	noToken := []struct {
		token        string
		addr         string
		trustedProxy bool
		want         bool
	}{
		{"", "127.0.0.1:8180", false, false},
		{"", "0.0.0.0:8180", false, true},
		{"", ":8180", false, true},
		{"", "not_a_valid_addr", false, true},
		{"", "0.0.0.0:8180", true, false},
		{"s3cret", "0.0.0.0:8180", false, false},
	}
	for _, tc := range noToken {
		if got := shouldWarnNoTokenOpen(tc.token, tc.addr, tc.trustedProxy); got != tc.want {
			t.Errorf("shouldWarnNoTokenOpen(%q, %q, %v) = %v, want %v", tc.token, tc.addr, tc.trustedProxy, got, tc.want)
		}
	}
	reverse := []struct {
		enabled      bool
		trustedProxy bool
		addr         string
		want         bool
	}{
		{false, false, "0.0.0.0:8180", false},
		{true, false, "127.0.0.1:8180", false},
		{true, false, "0.0.0.0:8180", true},
		{true, true, "0.0.0.0:8180", false},
	}
	for _, tc := range reverse {
		if got := shouldWarnReverseNodePlaintext(tc.enabled, tc.trustedProxy, tc.addr); got != tc.want {
			t.Errorf("shouldWarnReverseNodePlaintext(%v, %v, %q) = %v, want %v", tc.enabled, tc.trustedProxy, tc.addr, got, tc.want)
		}
	}
}
