// Phase 5-prep / R-send-helper-extract (2026-05-28):
// Anonymous-bucket cookie helpers moved out of dashboard_send.go into
// their own file. Pure physical split, zero behaviour change. The
// nz_anon cookie is NOT an auth credential — it's a per-browser random
// label hashed into the upload-owner key so co-NAT users don't collide
// in no-token mode. All three constants and three functions move
// together because they form one cohesive contract (mint + validate +
// hash); splitting them would scatter the security comments that justify
// the lengths and lifetime values.
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
// (opt-in) mode to disambiguate uploadOwner between co-NAT users. NOT an
// auth credential — just a 16-byte random label hashed into the owner key
// so User A's upload cannot be claimed by User B via TakeAll.
const anonCookieName = "nz_anon"

// anonCookieHexLen is the wire length of a freshly-minted nz_anon value:
// 16 random bytes hex-encoded (see mintAnonCookie). Validators on the
// upgrade / send paths re-mint when the inbound cookie does not match
// this length so a malformed-or-attacker-bytes value cannot land in
// uploadOwner buckets unaltered. The HMAC proposal in R236-SEC-06 / #485
// would add a signature but the cookie carries no trust to begin with —
// it's a label. Length + lowercase-hex is sufficient to reject obvious
// injection attempts while keeping the existing ownerKeyFromCookie hash
// chain intact for legitimately-minted values.
const anonCookieHexLen = 32

// anonCookieMaxAgeSeconds bounds the lifetime of the nz_anon owner label.
// R247-SEC-15 / #514: lowered from 30 days to 7 to shrink the window in
// which a stale label can be reused after a service restart, token-mode
// toggle, or non-TLS sniff. Pulled out as a const so regression tests can
// pin the value without parsing the cookie header.
const anonCookieMaxAgeSeconds = 7 * 24 * 3600

// isValidAnonCookieValue reports whether v looks like a freshly-minted
// nz_anon value: exactly anonCookieHexLen bytes, all lowercase hex.
// The hex check is intentionally strict (lowercase only) because
// mintAnonCookie always emits encoding/hex's lowercase form; any other
// shape originated outside the server and should be re-minted, not
// hashed-and-bucketed. R236-SEC-06 (#485) hardening.
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
// HttpOnly, SameSite=Strict (matches the auth cookie; nz_anon is only read by
// same-origin XHR so Lax offered no value and left a cross-site-GET window
// open for any future GET handler that reads it), Secure gated by
// auth.IsSecure(r), MaxAge=anonCookieMaxAgeSeconds.
//
// R247-SEC-15 / #514: MaxAge was 30 days, which kept the per-browser owner
// label alive across token-mode toggles and service restarts that the
// operator may have used to invalidate sessions. The cookie is NOT an auth
// credential — it only disambiguates uploadOwner between co-NAT users —
// but a stale owner label can still be claimed by an attacker who sniffed
// the value over a non-TLS deployment (where the Secure flag is absent
// because auth.IsSecure(r)=false). 7 days is the upper bound a reasonable
// dev-laptop user would expect for a "remember this tab" hint, and it
// shrinks the post-token-rotation window 4×. The cookieGen-coupled
// rotation that #514's proposal flags as the deeper fix is left for a
// follow-up because it requires a dashboard_auth coupling change; this
// commit just lowers the MaxAge floor where there is no design decision.
//
// R222-SEC-4 / #687: in multi-user mode (dashboardToken set) with no TLS
// terminator on the request path, the previous Secure=false branch let a
// same-network attacker sniff the cookie and bucket-collide future uploads.
// We now force Secure=true whenever a dashboard token is configured, which
// makes the browser refuse to ship the cookie over plaintext — fail-closed
// rather than silently degrade. The single-user / no-token deployment is
// unchanged: those operators legitimately run on http://127.0.0.1 and the
// Secure-on-non-TLS combination would simply make the browser drop the
// cookie, breaking upload disambiguation for nobody (single user, single
// owner). The startup-time warning on server.go:931 already calls out the
// "token + plaintext" misconfiguration so an operator running this combo
// will see the cookies disappear and find the warning in the same log.
func mintAnonCookie(w http.ResponseWriter, r *http.Request, ah *auth.Handlers) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	val := hex.EncodeToString(buf[:])
	secure := ah != nil && ah.IsSecure(r)
	// R222-SEC-4 / #687: force Secure when a dashboard token is configured —
	// the operator has signalled multi-user intent, so plaintext sniff of the
	// owner label is no longer an acceptable degradation. The browser will
	// drop the cookie under HTTP, which is the desired fail-closed.
	if !secure && ah != nil && ah.DashboardToken != "" {
		secure = true
	}
	http.SetCookie(w, &http.Cookie{
		Name: anonCookieName, Value: val, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: secure, MaxAge: anonCookieMaxAgeSeconds,
	})
	return val, nil
}

// ownerKeyFromCookie returns a stable owner key derived from an HMAC
// auth-cookie value. The cookie is itself an HMAC hex string so hashing it
// ensures the owner key does not leak raw MAC material (the old code used a
// raw 16-char cookie prefix which exposed half of the MAC).
//
// R247-SEC-16: sha256[:8] gave 64-bit owner-key entropy — collision-find /
// preimage feasible at scale (~2^32 keys for 50% collision). Bump to
// sha256[:16] (128-bit) so the per-owner upload bucket cannot be steered
// onto another tenant's quota by a chosen-cookie collision attack. The
// owner key is opaque: only equality-tested against ownerCounts/ownerBytes
// map keys. Existing in-memory entries from prior process incarnations are
// invalidated on restart anyway (uploadStore is RAM-only), so widening
// the key has no migration cost. Mirrors R246-SEC-5 / R247-SEC-24
// (resume key) and R67-SEC-1 (WS bearer hash) which all carry ≥128-bit
// material elsewhere in the codebase.
func ownerKeyFromCookie(cookieValue string) string {
	if cookieValue == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(cookieValue))
	return hex.EncodeToString(sum[:16])
}
