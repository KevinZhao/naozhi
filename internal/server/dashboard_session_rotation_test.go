package server

import (
	"testing"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
)

// TestServer_RotateDashboardSessions_InvalidatesCookies pins R217-SEC-6
// (#595): RotateDashboardSessions must change the auth cookie MAC so every
// outstanding cookie fails the next constant-time compare — i.e. real-time
// server-side revocation without a process restart.
func TestServer_RotateDashboardSessions_InvalidatesCookies(t *testing.T) {
	t.Parallel()

	a := auth.New("secret-token", []byte("0123456789abcdef0123456789abcdef"), "seed-gen", false)
	s := &Server{auth: a}

	before := a.CookieMAC()
	if before == "" {
		t.Fatal("CookieMAC empty for a configured token — fixture wrong")
	}

	s.RotateDashboardSessions()

	after := a.CookieMAC()
	if after == before {
		t.Fatalf("CookieMAC unchanged after RotateDashboardSessions (%q); "+
			"R217-SEC-6 regression: token rotation has no explicit session invalidation, "+
			"outstanding cookies still authenticate until the 24h MaxAge or a restart", after)
	}
}

// TestServer_RotateDashboardSessions_NilAuth ensures the method is a no-op
// (no panic) when the auth handler was never wired — defensive for partially
// constructed test fixtures.
func TestServer_RotateDashboardSessions_NilAuth(t *testing.T) {
	t.Parallel()

	s := &Server{}
	s.RotateDashboardSessions() // must not panic
}

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
