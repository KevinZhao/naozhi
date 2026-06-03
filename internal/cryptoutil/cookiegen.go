// Package cryptoutil holds small, dependency-free crypto helpers shared
// across packages so that a single hardening change propagates everywhere
// instead of drifting between near-identical copies.
package cryptoutil

import (
	"crypto/rand"
	"encoding/hex"
)

// RandomCookieGen returns 16 bytes of CSPRNG entropy hex-encoded, used as the
// per-construction seed for the auth-cookie generation marker mixed into the
// cookie HMAC. R217-SEC-6 / R172-SEC-L4 (#595 / #437): an unpredictable seed
// ensures a captured cookie cannot be replayed against a future process that
// shares the same dashboard token + cookie secret.
//
// crypto/rand unavailability is treated as a hard startup fault (panic), not a
// degraded-mode fallback. A time-derived seed is predictable and can be brute-
// forced in observed-deployment-time environments; it is never acceptable as a
// cookie seed. Consistent with internal/dashboard/project/files.go:58 which
// panics on the same failure. R20260603-010128-SEC-1.
//
// R20260602190132-SEC-9 (#1604): single source of truth. Previously this body
// was duplicated verbatim in internal/server and internal/dashboard/auth, so
// any hardening (e.g. removing the time fallback) risked silently diverging.
func RandomCookieGen() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable for RandomCookieGen: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
