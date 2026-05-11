package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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
