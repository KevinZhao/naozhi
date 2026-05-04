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
	"time"

	"golang.org/x/time/rate"

	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
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

		// R175-SEC-LOW: C1 controls (U+0080..U+009F) — U+0085 (NEL) and
		// U+0090 (DCS) are representative samples. They pass the `r < 0x20`
		// gate and would otherwise leak into Content-Disposition via RFC
		// 5987 percent-encoding; some older HTTP intermediaries mis-parse
		// such bytes in the header value.
		{"name\u0085nel.txt", "namenel.txt"},
		{"name\u0090dcs.txt", "namedcs.txt"},
		// Bidi override (U+202E RLO) / isolate (U+2066 LRI / U+2069 PDI).
		// These let an attacker supply a filename that renders misleadingly
		// in a terminal / UI.
		{"photo\u202Ejpg.exe", "photojpg.exe"},
		{"name\u2066wrap\u2069.txt", "namewrap.txt"},
		// Line-separator (U+2028) / paragraph-separator (U+2029) — would
		// break the quoted filename onto a new line and let an attacker
		// inject a second header.
		{"doc\u2028sneaky.md", "docsneaky.md"},
		{"doc\u2029sneaky.md", "docsneaky.md"},
	}
	for _, tc := range cases {
		got := sanitizeDownloadName(tc.in)
		if got != tc.want {
			t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestContentDisposition verifies R71-SEC-M1: non-ASCII filenames must be
// emitted via the RFC 5987 filename* form so proxies / old clients that
// strictly reject 8-bit chars in quoted filenames still see a usable
// attachment name, while modern browsers render the UTF-8 original.
func TestContentDisposition(t *testing.T) {
	cases := []struct {
		name       string
		kind       string
		resolved   string
		wantPrefix string
		wantSubstr string
	}{
		{
			name:       "pure ASCII keeps simple quoted form",
			kind:       "inline",
			resolved:   "/tmp/src/foo.go",
			wantPrefix: `inline; filename="foo.go"`,
		},
		{
			name:       "non-ASCII emits both fallback and RFC5987 form",
			kind:       "attachment",
			resolved:   "/tmp/docs/说明.md",
			wantPrefix: `attachment; filename="______.md"`, // 2 CJK chars × 3 UTF-8 bytes = 6 underscores
			wantSubstr: "filename*=UTF-8''%E8%AF%B4%E6%98%8E.md",
		},
		{
			name:       "CR/LF and quotes stripped before encoding",
			kind:       "inline",
			resolved:   "/tmp/x\r\ny\".log",
			wantPrefix: `inline; filename="xy_.log"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := contentDisposition(tc.kind, tc.resolved)
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("contentDisposition = %q, want prefix %q", got, tc.wantPrefix)
			}
			if tc.wantSubstr != "" && !strings.Contains(got, tc.wantSubstr) {
				t.Errorf("contentDisposition = %q, want substring %q", got, tc.wantSubstr)
			}
		})
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

// TestHandleFilesExists_RateLimit pins the S13 per-IP limiter contract.
// The endpoint does up to maxExistsPaths (100) filesystem stats per request
// inside a fileStatTimeout budget, so unmetered access lets an authenticated
// caller tie up worker goroutines by pointing the batch at deep NFS mounts or
// symlink loops. A wire-level burst cap is the first (and cheapest) line of
// defense. We install a burst-1 limiter so two requests from the same IP
// deterministically drive the bucket empty without relying on wall-clock
// timing; the production policy (rate.Every(6s), burst 10) is smoke-tested
// by the TestProjectHandlers_FilesExistsLimiter_Wired contract below.
func TestHandleFilesExists_RateLimit(t *testing.T) {
	t.Parallel()
	h, proj, _ := newProjectHandlersForTest(t, map[string]string{"a.txt": "hi"})
	h.filesExistsLimiter = newIPLimiterWithProxy(rate.Every(time.Hour), 1, false)

	body, _ := json.Marshal(existsReq{Project: proj, Paths: []string{"a.txt"}})
	mkReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/projects/files/exists", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = "10.0.0.7:54321"
		return r
	}

	// First request consumes the burst slot and succeeds.
	w1 := httptest.NewRecorder()
	h.handleFilesExists(w1, mkReq())
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200 (body=%q)",
			w1.Code, strings.TrimSpace(w1.Body.String()))
	}

	// Second request from the same IP must hit the limiter before any
	// filesystem I/O starts; the 429 must carry the JSON error so curl /
	// dashboard clients can distinguish rate-limit from other 4xx classes.
	w2 := httptest.NewRecorder()
	h.handleFilesExists(w2, mkReq())
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429 (body=%q)",
			w2.Code, strings.TrimSpace(w2.Body.String()))
	}
	if ct := w2.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("429 Content-Type = %q, want application/json", ct)
	}
	if !strings.Contains(w2.Body.String(), "rate limit") {
		t.Errorf("429 body = %q, want JSON with 'rate limit' message", w2.Body.String())
	}

	// A request from a different IP bypasses the drained bucket because the
	// limiter keys on client IP. This also asserts that newProjectHandlersForTest
	// wiring hasn't accidentally promoted the limiter to a global bucket.
	r3 := mkReq()
	r3.RemoteAddr = "10.0.0.8:54321"
	w3 := httptest.NewRecorder()
	h.handleFilesExists(w3, r3)
	if w3.Code != http.StatusOK {
		t.Fatalf("third request (different IP): status = %d, want 200 (body=%q)",
			w3.Code, strings.TrimSpace(w3.Body.String()))
	}
}

// TestProjectHandlers_FilesExistsLimiter_Wired locks the S13 wiring: server.New
// must install a non-nil filesExistsLimiter on ProjectHandlers so the
// production path has DoS protection out of the box. Without this contract a
// refactor that drops the newIPLimiterWithProxy call in server.go would leave
// the handler technically correct but unprotected — regressions like that have
// shipped before (see R59-PERF-M3 where resolveProjectFile was called per
// path). We verify by inspecting the struct field on a minimally-configured
// Server, not by probing the rate-limiter timing (which would be flaky).
func TestProjectHandlers_FilesExistsLimiter_Wired(t *testing.T) {
	t.Parallel()
	router := session.NewRouter(session.RouterConfig{})
	srv := NewWithOptions(ServerOptions{
		Addr:   ":0",
		Router: router,
	})
	if srv == nil {
		t.Fatal("NewWithOptions returned nil")
	}
	if srv.projectH == nil {
		t.Fatal("projectH must be constructed even with nil ProjectManager")
	}
	if srv.projectH.filesExistsLimiter == nil {
		t.Error("server.New must wire ProjectHandlers.filesExistsLimiter (S13); " +
			"a nil limiter leaves /api/projects/files/exists unprotected against DoS")
	}
}

// TestHandleFilesExists_NilLimiterBypasses locks the optional-wiring contract.
// newProjectHandlersForTest builds a handler with no limiter; the nil-guard in
// handleFilesExists keeps those tests green without forcing every test to wire
// a limiter. A regression that flips the nil-guard to a fail-closed check
// would break newProjectHandlersForTest silently for every future test.
func TestHandleFilesExists_NilLimiterBypasses(t *testing.T) {
	t.Parallel()
	h, proj, _ := newProjectHandlersForTest(t, map[string]string{"a.txt": "hi"})
	if h.filesExistsLimiter != nil {
		t.Fatal("newProjectHandlersForTest must leave filesExistsLimiter nil")
	}

	body, _ := json.Marshal(existsReq{Project: proj, Paths: []string{"a.txt"}})
	// 5 back-to-back requests must all succeed — no limiter => no 429.
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/projects/files/exists", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "10.0.0.9:54321"
		w := httptest.NewRecorder()
		h.handleFilesExists(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("req %d: status = %d, want 200 (body=%q)",
				i, w.Code, strings.TrimSpace(w.Body.String()))
		}
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

// TestHandleFileGet_PreviewRejectsHTML covers R176-SEC-H3: an .html file
// in the workspace previously flowed through the preview JSON content
// path (`isTextMime("text/html") == true`), and `writeJSON` has
// `SetEscapeHTML(false)`. The dashboard renderer uses `esc()` today but
// the JS-side defense is one regression away from stored XSS. The
// server-side contract must refuse text/html for preview — clients
// switch to download mode — mirroring serveRaw's explicit rejection.
func TestHandleFileGet_PreviewRejectsHTML(t *testing.T) {
	h, proj, projDir := newProjectHandlersForTest(t, nil)
	// CLI tool writes an arbitrary .html file into the workspace.
	htmlBytes := []byte(`<!doctype html><html><body><script>alert(1)</script></body></html>`)
	if err := os.WriteFile(filepath.Join(projDir, "report.html"), htmlBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+proj+"&path=report.html&mode=preview", nil)
	w := httptest.NewRecorder()
	h.handleFileGet(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with binary=true payload", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}
	if resp["binary"] != true {
		t.Errorf("binary = %v, want true (text/html must NOT flow through preview content)", resp["binary"])
	}
	if resp["content"] != "" {
		t.Errorf("content = %q, want empty string (html bytes leaked into JSON)", resp["content"])
	}
	// Defense-in-depth: the <script> substring must NOT appear anywhere
	// in the JSON body. If a future refactor loses the guard, this
	// catches the regression even if `binary` is also set correctly.
	if strings.Contains(w.Body.String(), "<script>") {
		t.Errorf("response body contains <script>; text/html leaked through preview")
	}
}

// TestHandleFileGet_PreviewRejectsXHTMLXMLVariants covers R179-SEC-2: an
// .xml file with XHTML content would be served inline (both the preview
// JSON content path and serveRaw), and browsers parse XHTML with full
// DOM+script support — achieving same-origin script execution on a user
// that clicks the raw-preview link. Guard mirrors the text/html case.
func TestHandleFileGet_PreviewRejectsXHTMLXMLVariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		path    string
		content string
	}{
		{
			name:    "xhtml_namespace",
			path:    "evil.xml",
			content: `<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml"><body><script>alert(1)</script></body></html>`,
		},
		{
			name:    "plain_xml_with_script_entity",
			path:    "a.xml",
			content: `<?xml version="1.0"?><root><data>x</data></root>`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, proj, projDir := newProjectHandlersForTest(t, nil)
			if err := os.WriteFile(filepath.Join(projDir, tc.path), []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			// Preview path: response must be binary:true with empty content.
			req := httptest.NewRequest(http.MethodGet,
				"/api/projects/file?project="+proj+"&path="+tc.path+"&mode=preview", nil)
			w := httptest.NewRecorder()
			h.handleFileGet(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("preview status = %d, want 200 with binary=true payload", w.Code)
			}
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if resp["binary"] != true {
				t.Errorf("binary = %v, want true (XML must NOT flow through preview content)", resp["binary"])
			}
			if resp["content"] != "" {
				t.Errorf("content = %q, want empty string", resp["content"])
			}
			// Raw path: must be refused with 415.
			rawReq := httptest.NewRequest(http.MethodGet,
				"/api/projects/file?project="+proj+"&path="+tc.path+"&mode=raw", nil)
			rawW := httptest.NewRecorder()
			h.handleFileGet(rawW, rawReq)
			if rawW.Code != http.StatusUnsupportedMediaType {
				t.Errorf("raw status = %d, want 415 (XML must not be served inline)", rawW.Code)
			}
		})
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
