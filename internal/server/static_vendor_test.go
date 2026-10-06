package server

import (
	"crypto/sha512"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// vendorPinRe matches one [url, sha384] pair render_md.js hands its loader.
var vendorPinRe = regexp.MustCompile(`\['/static/(vendor/[^']+)',\s*'sha384-([^']+)'\]`)

// TestVendorAssets_SRIMatchesEmbedded checks every vendored asset render_md.js
// injects against the bytes the binary serves: a hash that drifts from the
// file makes the browser refuse it, and the formula never renders. In reverse,
// every vendored script and stylesheet has a pin, so none is served unloaded.
func TestVendorAssets_SRIMatchesEmbedded(t *testing.T) {
	t.Parallel()
	pins := vendorPinRe.FindAllStringSubmatch(string(staticAssetBytes("render_md.js")), -1)
	if len(pins) < 2 {
		t.Fatalf("found %d vendored [url, sha384] pins in render_md.js, want the KaTeX script and stylesheet (regex drift?)", len(pins))
	}
	pinned := map[string]bool{}
	for _, p := range pins {
		key, want := p[1], p[2]
		pinned[key] = true
		b := staticAssetBytes(key)
		if b == nil {
			t.Errorf("render_md.js loads /static/%s, which is not embedded", key)
			continue
		}
		sum := sha512.Sum384(b)
		if got := base64.StdEncoding.EncodeToString(sum[:]); got != want {
			t.Errorf("/static/%s: render_md.js pins sha384-%s, the embedded file is sha384-%s", key, want, got)
		}
	}
	for key := range staticAssets {
		if strings.HasPrefix(key, "vendor/") && (strings.HasSuffix(key, ".js") || strings.HasSuffix(key, ".css")) && !pinned[key] {
			t.Errorf("%s is embedded but render_md.js pins no SRI hash for it", key)
		}
	}
}

// TestVendorAssets_DirectoryNamesRelease: vendor/ responses are immutable on
// the strength of their path, so the KaTeX build embedded must be the release
// its directory names. Changed bytes need a new directory, not a new pin.
func TestVendorAssets_DirectoryNamesRelease(t *testing.T) {
	t.Parallel()
	dirs := map[string]bool{}
	for key := range staticAssets {
		if rest, ok := strings.CutPrefix(key, "vendor/"); ok {
			dir, _, _ := strings.Cut(rest, "/")
			dirs[dir] = true
		}
	}
	if len(dirs) != 1 || !dirs["katex-0.16.21"] {
		t.Fatalf("vendor/ holds %v, want katex-0.16.21 alone: a new library or release needs its own release check here", dirs)
	}
	js := string(staticAssetBytes("vendor/katex-0.16.21/katex.min.js"))
	if got := regexp.MustCompile(`version:"([^"]+)"`).FindAllStringSubmatch(js, -1); len(got) != 1 || got[0][1] != "0.16.21" {
		t.Errorf("vendor/katex-0.16.21/katex.min.js declares versions %v, want exactly 0.16.21", got)
	}
	if css := string(staticAssetBytes("vendor/katex-0.16.21/katex.min.css")); !strings.Contains(css, `.katex .katex-version:after{content:"0.16.21"}`) {
		t.Error("vendor/katex-0.16.21/katex.min.css does not carry the 0.16.21 version marker")
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
		if w.Body.String() != string(staticAssetBytes(key)) {
			t.Errorf("GET /static/%s body differs from the embedded file", key)
		}
	}
}

// TestVendorRoutes_RejectOutsideTree pins the table lookup: only embedded
// stylesheets, fonts and the scripts with a route of their own are served.
func TestVendorRoutes_RejectOutsideTree(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})
	for _, p := range []string{
		"/static/vendor/katex-0.16.21/LICENSE",
		"/static/vendor/katex-0.16.21/fonts/KaTeX_Nope.woff2",
		"/static/vendor/katex-0.16.21/katex.css",
		"/static/vendor/tokens.css",
		"/static/vendor/css/tokens.css",
		"/static/vendor/%2e%2e/css/tokens.css",
	} {
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code == http.StatusOK {
			t.Errorf("GET %s = 200, want a miss", p)
		}
	}
	// The stylesheet handler serves no script, even an embedded one.
	w := httptest.NewRecorder()
	handleDashboardCSS(w, httptest.NewRequest(http.MethodGet, "/static/vendor/katex-0.16.21/katex.min.js", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("handleDashboardCSS served a script: %d, want 404", w.Code)
	}
	// The dashboard's own stylesheets still resolve through the same handler.
	w = httptest.NewRecorder()
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
	} {
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET %s = %d, want 401", p, w.Code)
		}
	}
}

// TestDashboardCSP_KatexSelfHosted: with KaTeX vendored, styles and fonts come
// from the dashboard's own origin only, and no KaTeX source is allowlisted.
func TestDashboardCSP_KatexSelfHosted(t *testing.T) {
	t.Parallel()
	directives := map[string]string{}
	for _, d := range strings.Split(dashboardCSP, ";") {
		if name, value, ok := strings.Cut(strings.TrimSpace(d), " "); ok {
			directives[name] = value
		}
	}
	for _, name := range []string{"style-src", "font-src"} {
		if got := directives[name]; got != "'self'" {
			t.Errorf("CSP %s = %q, want 'self' (KaTeX is served from /static/vendor)", name, got)
		}
	}
	if strings.Contains(dashboardCSP, "katex") {
		t.Errorf("CSP still allowlists a KaTeX source: %q", dashboardCSP)
	}
}

// TestDashboardHTML_DoesNotPreloadVendor: the vendored scripts are classic
// scripts render_md.js loads only when a message holds math, so the page
// neither maps nor preloads them as modules.
func TestDashboardHTML_DoesNotPreloadVendor(t *testing.T) {
	t.Parallel()
	if page := string(staticAssetBytes("dashboard.html")); strings.Contains(page, "/static/vendor/") {
		t.Error("dashboard.html as served names a /static/vendor/ asset; vendored scripts load on demand")
	}
}
