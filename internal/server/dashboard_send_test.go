package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAnonCookieMaxAge_BoundedAt7Days pins R247-SEC-15 / #514: the nz_anon
// MaxAge MUST be bounded at the 7-day floor encoded by
// anonCookieMaxAgeSeconds. The previous 30-day MaxAge let a stale per-
// browser owner label survive across token-mode toggles and service
// restarts that the operator may have used as an implicit invalidation
// step. The label is not an auth credential, but a sniffed value over a
// non-TLS deployment can still be replayed to claim a peer user's
// uploadOwner bucket; shrinking the window 4× is the contained fix here
// (the cookieGen-coupled rotation in #514 is a deeper follow-up).
//
// We assert the constant directly so a future refactor that re-introduces
// a longer MaxAge (e.g. a "remember me" UX request) is forced to update
// the constant rather than silently widen the window via a magic literal.
func TestAnonCookieMaxAge_BoundedAt7Days(t *testing.T) {
	t.Parallel()
	const sevenDays = 7 * 24 * 3600
	if anonCookieMaxAgeSeconds != sevenDays {
		t.Fatalf("R247-SEC-15 regression: anonCookieMaxAgeSeconds = %d; want 7 days (%d). Bumping the cap re-opens the 30d sniff-replay window — update the const intentionally and adjust this test if you really mean to widen the floor.", anonCookieMaxAgeSeconds, sevenDays)
	}

	// Mint a cookie via the real path and confirm Max-Age on the wire
	// matches the constant. This catches a regression where a future edit
	// keeps the constant but stops feeding it into http.SetCookie.
	r := httptest.NewRequest("POST", "/api/sessions/upload", nil)
	r.RemoteAddr = "203.0.113.5:40000"
	w := httptest.NewRecorder()
	if _, err := mintAnonCookie(w, r, nil); err != nil {
		t.Fatalf("mintAnonCookie returned error: %v", err)
	}
	var got *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == anonCookieName {
			got = c
			break
		}
	}
	if got == nil {
		t.Fatalf("nz_anon cookie not set by mintAnonCookie")
	}
	if got.MaxAge != anonCookieMaxAgeSeconds {
		t.Fatalf("Set-Cookie Max-Age = %d, want %d (anonCookieMaxAgeSeconds)", got.MaxAge, anonCookieMaxAgeSeconds)
	}
}

// TestUploadOwner_AnonCookieFallback locks RNEW-SEC-005: no-token mode mints
// a per-browser nz_anon cookie so co-NAT clients get distinct owners (no
// TakeAll theft), reuses an existing cookie, and emits the spec attributes.
func TestUploadOwner_AnonCookieFallback(t *testing.T) {
	t.Parallel()
	newReq := func() *http.Request {
		r := httptest.NewRequest("POST", "/api/sessions/upload", nil)
		r.RemoteAddr = "203.0.113.5:40000"
		return r
	}
	findAnon := func(w *httptest.ResponseRecorder) *http.Cookie {
		for _, c := range w.Result().Cookies() {
			if c.Name == anonCookieName {
				return c
			}
		}
		return nil
	}

	// Fresh browser: owner must not be the raw IP and a compliant cookie is set.
	w1 := httptest.NewRecorder()
	if o := uploadOwner(w1, newReq(), nil, false); o == "" || o == "203.0.113.5" {
		t.Fatalf("owner = %q; anon-cookie path skipped", o)
	}
	got := findAnon(w1)
	if got == nil || !got.HttpOnly || got.SameSite != http.SameSiteStrictMode || len(got.Value) != 32 {
		t.Fatalf("nz_anon Set-Cookie missing/malformed: %+v", got)
	}
	// Co-NAT browsers must get distinct owners.
	if a, b := uploadOwner(httptest.NewRecorder(), newReq(), nil, false),
		uploadOwner(httptest.NewRecorder(), newReq(), nil, false); a == b {
		t.Fatalf("co-NAT users got identical owner %q — TakeAll theft still possible", a)
	}
	// Existing cookie is reused (no Set-Cookie on the response).
	w2, r2 := httptest.NewRecorder(), newReq()
	r2.AddCookie(&http.Cookie{Name: anonCookieName, Value: "deadbeefcafebabe0011223344556677"})
	uploadOwner(w2, r2, nil, false)
	if c := findAnon(w2); c != nil {
		t.Fatalf("unexpected Set-Cookie when nz_anon already present: %q", c.Value)
	}
}
