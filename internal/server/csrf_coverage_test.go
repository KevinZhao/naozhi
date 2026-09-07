package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/session"
)

// TestAPICrossOriginRejected drives EVERY mutating /api/ route through the real
// mux with a valid session cookie and a foreign Origin, and asserts 403.
//
// This is the coverage guarantee behind splitting the same-origin gate out of
// RequireAuth (#2554). While the gate lived inside RequireAuth it could not be
// forgotten — one wrapper gave you both checks. Once it is a separate link in
// apiChain, "route X got auth but not CSRF" becomes expressible, so something
// has to rule it out. A source-level assertion ("nobody calls RequireAuth
// directly") would only pin today's spelling; this drives the actual mux and
// fails on any route that answers a cross-origin write.
//
// Route list comes from testdata/routes.golden.json — the same anti-drift gate
// the snapshot test uses — so a new mutating endpoint is covered the moment it
// lands in the golden, without anyone remembering to extend this test.
func TestAPICrossOriginRejected(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("testdata", "routes.golden.json"))
	if err != nil {
		t.Fatalf("read routes golden: %v", err)
	}
	var routes []routeEntry
	if err := json.Unmarshal(raw, &routes); err != nil {
		t.Fatalf("parse routes golden: %v", err)
	}

	const token = "csrf-coverage-token"
	router := session.NewRouter(session.RouterConfig{})
	srv := NewWithOptions(ServerOptions{
		Addr:           ":0",
		Router:         router,
		Platforms:      nil,
		Backend:        "claude",
		DashboardToken: token,
	})
	t.Cleanup(srv.appCancel)
	cookie := &http.Cookie{Name: auth.AuthCookieName, Value: srv.auth.CookieMAC()}

	var checked int
	for _, rt := range routes {
		if !strings.HasPrefix(rt.Path, "/api/") {
			continue
		}
		if auth.IsSafeMethod(rt.Method) || rt.Method == "" {
			continue
		}
		// /api/auth/* GRANTS auth rather than consuming it; HandleLogin does its
		// own origin check (a different message), and logout is idempotent.
		if strings.HasPrefix(rt.Path, "/api/auth/") {
			continue
		}
		// Concrete path for wildcard patterns: the gate runs before routing
		// params matter, but the request still has to match a pattern.
		p := strings.NewReplacer("{run_id}", "r1", "{id}", "i1", "{slug}", "s1", "{file}", "f1").Replace(rt.Path)

		req := httptest.NewRequest(rt.Method, "http://naozhi.example"+p, strings.NewReader("{}"))
		req.Host = "naozhi.example"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://evil.example")
		req.AddCookie(cookie)

		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		checked++

		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s: cross-origin write answered %d, want 403 — this route is reachable "+
				"by a sibling-origin attacker riding the victim's cookie", rt.Method, p, w.Code)
			continue
		}
		if body := w.Body.String(); !strings.Contains(body, "cross-origin") {
			t.Errorf("%s %s: 403 body %q does not name the reason; a 403 from some other "+
				"check would make this test pass for the wrong reason", rt.Method, p, body)
		}
	}

	// A refactor that stopped the golden from listing mutating routes (or this
	// loop from matching any) would make the test vacuously green.
	if checked < 20 {
		t.Fatalf("only %d mutating /api/ routes exercised; the golden or the filter above is wrong", checked)
	}
	t.Logf("cross-origin rejection verified on %d mutating /api/ routes", checked)
}

// TestAPISameOriginAllowed is the negative control: the same requests with a
// matching Origin must NOT be refused by the origin gate. Without it, a gate
// that rejected everything would satisfy the test above.
func TestAPISameOriginAllowed(t *testing.T) {
	t.Parallel()

	const token = "csrf-coverage-token-2"
	router := session.NewRouter(session.RouterConfig{})
	srv := NewWithOptions(ServerOptions{
		Addr:           ":0",
		Router:         router,
		Backend:        "claude",
		DashboardToken: token,
	})
	t.Cleanup(srv.appCancel)

	req := httptest.NewRequest(http.MethodPost, "http://naozhi.example/api/sessions/bind", strings.NewReader(`{}`))
	req.Host = "naozhi.example"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://naozhi.example")
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: srv.auth.CookieMAC()})

	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), "cross-origin") {
		t.Fatalf("same-origin write refused as cross-origin: %d %s", w.Code, w.Body.String())
	}
}
