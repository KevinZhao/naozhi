package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// TestAPIUnauthenticatedRejected drives EVERY /api/ route in the routes golden
// through the real mux with no credentials (no cookie, no Bearer) in token
// mode and asserts 401 from RequireAuth.
//
// TestAPICrossOriginRejected covers the origin link of apiChain but skips safe
// methods by design, and the routes golden records method/path/handler only.
// So until #2631 nothing behavioural would fail if a server-owned GET in
// routes.go were registered without `auth(...)`, or if a sub-package found a
// way to opt a route out of the chain — the golden would not change and CI
// would stay green. This test closes that gap: any /api/ route that answers
// anything other than 401 to an anonymous request fails here.
//
// Route list comes from testdata/routes.golden.json so a new endpoint is
// covered the moment it lands in the golden. The count is checked against the
// golden rather than a fixed floor: every /api/ route except /api/auth/* must
// be exercised, so a filter bug cannot silently shrink the scan.
func TestAPIUnauthenticatedRejected(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("testdata", "routes.golden.json"))
	if err != nil {
		t.Fatalf("read routes golden: %v", err)
	}
	var routes []routeEntry
	if err := json.Unmarshal(raw, &routes); err != nil {
		t.Fatalf("parse routes golden: %v", err)
	}

	router := session.NewRouter(session.RouterConfig{})
	srv := NewWithOptions(ServerOptions{
		Addr:           ":0",
		Router:         router,
		Backend:        "claude",
		DashboardToken: "unauth-coverage-token",
		// /api/debug/* are only mounted in debug mode; mount them so the scan
		// covers the routes the golden lists rather than a 404 subset.
		DebugMode: true,
	})
	t.Cleanup(srv.appCancel)

	// Concrete path for wildcard patterns: auth runs before routing params
	// matter, but the request still has to match a pattern.
	concrete := strings.NewReplacer("{run_id}", "r1", "{id}", "i1", "{slug}", "s1", "{file}", "f1")

	var want, checked int
	for _, rt := range routes {
		if !strings.HasPrefix(rt.Path, "/api/") {
			continue
		}
		// /api/auth/* GRANTS auth rather than consuming it (login, noscript
		// form target); logout is idempotent and authenticated separately.
		if strings.HasPrefix(rt.Path, "/api/auth/") {
			continue
		}
		want++
		method := rt.Method
		if method == "" {
			method = http.MethodGet
		}
		p := concrete.Replace(rt.Path)

		var body *strings.Reader
		if method == http.MethodGet || method == http.MethodHead {
			body = strings.NewReader("")
		} else {
			body = strings.NewReader("{}")
		}
		req := httptest.NewRequest(method, "http://naozhi.example"+p, body)
		req.Host = "naozhi.example"
		req.Header.Set("Content-Type", "application/json")
		// Same-origin so a 403 from the origin gate cannot mask a missing
		// auth link; the only acceptable answer is the auth link's 401.
		req.Header.Set("Origin", "http://naozhi.example")

		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		checked++

		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: anonymous request answered %d, want 401 — this route is mounted "+
				"without the auth link of apiChain (or the golden lists a route the mux does not serve)",
				method, p, w.Code)
			continue
		}
		if b := w.Body.String(); !strings.Contains(b, "unauthorized") {
			t.Errorf("%s %s: 401 body %q does not come from RequireAuth; a 401 from some other "+
				"check would make this test pass for the wrong reason", method, p, b)
		}
	}

	// Vacuity guards: every non-auth /api/ route in the golden must have been
	// exercised, and the golden must still list a meaningful number of them.
	if checked != want {
		t.Fatalf("exercised %d routes but the golden lists %d non-auth /api/ routes", checked, want)
	}
	if checked < 40 {
		t.Fatalf("only %d /api/ routes exercised; the golden or the filter above is wrong", checked)
	}
	t.Logf("anonymous-request rejection verified on %d /api/ routes", checked)
}

// TestAPIAuthenticatedNotRejectedByAuthLink is the negative control: the same
// anonymous-vs-authenticated distinction must actually be what produces the
// 401 above. With a valid cookie a GET must NOT answer 401, otherwise a
// RequireAuth that rejected everything would satisfy the scan.
func TestAPIAuthenticatedNotRejectedByAuthLink(t *testing.T) {
	t.Parallel()

	router := session.NewRouter(session.RouterConfig{})
	srv := NewWithOptions(ServerOptions{
		Addr:           ":0",
		Router:         router,
		Backend:        "claude",
		DashboardToken: "unauth-coverage-token-2",
	})
	t.Cleanup(srv.appCancel)

	req := httptest.NewRequest(http.MethodGet, "http://naozhi.example/api/sessions", nil)
	req.Host = "naozhi.example"
	req.Header.Set("Authorization", "Bearer unauth-coverage-token-2")

	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("authenticated GET /api/sessions answered 401: %s", w.Body.String())
	}
}
