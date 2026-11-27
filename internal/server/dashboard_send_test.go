package server

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
)

// TestBuildAttachmentETagSeed_MillisecondPrecision pins R20260527122801-SEC-14:
// the (size, mtime) ETag seed must use millisecond-resolution mtime, not
// nanoseconds. UnixNano gave an authenticated attacker an extra 30 bits of
// (size, mtime) entropy per object that can be brute-forced via repeated
// If-None-Match probes. UnixMilli still distinguishes every real attachment
// write (filesystem mtime updates land at human-message cadence, well above
// 1ms granularity) so cache effectiveness is unaffected.
//
// Two times that share their millisecond truncation but differ at the
// nanosecond level MUST produce a byte-identical seed (and therefore
// identical SHA-256). A regression that re-introduced UnixNano would
// produce two distinct seeds and fail this assertion.
func TestBuildAttachmentETagSeed_MillisecondPrecision(t *testing.T) {
	t.Parallel()

	const size int64 = 4096
	base := time.Date(2026, 5, 27, 12, 0, 0, 123_000_000, time.UTC) // …123 ms exact
	jitterNs := base.Add(456 * time.Nanosecond)                     // same ms, different ns

	a := buildAttachmentETagSeed(nil, size, base)
	b := buildAttachmentETagSeed(nil, size, jitterNs)
	if !bytes.Equal(a, b) {
		t.Fatalf("ETag seed differs across sub-millisecond mtime jitter — UnixNano regression?\nbase:   %q\njitter: %q", a, b)
	}

	// Confirm the resulting hash matches too — the seed is the only
	// non-stable input to sha256, so this is belt+braces.
	if sha256.Sum256(a) != sha256.Sum256(b) {
		t.Fatalf("ETag hash differs across sub-millisecond mtime jitter")
	}

	// Sanity guard against over-rounding: a +1ms jitter MUST still flip
	// the seed so legitimate updates still rotate the ETag.
	plusMs := base.Add(time.Millisecond)
	c := buildAttachmentETagSeed(nil, size, plusMs)
	if bytes.Equal(a, c) {
		t.Fatalf("ETag seed unchanged across +1ms mtime — over-rounding regression: %q", a)
	}
}

// TestUploadOwner_NoIPFallbackOnNilWriter pins R247-SEC-8 (#501): when
// uploadOwner cannot mint a fresh nz_anon (no ResponseWriter to set the
// cookie on, or crypto/rand failure on the real path), it MUST return
// ok=false instead of falling back to a clientIP-derived owner key. The
// IP fallback would silently bucket every co-NAT browser under the same
// SHA-256 hex digest, re-opening the TakeAll cross-tenant theft window
// that nz_anon was designed to close.
//
// We exercise the deterministic branch (`w == nil`) since a real
// crypto/rand failure isn't reproducible in CI without injection. The
// guarantee is symmetric: every path that previously fell to clientIP
// now fails closed.
func TestUploadOwner_NoIPFallbackWhenAnonMintImpossible(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "/api/sessions/upload", nil)
	r.RemoteAddr = "203.0.113.5:40000"
	owner, ok := uploadOwner(nil, r, nil, false)
	if ok {
		t.Fatalf("uploadOwner with nil writer must fail closed; got owner=%q ok=true", owner)
	}
	if owner != "" {
		t.Errorf("owner must be empty on failure path; got %q", owner)
	}
}

// TestMintAnonCookie_SecureByMode pins R222-SEC-4 / #687: with a dashboard
// token (multi-user intent) nz_anon is Secure even over plaintext, so a
// same-network sniffer cannot lift the owner label; without one (single-user
// http://127.0.0.1) it follows the request, so the browser still sends it.
func TestMintAnonCookie_SecureByMode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		token string
		want  bool
	}{{"deploy-token", true}, {"", false}} {
		r := httptest.NewRequest("POST", "/api/sessions/upload", nil)
		w := httptest.NewRecorder()
		if _, err := mintAnonCookie(w, r, auth.New(tc.token, nil, "", false)); err != nil {
			t.Fatalf("mintAnonCookie returned error: %v", err)
		}
		var got *http.Cookie
		for _, c := range w.Result().Cookies() {
			if c.Name == anonCookieName {
				got = c
			}
		}
		if got == nil || got.Secure != tc.want {
			t.Errorf("token %q: nz_anon = %+v, want Secure=%v", tc.token, got, tc.want)
		}
	}
}

// TestUploadOwner_AnonCookieFallback locks RNEW-SEC-005: no-token mode mints
// a per-browser nz_anon cookie so co-NAT clients get distinct owners (no
// TakeAll theft) and emits the spec attributes (reuse: anon_cookie_renewal_test).
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
	o, ok := uploadOwner(w1, newReq(), nil, false)
	if !ok || o == "" || o == "203.0.113.5" {
		t.Fatalf("owner = %q ok=%v; anon-cookie path skipped", o, ok)
	}
	got := findAnon(w1)
	if got == nil || !got.HttpOnly || got.SameSite != http.SameSiteStrictMode || len(got.Value) != 32 {
		t.Fatalf("nz_anon Set-Cookie missing/malformed: %+v", got)
	}
	// Co-NAT browsers must get distinct owners.
	a, _ := uploadOwner(httptest.NewRecorder(), newReq(), nil, false)
	b, _ := uploadOwner(httptest.NewRecorder(), newReq(), nil, false)
	if a == b {
		t.Fatalf("co-NAT users got identical owner %q — TakeAll theft still possible", a)
	}
}
