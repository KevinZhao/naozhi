package server

import (
	"strings"
	"testing"
)

// TestIsValidBackendID_MatchesSessionRouterCap pins R20260527122801-ARCH-8
// (#1314): the server-side dispatch validator's byte cap MUST be at least
// the session router's validateBackend cap (64) so a backend ID that
// passes the session layer cannot be rejected by the server layer. The
// previous 32-byte server cap rejected legal 33–64 byte IDs that
// router.validateBackend (maxBackendBytes=64) accepted, producing a
// "valid on cron but rejected by dashboard editor" asymmetry.
//
// We exercise the validator via its public boolean return rather than
// touching unexported constants from another package, keeping the
// contract stable through symbol renames. If session/router's
// maxBackendBytes ever shrinks below 64, this test must shrink in
// lock-step (see internal/session/router_backend.go for the source of
// truth).
func TestIsValidBackendID_MatchesSessionRouterCap(t *testing.T) {
	t.Parallel()

	// Probe sits inside the previously-asymmetric window (33–64 bytes).
	// Was: session accepts, server rejects. After the fix: both accept.
	const probe = 60
	id := strings.Repeat("a", probe)
	if !isValidBackendID(id) {
		t.Fatalf("isValidBackendID(%d×'a') = false; want true. The server cap is below the session cap (64) — the SEC-2 (#1314) regression: cron can route a backend that the dashboard editor refuses.", probe)
	}

	// Boundary at 64 must accept; 65 must reject. This guards against
	// "fix bumps cap too far" (e.g. someone setting it to 128).
	if !isValidBackendID(strings.Repeat("a", 64)) {
		t.Fatalf("isValidBackendID(64×'a') = false; want true at the alignment boundary")
	}
	if isValidBackendID(strings.Repeat("a", 65)) {
		t.Fatalf("isValidBackendID(65×'a') = true; cap exceeds session cap of 64 — alignment broke in the other direction")
	}
}
