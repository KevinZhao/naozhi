package cron

import "testing"

// TestRedactAddrInCronError pins the address half of cron error redaction:
// IPv4(:port) (R20260603-SEC-1 / SEC-4) and bracketed IPv6(:port)
// (R20260604-GO-016) become [redacted-addr], so "dial tcp 192.168.1.5:4012:
// connection refused" style errors do not expose network topology to
// dashboard viewers or log sinks. Bracketed tokens that are not IPv6 literals
// pass through: a real literal has a colon (R20260607-GO-013), and colon-only
// or single-colon tokens are not literals either (R20260607-COR-008,
// R20260607-CORR-004).
func TestRedactAddrInCronError(t *testing.T) {
	t.Parallel()
	const r = "[redacted-addr]"
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"ip with port", "dial tcp 192.168.1.5:4012: connection refused", "dial tcp " + r + ": connection refused"},
		{"ip without port", "connect to 10.0.0.1: timeout", "connect to " + r + ": timeout"},
		{"loopback with port", "dial tcp 127.0.0.1:8080: connection refused", "dial tcp " + r + ": connection refused"},
		{"public ip with high port", "send error: dial tcp 203.0.113.42:9999: i/o timeout", "send error: dial tcp " + r + ": i/o timeout"},
		{"no addr unchanged", "context deadline exceeded", "context deadline exceeded"},
		{"ipv6 with port", "dial tcp [2001:db8::1]:4012: connection refused", "dial tcp " + r + ": connection refused"},
		{"ipv6 loopback with port", "dial tcp [::1]:8080: connection refused", "dial tcp " + r + ": connection refused"},
		{"ipv6 loopback with other port", "dial tcp [::1]:443: connection refused", "dial tcp " + r + ": connection refused"},
		{"ipv6 without port", "connect to [fe80::1]: timeout", "connect to " + r + ": timeout"},
		{"ipv6 unspecified compressed", "bind [::]:8080: address in use", "bind " + r + ": address in use"},
		{"ipv6 link-local with port", "dial [fe80::1]:8080: no route", "dial " + r + ": no route"},
		{"ipv6 full form", "connect [2001:db8::1]: timeout", "connect " + r + ": timeout"},
		{"plain text with bracket", "unexpected token [foo] in config", "unexpected token [foo] in config"},
		{"hex token no colon", "parse error at [abc]: unexpected token", "parse error at [abc]: unexpected token"},
		{"dead hex no colon", "config key [dead] is unrecognised", "config key [dead] is unrecognised"},
		{"single hex digit", "flag [1] unknown", "flag [1] unknown"},
		{"all-hex word", "token [cafe] in stream", "token [cafe] in stream"},
		{"single colon empty brackets", "flag [:] unknown", "flag [:] unknown"},
		{"bare colon-only token", "got [:] in stream", "got [:] in stream"},
		{"hex plus single trailing colon", "flag [a:] unknown", "flag [a:] unknown"},
		{"single colon hex:hex token", "see [a:b] in output", "see [a:b] in output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := redactAddrInCronError(tc.input); got != tc.want {
				t.Errorf("redactAddrInCronError(%q)\n  got  = %q\n  want = %q", tc.input, got, tc.want)
			}
		})
	}
}
