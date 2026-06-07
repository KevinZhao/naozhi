package cron

import "testing"

// TestRedactAddrInCronError_IPv6ColonRequired pins R20260607-GO-013:
// the IPv6 redaction regex must require at least one colon inside the
// brackets so non-address bracketed tokens like [abc], [dead], or [1]
// are not over-redacted. A real IPv6 literal always contains at least
// one colon (e.g. [::1], [fe80::1], [2001:db8::1]).
func TestRedactAddrInCronError_IPv6ColonRequired(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string // must equal input: these are NOT addresses
	}{
		{
			"hex token no colon not redacted",
			"parse error at [abc]: unexpected token",
			"parse error at [abc]: unexpected token",
		},
		{
			"dead hex no colon not redacted",
			"config key [dead] is unrecognised",
			"config key [dead] is unrecognised",
		},
		{
			"single hex digit not redacted",
			"flag [1] unknown",
			"flag [1] unknown",
		},
		{
			"all-hex word not redacted",
			"token [cafe] in stream",
			"token [cafe] in stream",
		},
		// R20260607-COR-008: degenerate colon-only bracket tokens are NOT
		// valid IPv6 literals and must not be over-redacted.
		{
			"single colon empty brackets not redacted",
			"flag [:] unknown",
			"flag [:] unknown",
		},
		{
			"bare colon-only token not redacted",
			"got [:] in stream",
			"got [:] in stream",
		},
		// Positive: real IPv6 still redacted.
		{
			"ipv6 loopback redacted",
			"dial tcp [::1]:443: connection refused",
			"dial tcp [redacted-addr]: connection refused",
		},
		{
			"ipv6 unspecified compressed redacted",
			"bind [::]:8080: address in use",
			"bind [redacted-addr]: address in use",
		},
		{
			"ipv6 link-local with port redacted",
			"dial [fe80::1]:8080: no route",
			"dial [redacted-addr]: no route",
		},
		{
			"ipv6 full form redacted",
			"connect [2001:db8::1]: timeout",
			"connect [redacted-addr]: timeout",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := redactAddrInCronError(tc.input)
			if got != tc.want {
				t.Errorf("redactAddrInCronError(%q)\n  got  = %q\n  want = %q", tc.input, got, tc.want)
			}
		})
	}
}
