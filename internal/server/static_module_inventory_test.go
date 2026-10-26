package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStaticJS_ModuleInventory holds every static/*.js except sw.js to what
// production needs to serve it; e2e cannot see a gap, since mock-server.js
// serves modules from disk. Each module must be in the embedded asset table
// (staticAssetBytes non-nil) and, through the real mux in token mode, answer
// 401 anonymously and 200 with exactly its own bytes when authenticated. Vendored scripts are not
// modules; TestVendorRoutes_ServeEmbeddedTree covers their routes.
func TestStaticJS_ModuleInventory(t *testing.T) {
	t.Parallel()

	modules := dashboardModules(t)

	// static/ is embedded wholesale, so a stray copy of the e2e shim (or any
	// nzTest hook) would ship in the binary; it belongs in test/e2e/e2e-shim.js.
	for key, a := range staticAssets {
		for _, marker := range []string{"nzTest", "nz.test"} {
			if bytes.Contains(a.bytes, []byte(marker)) {
				t.Errorf("%s contains %q: the e2e test surface belongs in test/e2e/e2e-shim.js", key, marker)
			}
		}
	}

	const token = "module-inventory-token"
	srv := newTestServerWithToken(&mockPlatform{}, token)

	for _, name := range modules {
		want := staticAssetBytes(name)
		if want == nil {
			t.Errorf("static/%s is not in the embedded asset table (static_assets.go)", name)
			continue
		}
		path := "/static/" + name

		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s = %d, want 401", path, w.Code)
		}
		if bytes.Contains(w.Body.Bytes(), want[:min(len(want), 64)]) {
			t.Errorf("anonymous GET %s: the 401 body carries the module's source", path)
		}

		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w = httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("authenticated GET %s = %d, want 200", path, w.Code)
			continue
		}
		if !bytes.Equal(w.Body.Bytes(), want) {
			t.Errorf("authenticated GET %s does not answer static/%s's bytes (%d bytes, want %d)", path, name, w.Body.Len(), len(want))
		}
	}
}
