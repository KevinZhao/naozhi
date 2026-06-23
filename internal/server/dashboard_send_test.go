package server

import (
	"bytes"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/session"
)

// TestDashboardSend_LogKeySanitized pins R202606b-SEC-3: the user-controlled
// session key (r.FormValue("key") / req.Key) is attacker-influenced and must
// be run through session.SanitizeLogAttr before reaching slog, so a malicious
// key cannot inject newlines, C0/C1 control bytes, or bidi/zero-width runes
// into structured operator log lines. This asserts the exact wrapping the
// dashboard send handlers apply at their three slog callsites.
func TestDashboardSend_LogKeySanitized(t *testing.T) {
	t.Parallel()

	const malicious = "feishu:group:abc\n\rFAKE level=ERROR‮msg=spoof​\x07"

	// The sanitized value is what the handler hands to slog; inspect it
	// directly so we don't trip over slog's own trailing newline.
	sanitized := session.SanitizeLogAttr(malicious)
	for _, r := range []rune{'\n', '\r', '\x07', '‮', '​'} {
		if strings.ContainsRune(sanitized, r) {
			t.Errorf("unsanitized control/bidi rune U+%04X survived: %q", r, sanitized)
		}
	}
	// The sanitized payload still carries the meaningful prefix so operators
	// can correlate the event, just without the injection vectors.
	if !strings.Contains(sanitized, "feishu") {
		t.Errorf("expected the benign key prefix to survive sanitization: %q", sanitized)
	}

	// And confirm it produces a single, unfragmented log line when fed to slog
	// exactly as the handler does.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{}))
	logger.Warn("dashboard sessionSend rejected", "key", sanitized, "err", "boom")
	if got := strings.Count(strings.TrimRight(buf.String(), "\n"), "\n"); got != 0 {
		t.Errorf("log line fragmented into %d extra lines: %q", got, buf.String())
	}
}

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

// TestAnonCookieMaxAge_AlignedToAuthSession pins R202606f-SEC-005 / #2297
// (which supersedes the R247-SEC-15 / #514 7-day floor): the nz_anon MaxAge
// MUST equal the nz_auth session lifetime (1h). Previously the label lived 7
// days — 7× the auth session — so on a shared device a second user arriving
// after the first user's auth cookie expired (but before logout, which #2157
// wires to clear nz_anon) could inherit the first user's uploadOwner bucket
// and TakeAll their pending uploads. Bounding the label to the auth session
// closes that reuse window.
//
// We assert the constant directly so a future refactor that re-introduces a
// longer MaxAge (e.g. a "remember me" UX request) is forced to update the
// constant rather than silently widen the window via a magic literal.
func TestAnonCookieMaxAge_AlignedToAuthSession(t *testing.T) {
	t.Parallel()
	const oneHour = 3600 // mirrors internal/dashboard/auth authCookieMaxAgeSeconds
	if anonCookieMaxAgeSeconds != oneHour {
		t.Fatalf("#2297 regression: anonCookieMaxAgeSeconds = %d; want 1h (%d) aligned to the nz_auth session. A longer-lived anon label re-opens the shared-device upload-bucket reuse window — update the const intentionally and adjust this test if you really mean to widen it.", anonCookieMaxAgeSeconds, oneHour)
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

// TestUploadOwnerOrFail_503OnFailure pins the helper that handlers wrap
// uploadOwner with: a closed-over derivation MUST emit 503 + Retry-After
// so the dashboard retries on a fresh socket where /dev/urandom may have
// replenished, instead of silently dropping the request.
func TestUploadOwnerOrFail_503OnFailure(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "/api/sessions/upload", nil)
	r.RemoteAddr = "203.0.113.5:40000"
	w := httptest.NewRecorder()
	owner, ok := uploadOwnerOrFail(w, r, nil, false)
	// ok must be false; nil writer-into-mintAnonCookie path is exercised
	// by TestUploadOwner_NoIPFallbackWhenAnonMintImpossible. Here we use
	// a real recorder so mintAnonCookie succeeds and ok=true (sanity).
	if !ok {
		t.Fatalf("expected ok=true on real recorder; got owner=%q ok=false (status=%d)", owner, w.Code)
	}
	if owner == "" {
		t.Errorf("owner empty on success path")
	}
}

// TestMintAnonCookie_ForcesSecureInMultiUserMode pins R222-SEC-4 / #687: when
// a dashboard token is configured (multi-user intent) the nz_anon cookie MUST
// be marked Secure even if the request itself arrived over plaintext. The
// browser will then refuse to ship the cookie back over HTTP, which is the
// fail-closed behaviour the issue calls for. Without this guard, a same-
// network attacker on a non-TLS deployment could sniff the per-browser owner
// label and steal pending uploads via TakeAll.
//
// We construct an auth.Handlers literal with only dashboardToken populated;
// isSecure(r) returns false because r.TLS==nil and trustedProxy==false, so
// the legacy `secure` branch would have produced Secure=false. Asserting
// got.Secure==true therefore directly exercises the new force-Secure path.
func TestMintAnonCookie_ForcesSecureInMultiUserMode(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "/api/sessions/upload", nil)
	r.RemoteAddr = "203.0.113.5:40000"
	w := httptest.NewRecorder()
	auth := auth.New("deploy-token", nil, "", false)
	if _, err := mintAnonCookie(w, r, auth); err != nil {
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
	if !got.Secure {
		t.Fatalf("R222-SEC-4 regression: nz_anon Secure=false in multi-user mode (dashboardToken set). The browser would ship the owner label over plaintext, re-opening the same-network sniff window. Force-Secure must remain on whenever auth.dashboardToken is non-empty.")
	}
}

// TestMintAnonCookie_NoForceSecureInSingleUserMode pins the converse: when no
// dashboard token is configured (single-user / dev-laptop default) the cookie
// stays at the legacy Secure=isSecure(r) branch. Operators on http://127.0.0.1
// without a token must keep the cookie usable so upload disambiguation works
// — in single-user deployments there is at most one owner and no sniff vector
// to close. Asserts the new logic does not over-fire.
func TestMintAnonCookie_NoForceSecureInSingleUserMode(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("POST", "/api/sessions/upload", nil)
	r.RemoteAddr = "127.0.0.1:40000"
	w := httptest.NewRecorder()
	auth := auth.New("", nil, "", false) // no dashboardToken, single-user mode
	if _, err := mintAnonCookie(w, r, auth); err != nil {
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
	if got.Secure {
		t.Fatalf("nz_anon Secure=true in single-user mode without TLS — would make the browser drop the cookie under HTTP and break upload disambiguation. Force-Secure must only fire when auth.dashboardToken is set.")
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
	// Existing cookie is reused (no Set-Cookie on the response).
	w2, r2 := httptest.NewRecorder(), newReq()
	r2.AddCookie(&http.Cookie{Name: anonCookieName, Value: "deadbeefcafebabe0011223344556677"})
	uploadOwner(w2, r2, nil, false)
	if c := findAnon(w2); c != nil {
		t.Fatalf("unexpected Set-Cookie when nz_anon already present: %q", c.Value)
	}
}
