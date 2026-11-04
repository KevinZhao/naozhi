package project

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPreviewToken(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	g := previewGrant{root: "session:dashboard:direct:a:general", dir: "docs/site\nodd", exp: now.Add(previewTTL).Unix()}
	tok := signPreview(g)
	p64, m64, _ := strings.Cut(tok, ".")
	otherKey := func(payload string) string {
		m := hmac.New(sha256.New, []byte("not-the-process-key-0123456789ab"))
		m.Write([]byte(previewMACContext + payload))
		return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	}
	retarget := "9999999999\ndemo\n"
	cases := []struct {
		name string
		tok  string
		now  time.Time
		ok   bool
	}{
		{"valid", tok, now, true},
		{"just before expiry", tok, now.Add(previewTTL - time.Second), true},
		{"expired", tok, now.Add(previewTTL), false},
		{"empty", "", now, false},
		{"no mac", p64, now, false},
		{"mac flipped", p64 + "." + flip(m64), now, false},
		{"payload rewritten", base64.RawURLEncoding.EncodeToString([]byte(retarget)) + "." + m64, now, false},
		{"signed with another key", base64.RawURLEncoding.EncodeToString([]byte(retarget)) + "." + otherKey(retarget), now, false},
		{"bad base64", "!!." + m64, now, false},
		{"too long", strings.Repeat("A", maxPreviewTokenLen) + "." + m64, now, false},
	}
	for _, tc := range cases {
		got, ok := parsePreview(tc.tok, tc.now)
		if ok != tc.ok {
			t.Errorf("%s: ok = %v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if ok && got != g {
			t.Errorf("%s: grant = %+v, want %+v", tc.name, got, g)
		}
	}
}

func flip(s string) string {
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

func TestPreviewCSP(t *testing.T) {
	t.Parallel()
	csp := previewCSP("127.0.0.1:8180", "TOK")
	for _, want := range []string{
		"default-src 'none'",
		"script-src http://127.0.0.1:8180/api/preview/TOK/ https://127.0.0.1:8180/api/preview/TOK/ 'unsafe-inline'",
		"connect-src http://127.0.0.1:8180/api/preview/TOK/ https://127.0.0.1:8180/api/preview/TOK/",
		"form-action 'none'",
		"frame-ancestors 'self'",
		"sandbox allow-scripts",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("csp %q lacks %q", csp, want)
		}
	}
	// A Host that would smuggle in a directive names no source at all.
	if csp := previewCSP("evil; script-src *", "TOK"); strings.Contains(csp, "*") || !strings.Contains(csp, "script-src 'none'") {
		t.Errorf("forged host leaked into csp: %q", csp)
	}
}

// previewLink mints a link through HandlePreviewLink and returns its status
// and URL.
func previewLink(t *testing.T, h *Handlers, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.HandlePreviewLink(w, httptest.NewRequest(http.MethodPost, "/api/preview", strings.NewReader(body)))
	var res struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	return w.Code, res.URL
}

func TestPreviewLink(t *testing.T) {
	t.Parallel()
	h, name, _ := newProjectHandlersForTest(t, map[string]string{
		"site/index.html":         "<!doctype html><title>x</title>",
		"site/app.js":             "1",
		"secrets/index.html":      "<!doctype html>",
		"site/report-token.html":  "<!doctype html>",
		"site/nested/page.html":   "<!doctype html>",
		"site/Weird Name #1.html": "<!doctype html>",
		"site/report":             "<!doctype html><title>x</title>",
		"site/index.php":          "<!doctype html><title>x</title>"})
	cases := []struct {
		name, body string
		status     int
		suffix     string
	}{
		{"html", `{"project":"` + name + `","path":"site/index.html"}`, 200, "/index.html"},
		{"nested html", `{"project":"` + name + `","path":"site/nested/page.html"}`, 200, "/page.html"},
		{"escaped name", `{"project":"` + name + `","path":"site/Weird Name #1.html"}`, 200, "/Weird%20Name%20%231.html"},
		{"not html", `{"project":"` + name + `","path":"site/app.js"}`, 415, ""},
		// HTML bytes under a name the asset route does not serve as HTML.
		{"html without extension", `{"project":"` + name + `","path":"site/report"}`, 415, ""},
		{"html as php", `{"project":"` + name + `","path":"site/index.php"}`, 415, ""}, {"sensitive dir", `{"project":"` + name + `","path":"secrets/index.html"}`, 404, ""},
		{"sensitive name", `{"project":"` + name + `","path":"site/report-token.html"}`, 404, ""},
		{"missing", `{"project":"` + name + `","path":"site/nope.html"}`, 404, ""},
		{"escape", `{"project":"` + name + `","path":"../demo/site/index.html"}`, 404, ""},
		{"absolute", `{"project":"` + name + `","path":"/etc/hosts"}`, 404, ""},
		{"unknown project", `{"project":"ghost","path":"site/index.html"}`, 404, ""},
		{"no path", `{"project":"` + name + `"}`, 400, ""},
		{"unknown field", `{"project":"` + name + `","path":"site/index.html","dir":"/"}`, 400, ""},
	}
	for _, tc := range cases {
		code, u := previewLink(t, h, tc.body)
		if code != tc.status {
			t.Errorf("%s: status %d, want %d", tc.name, code, tc.status)
			continue
		}
		if tc.suffix != "" && (!strings.HasPrefix(u, "/api/preview/") || !strings.HasSuffix(u, tc.suffix)) {
			t.Errorf("%s: url %q, want /api/preview/…%s", tc.name, u, tc.suffix)
		}
	}
}

// TestPreviewAsset drives a minted link through a mux with the production
// pattern, so the path wildcard decodes as it does in the server.
func TestPreviewAsset(t *testing.T) {
	t.Parallel()
	h, name, projDir := newProjectHandlersForTest(t, map[string]string{
		"site/index.html":      "<!doctype html><link rel=stylesheet href=style.css>",
		"site/style.css":       "body{color:red}",
		"site/js/app.js":       "console.log(1)",
		"site/data.json":       `{"v":1}`,
		"site/main.go":         "package main",
		"site/secrets/k.json":  `{"k":1}`,
		"site/api_token.json":  `{"t":1}`,
		"site/Secrets/up.json": `{"k":2}`,
		"top.css":              "outside the dir",
		"other/x.css":          "sibling dir",
		// Credentials the sensitive names miss: no dot entry is served.
		".mcp.json":                        `{"k":"mcp"}`,
		"site/.claude/settings.local.json": `{"k":"cc"}`,
		"site/.well/x.css":                 "a{}",
		"site/.env.json":                   `{"k":"e"}`,
	})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "o.css"), []byte("outside the root"), 0o644); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"site/to-root.css":  filepath.Join(projDir, "top.css"),
		"site/to-other":     filepath.Join(projDir, "other"),
		"site/escape.css":   filepath.Join(outside, "o.css"),
		"site/self-link.js": filepath.Join(projDir, "site", "js", "app.js"),
		"site/mcp.json":     filepath.Join(projDir, ".mcp.json"),
	} {
		if err := os.Symlink(target, filepath.Join(projDir, link)); err != nil {
			t.Fatal(err)
		}
	}
	big := filepath.Join(projDir, "site", "big.js")
	if f, err := os.Create(big); err != nil {
		t.Fatal(err)
	} else {
		_ = f.Truncate(maxRawBytes + 1)
		f.Close()
	}

	now := time.Unix(1_800_000_000, 0)
	h.now = func() time.Time { return now }
	code, link := previewLink(t, h, `{"project":"`+name+`","path":"site/index.html"}`)
	if code != 200 {
		t.Fatalf("mint: %d", code)
	}
	prefix := strings.TrimSuffix(link, "index.html")
	tok := strings.TrimSuffix(strings.TrimPrefix(prefix, "/api/preview/"), "/")
	rootTok := signPreview(previewGrant{root: name, exp: now.Add(time.Hour).Unix()})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/preview/{token}/{path...}", h.HandlePreviewAsset)
	get := func(p, dest string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8180"+p, nil)
		if dest != "" {
			req.Header.Set("Sec-Fetch-Dest", dest)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}

	cases := []struct {
		name, path, dest string
		status           int
		ctype, body      string
	}{
		{"page in a frame", "index.html", "iframe", 200, "text/html; charset=utf-8", "<!doctype html>"},
		{"stylesheet", "style.css", "style", 200, "text/css; charset=utf-8", "body{color:red}"},
		{"script in a subdir", "js/app.js", "script", 200, "text/javascript; charset=utf-8", "console.log(1)"},
		{"fetch", "data.json", "empty", 200, "application/json; charset=utf-8", `{"v":1}`},
		{"symlink inside the dir", "self-link.js", "script", 200, "text/javascript; charset=utf-8", "console.log(1)"},
		{"top-level navigation", "index.html", "document", 403, "", ""},
		{"no Sec-Fetch-Dest", "style.css", "", 403, "", ""},
		// The mux redirects a dot-dot path to its cleaned form, which drops
		// the token; the handler's own check is driven directly below.
		{"parent dir", "../top.css", "style", 307, "", ""},
		{"encoded parent dir", "%2e%2e/top.css", "style", 404, "", ""},
		{"encoded slash", "..%2ftop.css", "style", 404, "", ""},
		{"symlink to the root", "to-root.css", "style", 404, "", ""},
		{"symlinked dir to a sibling", "to-other/x.css", "style", 404, "", ""},
		{"symlink out of the root", "escape.css", "style", 404, "", ""},
		{"source file", "main.go", "empty", 404, "", ""},
		{"sensitive dir", "secrets/k.json", "empty", 404, "", ""},
		{"sensitive dir, other case", "Secrets/up.json", "empty", 404, "", ""},
		{"sensitive name", "api_token.json", "empty", 404, "", ""},
		{"missing", "nope.css", "style", 404, "", ""},
		{"too large", "big.js", "script", 404, "", ""},
	}
	for _, tc := range cases {
		w := get(prefix+tc.path, tc.dest)
		if w.Code != tc.status {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, w.Code, tc.status, w.Body.String())
			continue
		}
		if tc.status != 200 {
			if strings.Contains(w.Body.String(), "outside") || strings.Contains(w.Body.String(), "sibling") {
				t.Errorf("%s: refusal carries file bytes: %q", tc.name, w.Body.String())
			}
			continue
		}
		hd := w.Header()
		if hd.Get("Content-Type") != tc.ctype || !strings.HasPrefix(w.Body.String(), tc.body) {
			t.Errorf("%s: %q %q, want %q %q", tc.name, hd.Get("Content-Type"), w.Body.String(), tc.ctype, tc.body)
		}
		csp := hd.Get("Content-Security-Policy")
		if !strings.Contains(csp, "http://127.0.0.1:8180/api/preview/"+tok+"/ ") || !strings.Contains(csp, "sandbox allow-scripts") {
			t.Errorf("%s: csp %q", tc.name, csp)
		}
		if hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("Cache-Control") != "no-store" || hd.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: headers %v", tc.name, hd)
		}
	}

	for _, rel := range []string{"../top.css", "../other/x.css", "/etc/hosts", "js/../../top.css"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.SetPathValue("token", tok)
		req.SetPathValue("path", rel)
		req.Header.Set("Sec-Fetch-Dest", "style")
		w := httptest.NewRecorder()
		h.HandlePreviewAsset(w, req)
		if w.Code != 404 {
			t.Errorf("handler, path %q: %d, want 404", rel, w.Code)
		}
	}

	// A token for the root itself still stays inside the root.
	if w := get("/api/preview/"+rootTok+"/top.css", "style"); w.Code != 200 {
		t.Errorf("root token, top.css: %d", w.Code)
	}
	if w := get("/api/preview/"+rootTok+"/site/escape.css", "style"); w.Code != 404 {
		t.Errorf("root token, symlink out of the root: %d", w.Code)
	}
	for _, p := range []string{".mcp.json", "site/mcp.json", "site/.claude/settings.local.json", "site/.well/x.css", "site/.env.json"} {
		if w := get("/api/preview/"+rootTok+"/"+p, "empty"); w.Code != 404 {
			t.Errorf("root token, dot entry %s: %d, want 404", p, w.Code)
		}
	}
	if w := get(prefix+".claude/settings.local.json", "empty"); w.Code != 404 {
		t.Errorf("dot dir below the token's dir: %d, want 404", w.Code)
	}
	// Forged, retargeted and expired tokens are refused before any read.
	forged := signPreview(previewGrant{root: name, dir: "site", exp: now.Add(time.Hour).Unix()})
	for name, p := range map[string]string{
		"forged mac": "/api/preview/" + strings.Split(forged, ".")[0] + "." + flip(strings.Split(forged, ".")[1]) + "/style.css",
		"garbage":    "/api/preview/abc/style.css",
	} {
		if w := get(p, "style"); w.Code != 401 {
			t.Errorf("%s: %d, want 401", name, w.Code)
		}
	}
	now = now.Add(previewTTL)
	if w := get(prefix+"style.css", "style"); w.Code != 401 {
		t.Errorf("expired: %d, want 401", w.Code)
	}
}

// TestPreviewAsset_SessionRoot: a link under root=session:<key> follows the
// session's root rules, and stops working once the session is gone.
func TestPreviewAsset_SessionRoot(t *testing.T) {
	t.Parallel()
	h := newSessionRootHandlers(t)
	const key = "dashboard:direct:repo:general"
	repo := h.sessionRootPath(key)
	if err := os.MkdirAll(filepath.Join(repo, "site"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, c := range map[string]string{"site/index.html": "<!doctype html>", "site/a.css": "a{}"} {
		if err := os.WriteFile(filepath.Join(repo, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	code, link := previewLink(t, h, `{"root":"session:`+key+`","path":"site/index.html"}`)
	if code != 200 {
		t.Fatalf("mint: %d", code)
	}
	css := strings.TrimSuffix(link, "index.html") + "a.css"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/preview/{token}/{path...}", h.HandlePreviewAsset)
	get := func() int {
		req := httptest.NewRequest(http.MethodGet, css, nil)
		req.Header.Set("Sec-Fetch-Dest", "style")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}
	if c := get(); c != 200 {
		t.Fatalf("session root: %d", c)
	}
	delete(h.deps.Router.(*fakeRouter).workspaces, key)
	if c := get(); c != 404 {
		t.Fatalf("session gone: %d, want 404", c)
	}
}
