package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIntegrationDashboardStack drives whole requests through the real
// *Server.mux — exercising the assembled auth middleware + route table +
// handler chain together rather than calling a single handler in isolation.
//
// This is the first integration-level test in internal/server, addressing the
// R243-ARCH-20 (#864) symptom that `rg ^func TestFull|TestIntegration|TestE2E`
// returned 0 hits: there was no test that wired the components and pushed a
// request through the public HTTP surface. It is intentionally hermetic (no
// testcontainers, no live Claude process) so it runs in the normal `go test`
// path; the broader testcontainers harness the issue proposes can layer on top
// of the same mux entry point.
func TestIntegrationDashboardStack(t *testing.T) {
	t.Run("token-protected server rejects unauthenticated API request", func(t *testing.T) {
		srv := newTestServerWithToken(&mockPlatform{}, "s3cret-token")

		req := httptest.NewRequest(http.MethodGet, "/api/system/daemons", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		// The full auth middleware must intercept before the handler runs:
		// no token cookie / Bearer header => not 200.
		if w.Code == http.StatusOK {
			t.Fatalf("unauthenticated request reached handler (status 200); "+
				"auth middleware in the mux chain must reject it; body=%q",
				w.Body.String())
		}
	})

	t.Run("authenticated API request flows through the assembled mux", func(t *testing.T) {
		// No dashboard token => single-user mode, requireAuth is a pass-through,
		// so the request traverses the real route table and handler unauthed.
		srv := newTestServer(&mockPlatform{})

		req := httptest.NewRequest(http.MethodGet, "/api/system/daemons", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
		}
		// SysessionManager is nil in the harness; the disabled-path contract is
		// a valid empty JSON array, proving the handler ran end-to-end.
		var daemons []json.RawMessage
		body := strings.TrimSpace(w.Body.String())
		if err := json.Unmarshal([]byte(body), &daemons); err != nil {
			t.Fatalf("response is not a JSON array: %v; body=%q", err, body)
		}
		if len(daemons) != 0 {
			t.Errorf("daemons = %d entries, want 0 (sysession disabled)", len(daemons))
		}
	})

	t.Run("unknown route returns 404 through the mux", func(t *testing.T) {
		srv := newTestServer(&mockPlatform{})

		req := httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 for unmounted route", w.Code)
		}
	})
}
