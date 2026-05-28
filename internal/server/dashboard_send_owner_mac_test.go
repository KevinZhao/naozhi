package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// helperMakeAuth returns an AuthHandlers whose cookieMAC() yields a
// deterministic value derived from token + a fixed cookieGen + seq=0.
// Used by the cookie-MAC verification tests to obtain a "valid" cookie
// value while still being able to construct different forged values.
func helperMakeAuth(token, secret, gen string) *AuthHandlers {
	return &AuthHandlers{
		dashboardToken: token,
		cookieSecret:   []byte(secret),
		cookieGen:      gen,
		// cookieGenSeq starts at 0 by default — we don't bump it so the
		// MAC stays stable across the test for repeatable assertions.
	}
}

// computeMAC mirrors AuthHandlers.cookieMAC's wire format so tests can
// pre-compute the legitimate cookie value and a forged sibling without
// reaching into the AuthHandlers internals. Mirrors the format documented
// at dashboard_auth.go cookieMAC().
func computeMAC(t *testing.T, token, secret, gen string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(token))
	mac.Write([]byte{0})
	mac.Write([]byte(gen))
	mac.Write([]byte{0})
	mac.Write([]byte("0")) // seq=0
	return hex.EncodeToString(mac.Sum(nil))
}

// TestUploadOwner_RejectsForgedAuthCookie pins R040034-SEC-2 (#1399):
// a caller presenting a forged or stale `nz_auth` cookie value must NOT
// have an owner derived from that cookie. Previously the cookie branch
// hashed c.Value directly without any HMAC compare — letting an
// authenticated caller bucket-shift across upload-quota namespaces by
// rotating the cookie value while keeping their Bearer header valid.
//
// Asserts:
//   - When the cookie value matches the live cookieMAC, the owner key
//     IS derived from the cookie (back-compat, still the "real" owner).
//   - When the cookie value does NOT match (forgery / stale rotation),
//     the cookie branch falls through and the Bearer fallback wins.
//
// Tests construct AuthHandlers manually (not via NewAuthHandlers) to
// avoid pulling in the full server bring-up; the cookieMAC() method
// only depends on the four exported fields we set.
func TestUploadOwner_RejectsForgedAuthCookie(t *testing.T) {
	t.Parallel()

	const token = "deploy-token-xyz"
	const secret = "cookie-secret-1234567890abcdef"
	const gen = "cookie-gen-stable"

	auth := helperMakeAuth(token, secret, gen)
	validMAC := computeMAC(t, token, secret, gen)
	if got := auth.cookieMAC(); got != validMAC {
		t.Fatalf("test wiring: helperMakeAuth produced cookieMAC %q, want %q — "+
			"computeMAC and AuthHandlers.cookieMAC drifted", got, validMAC)
	}

	// Bearer-only baseline: no auth cookie present, only Bearer. Owner
	// derives from sha256(token) so we can compare against the
	// "forged-cookie + Bearer" path below — they MUST match if the
	// fix is working (forged cookie ignored, Bearer wins).
	t.Run("bearer_only_baseline", func(t *testing.T) {
		t.Parallel()
		r := httptest.NewRequest("POST", "/api/sessions/upload", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		owner, ok := uploadOwner(httptest.NewRecorder(), r, auth, false)
		if !ok || owner == "" {
			t.Fatalf("Bearer-only path failed: ok=%v owner=%q", ok, owner)
		}
		// Recompute Bearer-derived owner so the next subtest can assert
		// equality without re-running uploadOwner here (we need the
		// same string to compare).
		sum := sha256.Sum256([]byte(token))
		want := hex.EncodeToString(sum[:16])
		if owner != want {
			t.Fatalf("Bearer owner = %q, want %q (sha256 truncated)", owner, want)
		}
	})

	t.Run("forged_cookie_ignored_bearer_wins", func(t *testing.T) {
		t.Parallel()
		r := httptest.NewRequest("POST", "/api/sessions/upload", nil)
		r.AddCookie(&http.Cookie{
			Name:  authCookieName,
			Value: "AAAforgedAAAA" + strings.Repeat("0", 40),
		})
		r.Header.Set("Authorization", "Bearer "+token)
		owner, ok := uploadOwner(httptest.NewRecorder(), r, auth, false)
		if !ok || owner == "" {
			t.Fatalf("forged-cookie + Bearer path failed: ok=%v owner=%q", ok, owner)
		}
		// MUST equal the Bearer-derived owner — the forged cookie should
		// have been ignored. Without the HMAC gate, owner would derive
		// from the forged cookie value (R040034-SEC-2 / #1399 hole).
		sum := sha256.Sum256([]byte(token))
		bearerOwner := hex.EncodeToString(sum[:16])
		if owner != bearerOwner {
			t.Fatalf("forged-cookie path produced owner=%q, want Bearer-derived %q — "+
				"R040034-SEC-2 (#1399) regression: forged cookie value was hashed into the bucket key, "+
				"letting a caller bucket-shift uploads by rotating the cookie", owner, bearerOwner)
		}
	})

	t.Run("valid_cookie_still_honoured", func(t *testing.T) {
		t.Parallel()
		r := httptest.NewRequest("POST", "/api/sessions/upload", nil)
		r.AddCookie(&http.Cookie{
			Name:  authCookieName,
			Value: validMAC,
		})
		owner, ok := uploadOwner(httptest.NewRecorder(), r, auth, false)
		if !ok || owner == "" {
			t.Fatalf("valid-cookie path failed: ok=%v owner=%q", ok, owner)
		}
		want := ownerKeyFromCookie(validMAC)
		if owner != want {
			t.Fatalf("valid-cookie owner = %q, want %q — HMAC gate must not break the legitimate path", owner, want)
		}
	})
}

// TestUploadOwner_NoTokenMode_AuthCookieIgnored pins the no-token-mode
// branch: when dashboardToken is empty, cookieMAC() returns "" so the
// auth-cookie branch must fall through even on an exact "value matches
// empty MAC" coincidence. The fix's `mac != ""` guard closes that
// window — without it, a caller setting `nz_auth=` (empty value) might
// look like it matched, though c.Value != "" already filters that out.
// Defensive-test the explicit no-token path with a non-empty cookie.
func TestUploadOwner_NoTokenMode_AuthCookieIgnored(t *testing.T) {
	t.Parallel()

	auth := &AuthHandlers{} // no token → cookieMAC() returns ""

	r := httptest.NewRequest("POST", "/api/sessions/upload", nil)
	r.AddCookie(&http.Cookie{
		Name:  authCookieName,
		Value: "anything-can-go-here",
	})
	w := httptest.NewRecorder()
	owner, ok := uploadOwner(w, r, auth, false)
	if !ok || owner == "" {
		t.Fatalf("no-token-mode path failed: ok=%v owner=%q", ok, owner)
	}
	// Owner must come from the freshly minted nz_anon cookie (the
	// auth-cookie branch should be skipped because cookieMAC() == "").
	// Verify by checking that a Set-Cookie for nz_anon was emitted —
	// uploadOwner mints one on the no-anon-cookie code path.
	var sawAnon bool
	for _, c := range w.Result().Cookies() {
		if c.Name == anonCookieName {
			sawAnon = true
			break
		}
	}
	if !sawAnon {
		t.Fatalf("no-token-mode with stray nz_auth cookie: nz_anon Set-Cookie missing — "+
			"the auth-cookie branch must be skipped (mac==\"\") so uploadOwner falls through to "+
			"the anon-mint branch; instead it appears the auth cookie was honoured. owner=%q", owner)
	}
}
