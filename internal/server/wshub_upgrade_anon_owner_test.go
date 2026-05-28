package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
)

// TestWSDeriveUploadOwner_NoTokenMintsAnonCookie pins R20260527122801-SEC-2
// (#1326): WS upgrade in no-token mode without an existing nz_anon cookie
// must mint one inline (so the Set-Cookie header rides the 101 response)
// rather than falling back to a client-IP-derived uploadOwner. The
// IP-fallback would let two co-NAT browsers share the same SHA-256-hashed
// uploadOwner bucket, re-opening the cross-tenant TakeAll theft window
// nz_anon closes on the HTTP path.
func TestWSDeriveUploadOwner_NoTokenMintsAnonCookie(t *testing.T) {
	t.Parallel()
	h := &Hub{auth: &auth.Handlers{}} // dashToken == "" → no-token mode
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.RemoteAddr = "10.0.0.1:1234" // co-NAT IP we explicitly do NOT want returned
	w := httptest.NewRecorder()

	owner, authed, ok := wsDeriveUploadOwner(w, r, h, "10.0.0.1")
	if !ok {
		t.Fatalf("wsDeriveUploadOwner ok=false; want true (mint must succeed)")
	}
	if !authed {
		t.Fatalf("authenticated=false in no-token mode; want true")
	}
	if owner == "" {
		t.Fatalf("owner empty; expected hashed nz_anon value")
	}
	if owner == "10.0.0.1" {
		t.Fatalf("owner=%q is the raw client IP — this is the SEC-2 (#1326) regression: co-NAT clients would share an uploadOwner bucket", owner)
	}
	// Confirm Set-Cookie was written so the browser can replay nz_anon
	// on the next request and recover the same owner key.
	cookies := w.Result().Cookies()
	var found bool
	for _, c := range cookies {
		if c.Name == anonCookieName {
			if c.Value == "" {
				t.Fatalf("nz_anon cookie value empty")
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Set-Cookie nz_anon missing from response (must ride the 101 upgrade)")
	}
}

// TestWSDeriveUploadOwner_ExistingAnonCookieReused pins the cookie-replay
// path: when the browser presents an existing nz_anon cookie we MUST
// derive the owner from it (preserving the bucket across reconnects)
// instead of minting a fresh cookie + bucket on every WS upgrade.
func TestWSDeriveUploadOwner_ExistingAnonCookieReused(t *testing.T) {
	t.Parallel()
	h := &Hub{auth: &auth.Handlers{}}
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	r.AddCookie(&http.Cookie{Name: anonCookieName, Value: "deadbeefcafebabe0011223344556677"})
	w := httptest.NewRecorder()

	owner, authed, ok := wsDeriveUploadOwner(w, r, h, "10.0.0.1")
	if !ok || !authed {
		t.Fatalf("ok=%v authed=%v; both must be true for cookie-presented path", ok, authed)
	}
	want := ownerKeyFromCookie("deadbeefcafebabe0011223344556677")
	if owner != want {
		t.Fatalf("owner=%q; want %q (must equal ownerKeyFromCookie of the presented nz_anon)", owner, want)
	}
	// Must NOT mint a second cookie — Set-Cookie should be absent.
	for _, c := range w.Result().Cookies() {
		if c.Name == anonCookieName {
			t.Fatalf("unexpected Set-Cookie on reuse path: %#v", c)
		}
	}
}

// TestWSDeriveUploadOwner_NilAuthFallsBackToIPForTests preserves the
// test-harness escape valve: NewHub callers that do not wire HubOptions.
// Auth (older unit fixtures) keep the legacy IP-fallback so they can
// authenticate without setting up a full auth.Handlers. Production wiring
// always passes Auth, so the SEC-2 fix takes effect there.
func TestWSDeriveUploadOwner_NilAuthFallsBackToIPForTests(t *testing.T) {
	t.Parallel()
	h := &Hub{} // dashToken == "" AND auth == nil
	r := httptest.NewRequest(http.MethodGet, "/ws", nil)
	w := httptest.NewRecorder()

	owner, authed, ok := wsDeriveUploadOwner(w, r, h, "10.0.0.1")
	if !ok || !authed {
		t.Fatalf("ok=%v authed=%v; nil-auth test path must still authenticate", ok, authed)
	}
	if owner != "10.0.0.1" {
		t.Fatalf("owner=%q; want raw IP fallback for legacy test harness", owner)
	}
}
