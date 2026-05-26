package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestServeRender_CSPImgSrcDoesNotIncludeSelf locks the R245-SEC-10 (#833)
// contract: the serveRender CSP must NOT include `'self'` on the img-src
// directive. Even though the rendered document lives in an opaque blob
// origin (so its scripts cannot read dashboard cookies via fetch / DOM),
// <img src="https://dashboard/..."> still issues a network request to
// the dashboard origin. That gives a malicious workspace .html / .svg a
// dual-purpose oracle:
//
//  1. Phone-home / probe — the request lands in dashboard logs, leaking
//     the operator IP to whoever can write into the workspace.
//  2. Side-channel timing — onerror / load timing observed via the
//     Resource Timing API leaks endpoint state to the iframe even
//     under an opaque origin.
//
// The legitimate render targets (coverage reports, Playwright trace,
// pytest-html, single-file SVG diagrams) embed their assets inline, as
// data: URIs, or as blob: refs to fetched siblings. None of them need
// 'self' to render correctly. Lock the directive shape so a future CSP
// refactor cannot regress the property.
func TestServeRender_CSPImgSrcDoesNotIncludeSelf(t *testing.T) {
	h, proj, projDir := newProjectHandlersForTest(t, nil)
	htmlBytes := []byte(`<!doctype html><html><body><img src="/probe.png"></body></html>`)
	if err := os.WriteFile(filepath.Join(projDir, "report.html"), htmlBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+proj+"&path=report.html&mode=render", nil)
	w := httptest.NewRecorder()
	h.handleFileGet(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("CSP header missing")
	}

	// Locate the img-src directive and assert 'self' is absent. Other
	// directives (e.g. script-src 'unsafe-inline' for inline math) are
	// untouched by this contract — only img-src matters for #833.
	const imgPrefix = "img-src "
	idx := strings.Index(csp, imgPrefix)
	if idx < 0 {
		t.Fatalf("CSP missing img-src directive: %q", csp)
	}
	tail := csp[idx+len(imgPrefix):]
	if end := strings.Index(tail, ";"); end >= 0 {
		tail = tail[:end]
	}
	imgSrc := strings.TrimSpace(tail)
	if strings.Contains(imgSrc, "'self'") {
		t.Errorf("img-src must not contain 'self' (lets sandboxed render phone home to dashboard origin); got %q", imgSrc)
	}
	// Positive lock: the legitimate scheme list (data:, blob:) must remain
	// so legitimate inline / fetched-blob images keep rendering. Without
	// this guard a future refactor could drop too much and silently break
	// coverage report rendering.
	for _, want := range []string{"data:", "blob:"} {
		if !strings.Contains(imgSrc, want) {
			t.Errorf("img-src must continue to allow %q (legit inline/blob refs); got %q", want, imgSrc)
		}
	}
}
