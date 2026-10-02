package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStaticJS_ModuleInventory holds every dashboard module on disk to the
// three things production needs to serve it. The Playwright suite cannot see
// a gap here: mock-server.js serves /static/*.js straight from disk, so a
// module with no embed entry or no Go route passes e2e and 404s in the real
// binary. For each static/*.js except sw.js:
//
//   - it is in the embedded asset table (staticAssetBytes, which is nil when
//     the embed entry is missing or failed to read);
//   - testdata/routes.golden.json lists GET /static/<name>, and every
//     /static/*.js route the golden lists is a file on disk;
//   - through the real mux in token mode, an anonymous GET answers 401 and an
//     authenticated GET answers 200 with exactly that module's bytes, so a
//     route wired to the wrong asset fails too.
//
// The route list is read from the golden JSON rather than routes.go: the
// golden is what routes_snapshot_test.go already keeps in step with the mux.
func TestStaticJS_ModuleInventory(t *testing.T) {
	t.Parallel()

	modules := dashboardModules(t)

	raw, err := os.ReadFile(filepath.Join("testdata", "routes.golden.json"))
	if err != nil {
		t.Fatalf("read routes golden: %v", err)
	}
	var routes []routeEntry
	if err := json.Unmarshal(raw, &routes); err != nil {
		t.Fatalf("parse routes golden: %v", err)
	}
	routed := map[string]bool{}
	for _, rt := range routes {
		name, ok := strings.CutPrefix(rt.Path, "/static/")
		if !ok || !strings.HasSuffix(name, ".js") {
			continue
		}
		if rt.Method != http.MethodGet {
			t.Errorf("routes golden: %s %s, want GET", rt.Method, rt.Path)
		}
		routed[name] = true
	}
	onDisk := map[string]bool{}
	for _, name := range modules {
		onDisk[name] = true
	}
	for name := range routed {
		if !onDisk[name] {
			t.Errorf("routes golden lists GET /static/%s but static/%s does not exist", name, name)
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
		if !routed[name] {
			t.Errorf("static/%s has no GET /static/%s in testdata/routes.golden.json (routes.go)", name, name)
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
