package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strings"
	"testing"
)

// vendorContent is the release file an asset key serves: its bytes, or the
// decompression of a gzip-only asset's gz form.
func vendorContent(t *testing.T, key string) []byte {
	t.Helper()
	a, ok := staticAssets[key]
	if !ok || a.bytes != nil {
		return a.bytes
	}
	raw, err := gunzip(a.gz)
	if err != nil {
		t.Fatalf("%s: gzip form does not decompress: %v", key, err)
	}
	return raw
}

// TestVendorAssets_LargeFilesPrecompressed: a raw bundle would be gzipped at
// BestCompression during init, on every start (mermaid's 3 MB: 65-140ms against
// 11-18ms to gunzip), and would weigh its full size in the binary. Anything
// that large is embedded as <name>.gz instead, its gzip form served as is and
// the only form kept in memory (#3662).
func TestVendorAssets_LargeFilesPrecompressed(t *testing.T) {
	t.Parallel()
	err := fs.WalkDir(staticFS, "static/vendor", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !strings.HasSuffix(name, ".gz") && info.Size() > 1<<20 {
			t.Errorf("%s is %d bytes raw: embed it as %s.gz (gzip -9n)", name, info.Size(), path.Base(name))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	const key = "vendor/mermaid-11.14.0/mermaid.min.js"
	gz, err := staticFS.ReadFile("static/" + key + ".gz")
	if err != nil {
		t.Fatal(err)
	}
	a := staticAssets[key]
	if !bytes.Equal(a.gz, gz) {
		t.Errorf("%s: gzip form is not the embedded .gz", key)
	}
	if a.bytes != nil {
		t.Errorf("%s keeps its %d-byte decompression resident, want only the gzip form", key, len(a.bytes))
	}
	if raw := vendorContent(t, key); len(raw) < 3<<20 {
		t.Errorf("%s decompresses to %d bytes, want the 3 MB release bundle", key, len(raw))
	}
}

// TestVendorRoutes_ServeEmbeddedTree requests every vendored asset through the
// mux, plus every font the KaTeX stylesheet names, so a font left out of the
// embed or a route that misses a subdirectory fails here instead of rendering
// formulas in a fallback face.
func TestVendorRoutes_ServeEmbeddedTree(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})
	keys := map[string]bool{}
	for key := range staticAssets {
		if strings.HasPrefix(key, "vendor/") {
			keys[key] = true
		}
	}
	css := string(staticAssetBytes("vendor/katex-0.16.21/katex.min.css"))
	fonts := regexp.MustCompile(`url\((fonts/[^)]+\.woff2)\)`).FindAllStringSubmatch(css, -1)
	if len(fonts) < 20 {
		t.Fatalf("KaTeX stylesheet names %d woff2 fonts, want 20 (stylesheet missing?)", len(fonts))
	}
	for _, f := range fonts {
		keys["vendor/katex-0.16.21/"+f[1]] = true
	}
	types := map[string]string{".js": "application/javascript", ".css": "text/css; charset=utf-8", ".woff2": "font/woff2"}
	for key := range keys {
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/static/"+key, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET /static/%s = %d, want 200", key, w.Code)
			continue
		}
		if got, want := w.Header().Get("Content-Type"), types[key[strings.LastIndex(key, "."):]]; got != want {
			t.Errorf("GET /static/%s Content-Type = %q, want %q", key, got, want)
		}
		if got := w.Header().Get("Cache-Control"); got != "private, max-age=31536000, immutable" {
			t.Errorf("GET /static/%s Cache-Control = %q, want immutable: the path names the release", key, got)
		}
		if w.Body.String() != string(vendorContent(t, key)) {
			t.Errorf("GET /static/%s body differs from the embedded file", key)
		}
		if strings.HasSuffix(key, ".woff2") {
			continue
		}
		req := httptest.NewRequest(http.MethodGet, "/static/"+key, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		w = httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		if raw, err := gunzip(w.Body.Bytes()); w.Header().Get("Content-Encoding") != "gzip" || err != nil || !bytes.Equal(raw, vendorContent(t, key)) {
			t.Errorf("GET /static/%s with gzip = %q encoding, err %v: want the gzip form of the embedded file", key, w.Header().Get("Content-Encoding"), err)
		}
	}
}

// TestVendorRoutes_RejectOutsideTree pins the table lookup: only embedded
// scripts, stylesheets and fonts are served.
func TestVendorRoutes_RejectOutsideTree(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})
	for _, p := range []string{
		"/static/vendor/katex-0.16.21/LICENSE",
		"/static/vendor/mermaid-11.14.0/LICENSE",
		"/static/vendor/mermaid-11.14.0/mermaid.min.js.gz",
		"/static/vendor/mermaid-11.14.0/mermaid.js",
		"/static/vendor/katex-0.16.21/fonts/KaTeX_Nope.woff2",
		"/static/vendor/katex-0.16.21/katex.css",
		"/static/vendor/tokens.css",
		"/static/vendor/css/tokens.css",
		"/static/vendor/%2e%2e/css/tokens.css",
		// Embedded but not served below /static/: sw.js lives at /sw.js.
		"/static/sw.js",
		"/static/package.json",
		"/static/dashboard.html",
	} {
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code == http.StatusOK {
			t.Errorf("GET %s = 200, want a miss", p)
		}
	}
	// The dashboard's own stylesheets resolve through the same handler.
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/static/css/tokens.css", nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/css; charset=utf-8" {
		t.Errorf("GET /static/css/tokens.css = %d %q, want 200 text/css", w.Code, w.Header().Get("Content-Type"))
	}
}

// TestVendorRoutes_RequireAuth: in token mode the vendored files sit behind
// the same gate as every other /static/ asset.
func TestVendorRoutes_RequireAuth(t *testing.T) {
	t.Parallel()
	srv := newTestServerWithToken(&mockPlatform{}, "secret")
	for _, p := range []string{
		"/static/vendor/katex-0.16.21/katex.min.js",
		"/static/vendor/katex-0.16.21/katex.min.css",
		"/static/vendor/katex-0.16.21/fonts/KaTeX_Main-Regular.woff2",
		"/static/vendor/mermaid-11.14.0/mermaid.min.js",
	} {
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET %s = %d, want 401", p, w.Code)
		}
	}
}

// gunzip returns the decompressed form of b.
func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}
