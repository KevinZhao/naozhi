package cryptoutil

import "testing"

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
}
