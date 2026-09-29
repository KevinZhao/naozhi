package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The e2e suite's instrumentation surface lives in test/e2e/e2e-shim.js,
// which only the mock server serves (#2941). No production asset carries it,
// and the real server has no route for the shim.
func TestStaticAssets_CarryNoTestSurface(t *testing.T) {
	t.Parallel()
	if len(staticAssets) < 30 {
		t.Fatalf("only %d static assets read: the scan below would check almost nothing", len(staticAssets))
	}
	for key, a := range staticAssets {
		for _, marker := range []string{"nzTest", "nz.test"} {
			if strings.Contains(string(a.bytes), marker) {
				t.Errorf("%s contains %q: the e2e test surface belongs in test/e2e/e2e-shim.js", key, marker)
			}
		}
	}
}

func TestE2EShim_NotServed(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})
	for _, p := range []string{"/e2e-shim.js", "/static/e2e-shim.js"} {
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, w.Code)
		}
	}
}
