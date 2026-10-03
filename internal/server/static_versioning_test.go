package server

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var (
	servedStaticAttrRe = regexp.MustCompile(`(?:src|href)="/static/([^"?#]+)(\?v=[^"]*)?"`)
	modulePreloadRe    = regexp.MustCompile(`<link rel="modulepreload" href="([^"]+)">`)
	moduleScriptRe     = regexp.MustCompile(`<script type="module" src="/static/([^"?]+)\?v=([^"]+)"></script>`)
	assetVersionMetaRe = regexp.MustCompile(`<meta name="nz-asset-version" content="([0-9a-f]{16})">`)
)

// rawStaticAssets is staticAssets with the page as written in place of the
// rendered one: the input renderDashboardHTML gets at init.
func rawStaticAssets(t *testing.T) map[string]staticAsset {
	t.Helper()
	out := maps.Clone(staticAssets)
	out["dashboard.html"] = newStaticAsset(rawDashboardHTML(t), true)
	return out
}

func servedDashboardPage(t *testing.T) string {
	t.Helper()
	page := staticAssetBytes("dashboard.html")
	if page == nil {
		t.Fatal("dashboard.html not embedded")
	}
	return string(page)
}

func importMapOf(t *testing.T, page string) map[string]string {
	t.Helper()
	m := dashImportMapRe.FindAllStringSubmatch(page, -1)
	if len(m) != 1 {
		t.Fatalf("page carries %d import maps, want 1", len(m))
	}
	var im struct {
		Imports map[string]string `json:"imports"`
	}
	if err := json.Unmarshal([]byte(m[0][1]), &im); err != nil {
		t.Fatalf("import map is not valid JSON: %v", err)
	}
	return im.Imports
}

// Every stylesheet and module the page links names its asset's current hash,
// and none of the raw page's links is lost.
func TestDashboardPage_StaticURLsVersioned(t *testing.T) {
	t.Parallel()
	page := servedDashboardPage(t)
	hits := servedStaticAttrRe.FindAllStringSubmatch(page, -1)
	rawHits := servedStaticAttrRe.FindAllStringSubmatch(string(rawDashboardHTML(t)), -1)
	modules := len(dashboardModuleKeys(staticAssets))
	if len(hits) != len(rawHits)+modules {
		t.Errorf("served page links %d /static/ URLs, want the raw page's %d plus %d modulepreloads", len(hits), len(rawHits), modules)
	}
	for _, h := range hits {
		want := "?v=" + assetURLVersion(staticAssets[h[1]].etag)
		if want == "?v=" || h[2] != want {
			t.Errorf("/static/%s linked with %q, want %q", h[1], h[2], want)
		}
	}
}

// The import map covers exactly the module set, each entry pointing at the
// versioned URL; the page's own module tags use the same URLs (one module
// instance per file, not one per URL form); the map comes before anything
// that loads a module; modulepreload fetches exactly the map's targets.
func TestDashboardPage_ImportMapAndPreload(t *testing.T) {
	t.Parallel()
	page := servedDashboardPage(t)
	imports := importMapOf(t, page)

	modules := dashboardModules(t)
	if len(imports) != len(modules) {
		t.Errorf("import map has %d entries, want one per module (%d)", len(imports), len(modules))
	}
	for _, name := range modules {
		want := "/static/" + name + "?v=" + assetURLVersion(staticAssets[name].etag)
		if got := imports["/static/"+name]; got != want {
			t.Errorf("import map /static/%s -> %q, want %q", name, got, want)
		}
	}

	scripts := moduleScriptRe.FindAllStringSubmatch(page, -1)
	if len(scripts) == 0 {
		t.Fatal("served page has no versioned module script tags")
	}
	for _, s := range scripts {
		if tag := "/static/" + s[1] + "?v=" + s[2]; imports["/static/"+s[1]] != tag {
			t.Errorf("<script src=%q> differs from the import map's %q: the module would load twice", tag, imports["/static/"+s[1]])
		}
	}

	mapAt := strings.Index(page, `<script type="importmap">`)
	for _, marker := range []string{`<script type="module"`, `<link rel="modulepreload"`} {
		if at := strings.Index(page, marker); at < mapAt {
			t.Errorf("%s at %d precedes the import map at %d", marker, at, mapAt)
		}
	}
	if headEnd := strings.Index(page, "</head>"); mapAt > headEnd {
		t.Errorf("import map at %d is outside <head> (ends %d)", mapAt, headEnd)
	}

	preloads := map[string]bool{}
	for _, m := range modulePreloadRe.FindAllStringSubmatch(page, -1) {
		preloads[m[1]] = true
	}
	targets := map[string]bool{}
	for _, v := range imports {
		targets[v] = true
	}
	if !maps.Equal(preloads, targets) {
		t.Errorf("modulepreload set (%d) differs from the import map targets (%d)", len(preloads), len(targets))
	}

	if m := assetVersionMetaRe.FindStringSubmatch(page); m == nil {
		t.Error("served page carries no nz-asset-version meta")
	} else if want := dashboardAssetVersion(rawStaticAssets(t)); m[1] != want {
		t.Errorf("nz-asset-version = %s, want %s", m[1], want)
	}
}

// Rendering is a pure function of the assets, and editing one module moves
// only that module's URL and the asset version, so a release re-downloads
// only what changed.
func TestRenderDashboardHTML_DeterministicAndPerFile(t *testing.T) {
	t.Parallel()
	assets := rawStaticAssets(t)
	raw := assets["dashboard.html"].bytes
	first, err := renderDashboardHTML(raw, assets)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != servedDashboardPage(t) {
		t.Error("rendering the raw page again differs from the page served")
	}
	for range 3 {
		again, err := renderDashboardHTML(raw, maps.Clone(assets))
		if err != nil || string(again) != string(first) {
			t.Fatalf("rendering is not deterministic (err %v)", err)
		}
	}

	edited := maps.Clone(assets)
	edited["ws_manager.js"] = newStaticAsset(append([]byte("// edited\n"), assets["ws_manager.js"].bytes...), true)
	next, err := renderDashboardHTML(raw, edited)
	if err != nil {
		t.Fatal(err)
	}
	before, after := importMapOf(t, string(first)), importMapOf(t, string(next))
	for k, v := range before {
		changed := after[k] != v
		if want := k == "/static/ws_manager.js"; changed != want {
			t.Errorf("editing ws_manager.js: import map entry %s changed=%v, want %v", k, changed, want)
		}
	}
	if assetVersionMetaRe.FindString(string(first)) == assetVersionMetaRe.FindString(string(next)) {
		t.Error("editing ws_manager.js left nz-asset-version unchanged")
	}
}

func TestRenderDashboardHTML_Errors(t *testing.T) {
	t.Parallel()
	assets := rawStaticAssets(t)
	for name, raw := range map[string]string{
		"unembedded asset": `<head></head><script type="module" src="/static/nope.js"></script>`,
		"no </head>":       `<script type="module" src="/static/dashboard.js"></script>`,
		"two </head>":      `<head></head></head>`,
	} {
		if _, err := renderDashboardHTML([]byte(raw), assets); err == nil {
			t.Errorf("%s: renderDashboardHTML returned no error", name)
		}
	}
}

// The browser may keep a static asset for a year only when the URL names its
// current bytes; an unversioned or stale URL revalidates. Checked through the
// real mux for a module and a stylesheet, on 200 and on the 304 path.
func TestStaticCacheControl_ThroughMux(t *testing.T) {
	t.Parallel()
	const token = "static-cache-token"
	srv := newTestServerWithToken(&mockPlatform{}, token)
	const (
		immutable   = "private, max-age=31536000, immutable"
		revalidated = "no-cache, must-revalidate"
	)
	for _, key := range []string{"ws_manager.js", "css/tokens.css"} {
		v := assetURLVersion(staticAssets[key].etag)
		stale := strings.Repeat("0", 16)
		for _, tc := range []struct {
			name, query, inm, want string
			code                   int
		}{
			{"current v", "?v=" + v, "", immutable, http.StatusOK},
			{"current v, 304", "?v=" + v, staticAssets[key].etag, immutable, http.StatusNotModified},
			{"no v", "", "", revalidated, http.StatusOK},
			{"no v, 304", "", staticAssets[key].etag, revalidated, http.StatusNotModified},
			{"stale v", "?v=" + stale, "", revalidated, http.StatusOK},
			{"empty v", "?v=", "", revalidated, http.StatusOK},
		} {
			req := httptest.NewRequest(http.MethodGet, "/static/"+key+tc.query, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			if tc.inm != "" {
				req.Header.Set("If-None-Match", tc.inm)
			}
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, req)
			if w.Code != tc.code {
				t.Errorf("%s %s: status %d, want %d", key, tc.name, w.Code, tc.code)
			}
			if got := w.Header().Get("Cache-Control"); got != tc.want {
				t.Errorf("%s %s: Cache-Control %q, want %q", key, tc.name, got, tc.want)
			}
		}
	}
}

// The /dashboard response is the rendered page, its CSP admits the page's
// import map by hash, and the policy the raw page derives (the e2e mock's)
// does not carry that hash.
func TestDashboardCSP_AdmitsServedImportMap(t *testing.T) {
	t.Parallel()
	const token = "csp-importmap-token"
	srv := newTestServerWithToken(&mockPlatform{}, token)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /dashboard = %d", w.Code)
	}
	body := w.Body.String()
	if body != servedDashboardPage(t) {
		t.Fatal("GET /dashboard does not answer the rendered page")
	}
	m := dashImportMapRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("served /dashboard carries no import map")
	}
	hash := cspHash(m[1])
	csp := w.Header().Get("Content-Security-Policy")
	var scriptSrc string
	for _, d := range strings.Split(csp, ";") {
		if d = strings.TrimSpace(d); strings.HasPrefix(d, "script-src ") {
			scriptSrc = d
		}
	}
	if !strings.Contains(scriptSrc, hash) {
		t.Errorf("script-src %q does not admit the served import map %s", scriptSrc, hash)
	}
	if raw := buildDashboardCSP(rawDashboardHTML(t)); strings.Contains(raw, hash) {
		t.Error("the raw page's policy carries the import map hash")
	}
}
