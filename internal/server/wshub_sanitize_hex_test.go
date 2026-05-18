package server

import (
	"strings"
	"testing"
)

// TestSanitizeHexIDForBroadcast locks the R222-PERF-15 contract: the broadcast
// helper must short-circuit on cron.IsValidID-shaped strings (no allocation /
// no Map walk) and still scrub anything outside the lowercase-hex shape via
// the regular sanitiser. Both branches must stay safe for the dashboard
// payload — a regression here would either expose log-injection runes or
// silently zero out legitimate hex IDs.
func TestSanitizeHexIDForBroadcast(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		id   string
		max  int
		want string
	}{
		{"empty", "", 64, ""},
		{"valid lower hex", "deadbeefcafef00d", 64, "deadbeefcafef00d"},
		{"valid 16-hex over short cap routes via sanitiser",
			"deadbeefcafef00d", 8, // exceeds max → fallback truncates
			"deadbeef"},
		{"uppercase falls through to sanitiser",
			"DEADBEEFCAFEF00D", 64, "DEADBEEFCAFEF00D"},
		{"contains slash falls through to sanitiser (slash is ASCII-printable so kept)",
			"abc/def", 64, "abc/def"},
		{"newline injection routes via sanitiser",
			"abc\ndef", 64, "abc_def"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeHexIDForBroadcast(c.id, c.max)
			if got != c.want {
				t.Errorf("sanitizeHexIDForBroadcast(%q, %d) = %q; want %q",
					c.id, c.max, got, c.want)
			}
		})
	}
}

// TestSanitizeHexIDForBroadcast_FastPathNoStringMap is a regression guard:
// the fast-path must not allocate a new string. We compare the underlying
// data pointer indirectly via length-equal + identical content + the input
// being already canonical, then rely on inlining/escape analysis to keep
// the fast path branch-only. Functionally we only assert "valid hex returns
// the original".
func TestSanitizeHexIDForBroadcast_FastPathReturnsInput(t *testing.T) {
	t.Parallel()
	id := strings.Repeat("ab", 8) // 16 lowercase hex
	got := sanitizeHexIDForBroadcast(id, 64)
	if got != id {
		t.Fatalf("fast path mutated input: got %q want %q", got, id)
	}
}
