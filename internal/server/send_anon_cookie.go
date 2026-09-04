// nz_anon cookie helpers (mint + validate + hash). The cookie is NOT an auth
// credential — it is a per-browser random label hashed into the upload-owner
// key so co-NAT users don't collide in no-token mode.
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
)

// anonCookieName labels a per-browser random bucket used ONLY in no-token
// mode to disambiguate uploadOwner between co-NAT users, so User A's upload
// cannot be claimed by User B via TakeAll. Not an auth credential.
const anonCookieName = "nz_anon"

// anonCookieHexLen is the wire length of a minted nz_anon value (16 random
// bytes, hex). Validators re-mint when the inbound cookie does not match, so
// attacker-supplied bytes never land in uploadOwner buckets unaltered (#485).
const anonCookieHexLen = 32

// anonCookieMaxAgeSeconds bounds the nz_anon label lifetime. It MUST equal
// the nz_auth session lifetime (authCookieMaxAgeSeconds in
// internal/dashboard/auth): a label outliving the auth session lets a second
// user on a shared device inherit the first user's owner bucket and TakeAll
// their pending uploads (#2297). Kept a literal; the
// anonCookieMaxAgeMatchesAuth test pins the two values together.
const anonCookieMaxAgeSeconds = 3600 // 1 hour, aligned to nz_auth session

// isValidAnonCookieValue reports whether v looks like a freshly-minted
// nz_anon value: exactly anonCookieHexLen bytes, all lowercase hex.
// Strictly lowercase because mintAnonCookie emits encoding/hex's form; any
// other shape originated outside the server and must be re-minted (#485).
func isValidAnonCookieValue(v string) bool {
	if len(v) != anonCookieHexLen {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// mintAnonCookie writes a freshly-random nz_anon cookie and returns its value.
// Attributes: HttpOnly, SameSite=Strict (only same-origin XHR reads it; Lax
// would open a cross-site-GET window), Secure per setAnonCookie,
// MaxAge=anonCookieMaxAgeSeconds.
func mintAnonCookie(w http.ResponseWriter, r *http.Request, ah *auth.Handlers) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	val := hex.EncodeToString(buf[:])
	setAnonCookie(w, r, ah, val)
	return val, nil
}

// setAnonCookie writes the nz_anon Set-Cookie header with the shared
// attribute set, so mintAnonCookie (fresh value) and renewAnonCookie (same
// value, fresh expiry) cannot drift on attributes.
func setAnonCookie(w http.ResponseWriter, r *http.Request, ah *auth.Handlers, val string) {
	secure := ah != nil && ah.IsSecure(r)
	// Force Secure when a dashboard token is configured (multi-user intent):
	// the browser drops the cookie under plaintext HTTP, which is the desired
	// fail-closed against same-network sniffing (#687). No-token deployments
	// on http://127.0.0.1 keep working.
	if !secure && ah != nil && ah.DashboardToken != "" {
		secure = true
	}
	http.SetCookie(w, &http.Cookie{
		Name: anonCookieName, Value: val, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: secure, MaxAge: anonCookieMaxAgeSeconds,
	})
}

// renewAnonCookie re-issues the SAME nz_anon value with a fresh MaxAge
// (sliding renewal, mirroring nz_auth). Without it a long-lived WS keeps the
// owner derived from an expired label while the next upload mints a new one,
// and every file-bearing send fails TakeAll with "file not found or expired".
func renewAnonCookie(w http.ResponseWriter, r *http.Request, ah *auth.Handlers, val string) {
	setAnonCookie(w, r, ah, val)
}

// renewedOwnerFromCookie is the "valid nz_anon presented" arm of HTTP owner
// derivation: sliding-renew the label (w may be nil) and return the owner
// key it hashes to.
func renewedOwnerFromCookie(w http.ResponseWriter, r *http.Request, ah *auth.Handlers, val string) string {
	if w != nil {
		renewAnonCookie(w, r, ah, val)
	}
	return ownerKeyFromCookie(val)
}

// ownerKeyFromCookie returns a stable owner key derived from an HMAC
// auth-cookie value. Hashing keeps raw MAC material out of the owner key;
// sha256[:16] (128-bit) so a chosen-cookie collision cannot steer the
// per-owner upload bucket onto another tenant's quota. The key is opaque —
// only equality-tested against ownerCounts/ownerBytes.
func ownerKeyFromCookie(cookieValue string) string {
	if cookieValue == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(cookieValue))
	return hex.EncodeToString(sum[:16])
}
