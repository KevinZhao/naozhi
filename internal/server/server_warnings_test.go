package server

import "testing"

func TestStartupWarnings(t *testing.T) {
	t.Parallel()
	noToken := []struct {
		addr         string
		trustedProxy bool
		want         bool
	}{
		{"127.0.0.1:8180", false, false},
		{"0.0.0.0:8180", false, true},
		{"0.0.0.0:8180", true, false},
	}
	for _, tc := range noToken {
		if got := shouldWarnNoTokenOpen("", tc.addr, tc.trustedProxy); got != tc.want {
			t.Errorf("shouldWarnNoTokenOpen(\"\", %q, %v) = %v, want %v", tc.addr, tc.trustedProxy, got, tc.want)
		}
	}
	reverse := []struct {
		enabled bool
		addr    string
		want    bool
	}{
		{false, "0.0.0.0:8180", false},
		{true, "127.0.0.1:8180", false},
		{true, "0.0.0.0:8180", true},
	}
	for _, tc := range reverse {
		if got := shouldWarnReverseNodePlaintext(tc.enabled, false, tc.addr); got != tc.want {
			t.Errorf("shouldWarnReverseNodePlaintext(%v, false, %q) = %v, want %v", tc.enabled, tc.addr, got, tc.want)
		}
	}
}
