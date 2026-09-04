package auth

import (
	"crypto/rand"
	"encoding/hex"
)

// RandomCookieGen returns 16 bytes of CSPRNG entropy hex-encoded, the
// per-process seed mixed into the auth-cookie HMAC so a captured cookie cannot
// be replayed against a future process sharing the same token + secret (#595).
// crypto/rand unavailability panics: a time-derived seed is predictable and
// never acceptable as a cookie seed (#1604).
func RandomCookieGen() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable for RandomCookieGen: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
