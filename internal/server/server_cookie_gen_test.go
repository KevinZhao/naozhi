package server

import "testing"

// TestRandomCookieGen_Unpredictable pins R172-SEC-L4 (#437): the per-process
// cookie generation seed must come from a CSPRNG, not a predictable
// time.Now().UnixNano(). Two successive seeds must differ and must carry
// 32 hex chars (16 bytes) of entropy so a captured cookie cannot be replayed
// against a future instance whose start time an attacker can guess.
func TestRandomCookieGen_Unpredictable(t *testing.T) {
	t.Parallel()

	a := randomCookieGen()
	b := randomCookieGen()
	if a == b {
		t.Fatalf("randomCookieGen returned identical seeds %q twice; "+
			"R172-SEC-L4 regression: predictable cookie generation seed", a)
	}
	if len(a) != 32 {
		t.Fatalf("randomCookieGen len = %d, want 32 hex chars (16 bytes CSPRNG); got %q", len(a), a)
	}
}
