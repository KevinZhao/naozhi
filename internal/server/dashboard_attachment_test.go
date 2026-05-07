package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// writeAttachmentFixture writes a fake image file under the workspace
// attachment directory and returns the workspace-relative path with
// forward slashes — matches the shape EventEntry.ImagePaths carries.
func writeAttachmentFixture(t *testing.T, ws string, subDir, filename string, body []byte) string {
	t.Helper()
	dir := filepath.Join(ws, ".naozhi", "attachments", subDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return ".naozhi/attachments/" + subDir + "/" + filename
}

func newAttachmentServer(t *testing.T, ws string) *Server {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	router := session.NewRouter(session.RouterConfig{Workspace: resolved})
	srv := New(":0", router, nil, nil, nil, nil, "claude", ServerOptions{})
	srv.registerDashboard()
	router.SetWorkspace("dash:direct:alice", resolved)
	return srv
}

func TestHandleAttachment_ServesImage(t *testing.T) {
	ws := t.TempDir()
	pngBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 13}
	rel := writeAttachmentFixture(t, ws, "2026-05-07", "deadbeef.png", pngBytes)

	srv := newAttachmentServer(t, ws)
	key := "dash:direct:alice:general"

	u := "/api/sessions/attachment?key=" + url.QueryEscape(key) +
		"&path=" + url.QueryEscape(rel)
	req := httptest.NewRequest(http.MethodGet, u, nil)
	w := httptest.NewRecorder()
	srv.sendH.handleAttachment(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (body=%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type=%q want image/png", ct)
	}
	if w.Header().Get("ETag") == "" {
		t.Error("missing ETag")
	}
	if got := w.Body.Bytes(); len(got) != len(pngBytes) {
		t.Errorf("body len=%d want %d", len(got), len(pngBytes))
	}
	// Security headers
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options=nosniff")
	}
	if w.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Error("missing CORP same-origin")
	}
	if w.Header().Get("Content-Security-Policy") == "" {
		t.Error("missing CSP")
	}
}

func TestHandleAttachment_MissingParams(t *testing.T) {
	ws := t.TempDir()
	srv := newAttachmentServer(t, ws)

	cases := []struct {
		name, url string
	}{
		{"no key", "/api/sessions/attachment?path=.naozhi/attachments/x.png"},
		{"no path", "/api/sessions/attachment?key=dash:direct:alice:general"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.url, nil)
			w := httptest.NewRecorder()
			srv.sendH.handleAttachment(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status=%d want 400", w.Code)
			}
		})
	}
}

// Path-traversal and escape attempts must collapse to 400/404 without
// ever touching the filesystem outside the attachment subtree.
func TestHandleAttachment_PathGuards(t *testing.T) {
	ws := t.TempDir()
	// Writing a secret outside the attachment dir — the endpoint must NOT
	// ever serve it regardless of clever path encoding.
	secret := filepath.Join(ws, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	srv := newAttachmentServer(t, ws)
	key := "dash:direct:alice:general"

	cases := []struct {
		name, path string
		wantCode   int
	}{
		{"absolute path", "/etc/passwd", http.StatusBadRequest},
		{"absolute under ws", secret, http.StatusBadRequest},
		{"parent traversal", "../../secret.txt", http.StatusBadRequest},
		{"escape via attachments prefix", ".naozhi/attachments/../secret.txt", http.StatusNotFound},
		{"not under attachment dir", "other/file.png", http.StatusNotFound},
		{"backslash path", ".naozhi\\attachments\\x.png", http.StatusBadRequest},
		{"nul byte", ".naozhi/attachments/x\x00.png", http.StatusBadRequest},
		{"missing file", ".naozhi/attachments/2026-05-07/missing.png", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := "/api/sessions/attachment?key=" + url.QueryEscape(key) +
				"&path=" + url.QueryEscape(c.path)
			req := httptest.NewRequest(http.MethodGet, u, nil)
			w := httptest.NewRecorder()
			srv.sendH.handleAttachment(w, req)
			if w.Code != c.wantCode {
				t.Errorf("status=%d want %d (body=%s)", w.Code, c.wantCode, w.Body.String())
			}
			// Defense-in-depth: secret bytes MUST never appear in body.
			if got := w.Body.String(); got == "top secret" {
				t.Errorf("secret bytes leaked for %s", c.name)
			}
		})
	}
}

func TestHandleAttachment_InvalidKey(t *testing.T) {
	ws := t.TempDir()
	rel := writeAttachmentFixture(t, ws, "2026-05-07", "a.png", []byte("x"))
	srv := newAttachmentServer(t, ws)

	// Control char in key → ValidateSessionKey rejects with 400.
	u := "/api/sessions/attachment?key=dash:direct:alice%0A:general&path=" + url.QueryEscape(rel)
	req := httptest.NewRequest(http.MethodGet, u, nil)
	w := httptest.NewRecorder()
	srv.sendH.handleAttachment(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

// Unknown chat key + no default workspace → 404. The router's
// GetWorkspace() would otherwise return "" and the path check collapses.
func TestHandleAttachment_NoWorkspace(t *testing.T) {
	ws := t.TempDir()
	rel := writeAttachmentFixture(t, ws, "2026-05-07", "a.png", []byte("x"))

	// Construct a server whose router has NO default workspace — the
	// fallback path that newAttachmentServer exercises is specifically
	// what we want to bypass here.
	router := session.NewRouter(session.RouterConfig{})
	srv := New(":0", router, nil, nil, nil, nil, "claude", ServerOptions{})
	srv.registerDashboard()

	u := "/api/sessions/attachment?key=dash:direct:bob:general&path=" + url.QueryEscape(rel)
	req := httptest.NewRequest(http.MethodGet, u, nil)
	w := httptest.NewRecorder()
	srv.sendH.handleAttachment(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404 (body=%s)", w.Code, w.Body.String())
	}
}

func TestHandleAttachment_NotModified(t *testing.T) {
	ws := t.TempDir()
	rel := writeAttachmentFixture(t, ws, "2026-05-07", "a.png", []byte{0x89, 'P', 'N', 'G'})
	srv := newAttachmentServer(t, ws)
	key := "dash:direct:alice:general"

	// First request to grab ETag.
	u := "/api/sessions/attachment?key=" + url.QueryEscape(key) + "&path=" + url.QueryEscape(rel)
	req := httptest.NewRequest(http.MethodGet, u, nil)
	w := httptest.NewRecorder()
	srv.sendH.handleAttachment(w, req)
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on first response")
	}

	// Second request with If-None-Match → 304.
	req2 := httptest.NewRequest(http.MethodGet, u, nil)
	req2.Header.Set("If-None-Match", etag)
	w2 := httptest.NewRecorder()
	srv.sendH.handleAttachment(w2, req2)
	if w2.Code != http.StatusNotModified {
		t.Errorf("status=%d want 304", w2.Code)
	}
	if w2.Body.Len() != 0 {
		t.Errorf("304 body should be empty, got %d bytes", w2.Body.Len())
	}
}
