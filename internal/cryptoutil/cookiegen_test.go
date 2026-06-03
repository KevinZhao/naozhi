package cryptoutil

import (
	"strings"
	"testing"
)

// TestRandomCookieGen_Unpredictable pins R172-SEC-L4 (#437) / R20260602190132-SEC-9
// (#1604): the per-process cookie generation seed must come from a CSPRNG, not a
// predictable time.Now().UnixNano(). Two successive seeds must differ and must
// carry 32 hex chars (16 bytes) of entropy so a captured cookie cannot be replayed
// against a future instance whose start time an attacker can guess.
func TestRandomCookieGen_Unpredictable(t *testing.T) {
	t.Parallel()

	a := RandomCookieGen()
	b := RandomCookieGen()
	if a == b {
		t.Fatalf("RandomCookieGen returned identical seeds %q twice; "+
			"R172-SEC-L4 regression: predictable cookie generation seed", a)
	}
	if len(a) != 32 {
		t.Fatalf("RandomCookieGen len = %d, want 32 hex chars (16 bytes CSPRNG); got %q", len(a), a)
	}
	// Ensure the output is lowercase hex (not a time-derived decimal fallback).
	// R20260603-010128-SEC-1: the time fallback has been removed; a decimal
	// string (digits-only, no a-f) would indicate a regression to the old
	// predictable path.
	for _, c := range a {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("RandomCookieGen output %q contains non-hex char %q; "+
				"expected lowercase hex from CSPRNG", a, string(c))
		}
	}
	if isDecimalOnly(a) {
		t.Fatalf("RandomCookieGen output %q looks like a decimal time seed; "+
			"R20260603-010128-SEC-1: time fallback must not be present", a)
	}
}

// isDecimalOnly returns true if every character is 0-9 (a decimal-only string
// would indicate a time.Now().UnixNano() fallback rather than a hex-encoded
// CSPRNG output).
func isDecimalOnly(s string) bool {
	return !strings.ContainsAny(s, "abcdef")
}
