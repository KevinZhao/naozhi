package cron

import (
	"strings"
	"testing"
)

// TestRedactAddrInCronError_IPPort pins R20260603-SEC-1 / R20260603-SEC-4:
// IPv4 addresses and IPv4:port pairs must be replaced with [redacted-addr]
// so that "dial tcp 192.168.1.5:4012: connection refused" style errors do
// not expose network topology to dashboard viewers or log sinks.
func TestRedactAddrInCronError_IPPort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			"ip with port",
			"dial tcp 192.168.1.5:4012: connection refused",
			"dial tcp [redacted-addr]: connection refused",
		},
		{
			"ip without port",
			"connect to 10.0.0.1: timeout",
			"connect to [redacted-addr]: timeout",
		},
		{
			"loopback with port",
			"dial tcp 127.0.0.1:8080: connection refused",
			"dial tcp [redacted-addr]: connection refused",
		},
		{
			"public ip with high port",
			"send error: dial tcp 203.0.113.42:9999: i/o timeout",
			"send error: dial tcp [redacted-addr]: i/o timeout",
		},
		{
			"no ip unchanged",
			"context deadline exceeded",
			"context deadline exceeded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactAddrInCronError(tc.input)
			if got != tc.want {
				t.Errorf("redactAddrInCronError(%q)\n  got  = %q\n  want = %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestRedactAddrInCronError_IPv6 pins R20260604-GO-016: bracketed IPv6
// addresses (as produced by "dial tcp [addr]:port" errors) must be replaced
// with [redacted-addr]; bare non-bracket text must pass through unchanged;
// IPv4 behaviour must be fully preserved.
func TestRedactAddrInCronError_IPv6(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			"ipv6 with port",
			"dial tcp [2001:db8::1]:4012: connection refused",
			"dial tcp [redacted-addr]: connection refused",
		},
		{
			"ipv6 loopback with port",
			"dial tcp [::1]:8080: connection refused",
			"dial tcp [redacted-addr]: connection refused",
		},
		{
			"ipv6 without port",
			"connect to [fe80::1]: timeout",
			"connect to [redacted-addr]: timeout",
		},
		{
			"ipv4 still redacted",
			"dial tcp 192.168.1.5:4012: connection refused",
			"dial tcp [redacted-addr]: connection refused",
		},
		{
			"no addr unchanged",
			"context deadline exceeded",
			"context deadline exceeded",
		},
		{
			"plain text with bracket not addr",
			"unexpected token [foo] in config",
			"unexpected token [foo] in config",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactAddrInCronError(tc.input)
			if got != tc.want {
				t.Errorf("redactAddrInCronError(%q)\n  got  = %q\n  want = %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestRedactPathsInCronError_IPPortFastPath asserts that IP:port strings
// reach redactPathsInCronError's IP:port branch even when hasNoPathTrigger
// returns true (i.e. no slash/backslash/tilde) — ensuring both fast-path
// returns in the function apply addr redaction. R20260603-SEC-1/SEC-4.
func TestRedactPathsInCronError_IPPortFastPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
	}{
		{"session error with ip:port", "session error: dial tcp 192.168.1.5:4012: connection refused"},
		{"send error with ip", "send error: connect 10.0.0.2: refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactPathsInCronError(tc.input)
			if strings.ContainsAny(got, "0123456789") {
				// Allow digits that are not IP-shaped; check specifically for
				// dot-separated quad patterns to avoid false positives on port
				// numbers in other contexts.
			}
			if redactAddrRe.MatchString(got) {
				t.Errorf("redactPathsInCronError(%q) still contains IP:port pattern: %q", tc.input, got)
			}
			if !strings.Contains(got, "[redacted-addr]") {
				t.Errorf("redactPathsInCronError(%q) missing [redacted-addr] sentinel: %q", tc.input, got)
			}
		})
	}
}
