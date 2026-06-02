// Package cryptoutil holds small, dependency-free crypto helpers shared
// across packages so that a single hardening change propagates everywhere
// instead of drifting between near-identical copies.
package cryptoutil

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"
)

// RandomCookieGen returns 16 bytes of CSPRNG entropy hex-encoded, used as the
// per-construction seed for the auth-cookie generation marker mixed into the
// cookie HMAC. R217-SEC-6 / R172-SEC-L4 (#595 / #437): an unpredictable seed
// ensures a captured cookie cannot be replayed against a future process that
// shares the same dashboard token + cookie secret. On the (practically
// impossible) rand.Read failure we fall back to a time-derived value so the
// process still starts — strictly no worse than the previous always-time seed.
//
// R20260602190132-SEC-9 (#1604): single source of truth. Previously this body
// was duplicated verbatim in internal/server and internal/dashboard/auth, so
// any hardening (e.g. removing the time fallback) risked silently diverging.
func RandomCookieGen() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b[:])
}
