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

	"github.com/naozhi/naozhi/internal/project"
)

// newProjectHandlersForTest builds a ProjectHandlers pointed at a temp
// workspace with CLAUDE.md + optional extra files.  Returns (handlers,
// project name, root dir) so tests can construct request URLs.
func newProjectHandlersForTest(t *testing.T, files map[string]string) (*ProjectHandlers, string, string) {
	t.Helper()
	root := t.TempDir()
	projDir := filepath.Join(root, "demo")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "CLAUDE.md"), []byte("# demo"), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		full := filepath.Join(projDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mgr, err := project.NewManager(root, project.PlannerDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Scan(); err != nil {
		t.Fatal(err)
	}
	return &ProjectHandlers{projectMgr: mgr}, "demo", projDir
}

// ─── resolveProjectFile ───────────────────────────────────────────────────────

func TestResolveProjectFile_Traversal(t *testing.T) {
	_, _, projDir := newProjectHandlersForTest(t, nil)

	cases := []struct {
		name string
		rel  string
	}{
		{"dotdot literal", "../etc/passwd"},
		{"deep traversal", "a/../../x"},
		{"abs path", "/etc/passwd"},
		{"null byte", "foo\x00.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveProjectFile(projDir, tc.rel); err == nil {
				t.Errorf("resolveProjectFile(%q) should have errored", tc.rel)
			}
		})
	}
}

func TestResolveProjectFile_Valid(t *testing.T) {
	_, _, projDir := newProjectHandlersForTest(t, map[string]string{
		"src/foo.go": "package foo\n",
	})
	got, err := resolveProjectFile(projDir, "src/foo.go")
	if err != nil {
		t.Fatalf("resolveProjectFile: %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join("src", "foo.go")) {
		t.Errorf("resolved path = %q, want suffix src/foo.go", got)
	}
}

func TestResolveProjectFile_EmptyOrTooLong(t *testing.T) {
	_, _, projDir := newProjectHandlersForTest(t, nil)
	if _, err := resolveProjectFile(projDir, ""); err == nil {
		t.Error("empty rel should error")
	}
	big := strings.Repeat("a", maxExistsPathLen+1)
	if _, err := resolveProjectFile(projDir, big); err == nil {
		t.Error("overlong rel should error")
	}
}

// TestResolveProjectFile_EmptyProjectRejected covers R61-GO-1: on Linux,
// filepath.EvalSymlinks("") returns (".", nil), so the old order (EvalSymlinks
// first, then empty-check in the err branch) would silently fall back to the
// process CWD. The fix checks empty before EvalSymlinks. Without it, a
// misconfigured caller could expose files relative to the naozhi CWD.
func TestResolveProjectFile_EmptyProjectRejected(t *testing.T) {
	if _, err := resolveProjectFile("", "README.md"); err == nil {
		t.Fatal("empty projectPath must error, not fall back to CWD")
	}
}

// ─── detectMime / isTextMime ──────────────────────────────────────────────────

func TestDetectMime_SourceCodeExtensions(t *testing.T) {
	cases := []struct {
		path string
		head string
		want string
	}{
		{"src/foo.go", "package foo\n", "text/x-go"},
		{"a.py", "print('x')", "text/x-python"},
		{"a.json", `{"a":1}`, "application/json"},
		{"Dockerfile", "FROM debian", "text/plain"}, // http default
		{"a.txt", "hello", "text/plain"},
	}
	for _, tc := range cases {
		got := detectMime(tc.path, []byte(tc.head))
		// DetectContentType appends charset to text/plain, strip it for compare.
		got = strings.SplitN(got, ";", 2)[0]
		got = strings.TrimSpace(got)
		if got != tc.want {
			t.Errorf("detectMime(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestIsTextMime(t *testing.T) {
	if !isTextMime("text/plain; charset=utf-8") {
		t.Error("text/plain should be text")
	}
	if !isTextMime("application/json") {
		t.Error("application/json should be text")
	}
	if isTextMime("application/octet-stream") {
		t.Error("octet-stream should not be text")
	}
	if isTextMime("image/png") {
		t.Error("image/png should not be text")
	}
}

func TestIsRawPreviewMime(t *testing.T) {
	if !isRawPreviewMime("image/png") {
		t.Error("image/png should be raw-previewable")
	}
	if !isRawPreviewMime("application/pdf") {
		t.Error("pdf should be raw-previewable")
	}
	if isRawPreviewMime("application/x-msdownload") {
		t.Error(".exe should not be raw-previewable")
	}
}

func TestSanitizeDownloadName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"src/foo.go", "foo.go"},
		{"a/b/c.txt", "c.txt"},
		{"bad\r\nname.txt", "badname.txt"},
		{`x"y.go`, "x_y.go"},
		{"", "download"},
	}
	for _, tc := range cases {
		got := sanitizeDownloadName(tc.in)
		if got != tc.want {
			t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ─── handleFilesExists ────────────────────────────────────────────────────────

func TestHandleFilesExists_Batch(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, map[string]string{
		"src/foo.go":   "package foo\n",
		"docs/TODO.md": "# todo",
	})

	body, _ := json.Marshal(existsReq{
		Project: proj,
		Paths: []string{
			"src/foo.go",
			"docs/TODO.md",
			"missing/x.go",
			"../etc/passwd",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/projects/files/exists", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleFilesExists(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Results map[string]existsEntry `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Results["src/foo.go"].Exists {
		t.Error("src/foo.go should exist")
	}
	if resp.Results["src/foo.go"].Size == 0 {
		t.Error("src/foo.go size should be nonzero")
	}
	if resp.Results["src/foo.go"].Mime == "" {
		t.Error("src/foo.go mime should be set")
	}
	if !resp.Results["docs/TODO.md"].Exists {
		t.Error("docs/TODO.md should exist")
	}
	if resp.Results["missing/x.go"].Exists {
		t.Error("missing path should NOT exist")
	}
	if resp.Results["../etc/passwd"].Exists {
		t.Error("traversal should NOT be reported as existing")
	}
}

func TestHandleFilesExists_UnknownProject(t *testing.T) {
	h, _, _ := newProjectHandlersForTest(t, nil)
	body := `{"project":"nosuch","paths":["a"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/projects/files/exists", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleFilesExists(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleFilesExists_TooManyPaths(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, nil)
	paths := make([]string, maxExistsPaths+1)
	for i := range paths {
		paths[i] = "x"
	}
	body, _ := json.Marshal(existsReq{Project: proj, Paths: paths})
	req := httptest.NewRequest(http.MethodPost, "/api/projects/files/exists", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.handleFilesExists(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleFilesExists_InvalidJSON(t *testing.T) {
	h, _, _ := newProjectHandlersForTest(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/projects/files/exists", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	h.handleFilesExists(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// ─── handleFileGet: preview ───────────────────────────────────────────────────

func TestHandleFileGet_PreviewText(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, map[string]string{
		"src/foo.go": "package foo\n",
	})
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+proj+"&path=src/foo.go&mode=preview", nil)
	w := httptest.NewRecorder()
	h.handleFileGet(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["content"] != "package foo\n" {
		t.Errorf("content = %v", resp["content"])
	}
	if resp["truncated"] != false {
		t.Error("should not be truncated")
	}
	if resp["binary"] != false {
		t.Error("should not be binary")
	}
	mime, _ := resp["mime"].(string)
	if !strings.HasPrefix(mime, "text/") {
		t.Errorf("mime = %q, want text/*", mime)
	}
}

func TestHandleFileGet_PreviewBinary(t *testing.T) {
	// A PNG magic header triggers http.DetectContentType to return image/png.
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d}
	h, proj, projDir := newProjectHandlersForTest(t, nil)
	if err := os.WriteFile(filepath.Join(projDir, "logo.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+proj+"&path=logo.png&mode=preview", nil)
	w := httptest.NewRecorder()
	h.handleFileGet(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp["binary"] != true {
		t.Errorf("binary = %v, want true", resp["binary"])
	}
	if resp["content"] != "" {
		t.Errorf("content should be empty for binary")
	}
}

func TestHandleFileGet_PreviewTruncation(t *testing.T) {
	big := strings.Repeat("x", maxPreviewBytes+1024)
	h, proj, _ := newProjectHandlersForTest(t, map[string]string{
		"big.txt": big,
	})
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+proj+"&path=big.txt&mode=preview", nil)
	w := httptest.NewRecorder()
	h.handleFileGet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	if resp["truncated"] != true {
		t.Error("should be truncated")
	}
	content, _ := resp["content"].(string)
	if len(content) != maxPreviewBytes {
		t.Errorf("len(content) = %d, want %d", len(content), maxPreviewBytes)
	}
}

func TestHandleFileGet_PreviewInvalidUTF8(t *testing.T) {
	h, proj, projDir := newProjectHandlersForTest(t, nil)
	// Start with a text extension so detectMime classifies as text, but with
	// invalid UTF-8 bytes (0xff 0xfe sequence typical of UTF-16 BOM).
	if err := os.WriteFile(filepath.Join(projDir, "bad.txt"),
		[]byte("hello\xff\xfeworld"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+proj+"&path=bad.txt&mode=preview", nil)
	w := httptest.NewRecorder()
	h.handleFileGet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp) //nolint:errcheck
	content, _ := resp["content"].(string)
	if !strings.Contains(content, "\uFFFD") {
		t.Errorf("invalid UTF-8 should be replaced with U+FFFD, got %q", content)
	}
}

// ─── handleFileGet: raw ───────────────────────────────────────────────────────

func TestHandleFileGet_RawImage(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d}
	h, proj, projDir := newProjectHandlersForTest(t, nil)
	if err := os.WriteFile(filepath.Join(projDir, "logo.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+proj+"&path=logo.png&mode=raw", nil)
	w := httptest.NewRecorder()
	h.handleFileGet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
		t.Errorf("Content-Disposition = %q, want inline", cd)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("raw response should have CSP sandbox, got %q", csp)
	}
	if !bytes.Equal(w.Body.Bytes(), png) {
		t.Error("body should match input bytes")
	}
}

func TestHandleFileGet_RawRejectsBinary(t *testing.T) {
	h, proj, projDir := newProjectHandlersForTest(t, nil)
	// Random binary — not an image, not text.
	if err := os.WriteFile(filepath.Join(projDir, "blob.bin"),
		[]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05}, 0o644); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+proj+"&path=blob.bin&mode=raw", nil)
	w := httptest.NewRecorder()
	h.handleFileGet(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", w.Code)
	}
}

// ─── handleFileGet: download ──────────────────────────────────────────────────

func TestHandleFileGet_Download(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, map[string]string{
		"weird file.txt": "contents here",
	})
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+proj+"&path=weird%20file.txt&mode=download", nil)
	w := httptest.NewRecorder()
	h.handleFileGet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "weird file.txt") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if w.Body.String() != "contents here" {
		t.Errorf("body = %q", w.Body.String())
	}
}

// ─── handleFileGet: ETag / 304 ────────────────────────────────────────────────

func TestHandleFileGet_ETag304(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, map[string]string{
		"a.txt": "hello",
	})

	// First request: collect ETag.
	req1 := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+proj+"&path=a.txt&mode=preview", nil)
	w1 := httptest.NewRecorder()
	h.handleFileGet(w1, req1)
	etag := w1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("first response missing ETag")
	}

	// Second request with matching If-None-Match → 304.
	req2 := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+proj+"&path=a.txt&mode=preview", nil)
	req2.Header.Set("If-None-Match", etag)
	w2 := httptest.NewRecorder()
	h.handleFileGet(w2, req2)
	if w2.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", w2.Code)
	}
	if w2.Body.Len() != 0 {
		t.Error("304 response should have empty body")
	}
}

// ─── handleFileGet: error paths ───────────────────────────────────────────────

func TestHandleFileGet_Missing(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, nil)
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+proj+"&path=nope.txt&mode=preview", nil)
	w := httptest.NewRecorder()
	h.handleFileGet(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleFileGet_Traversal(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, nil)
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+proj+"&path=../../etc/passwd&mode=preview", nil)
	w := httptest.NewRecorder()
	h.handleFileGet(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleFileGet_InvalidMode(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, map[string]string{"a.txt": "hi"})
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+proj+"&path=a.txt&mode=bogus", nil)
	w := httptest.NewRecorder()
	h.handleFileGet(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleFileGet_Directory(t *testing.T) {
	h, proj, projDir := newProjectHandlersForTest(t, nil)
	if err := os.MkdirAll(filepath.Join(projDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+proj+"&path=sub&mode=preview", nil)
	w := httptest.NewRecorder()
	h.handleFileGet(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for directory", w.Code)
	}
}
