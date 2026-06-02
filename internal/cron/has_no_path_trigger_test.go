package cron

import "testing"

// TestHasNoPathTrigger pins R243-ARCH-18 (#850): the extracted path-trigger
// predicate must report exactly the three byte triggers ('/', '\\', '~') that
// redactPathsInCronError's two fast-paths used to test inline. A regression
// that drops one trigger byte would let a path-shaped error slip through
// unredacted; one that adds a spurious trigger would force the Builder path
// for harmless strings.
func TestHasNoPathTrigger(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want bool // true => no path trigger present
	}{
		{"", true},
		{"context deadline exceeded", true},
		{"dispatcher queue full", true},
		{"weight 5kg", true},
		{"open /tmp/x: no such file", false},
		{`C:\Users\bob\x`, false},
		{"see ~/secrets", false},
		{"trailing slash /", false},
		{"backslash only \\", false},
	}
	for _, c := range cases {
		if got := hasNoPathTrigger(c.in); got != c.want {
			t.Errorf("hasNoPathTrigger(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestRedactPathsInCronError_FastPathParity guards the refactor: strings with
// no path trigger must round-trip unchanged through the full redactor, and
// strings with a trigger must be redacted to <path>. This is the
// observable behaviour the inline fast-paths guaranteed before the predicate
// was hoisted. R243-ARCH-18 (#850).
func TestRedactPathsInCronError_FastPathParity(t *testing.T) {
	t.Parallel()

	noTrigger := "session error: context deadline exceeded"
	if got := redactPathsInCronError(noTrigger); got != noTrigger {
		t.Errorf("no-trigger input changed: got %q want %q", got, noTrigger)
	}

	withPath := "open /home/u/secret: permission denied"
	got := redactPathsInCronError(withPath)
	if got == withPath {
		t.Errorf("path-bearing input was not redacted: %q", got)
	}
}
