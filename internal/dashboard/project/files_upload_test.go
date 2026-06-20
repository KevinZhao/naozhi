package project

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildUpload constructs a multipart upload request body. filename is the
// part's Content-Disposition filename (attacker-controlled in production).
func buildUpload(t *testing.T, project, dir, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if project != "" {
		_ = mw.WriteField("project", project)
	}
	if dir != "" {
		_ = mw.WriteField("dir", dir)
	}
	if filename != "\x00omit" { // sentinel to skip the file part entirely
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, mw.FormDataContentType()
}

func doUpload(t *testing.T, h *Handlers, query, project, dir, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := buildUpload(t, project, dir, filename, content)
	url := "/api/projects/files/upload"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	h.HandleFilesUpload(w, req)
	return w
}

func TestHandleFilesUpload_Success(t *testing.T) {
	h, proj, projDir := newProjectHandlersForTest(t, map[string]string{"sub/keep.txt": "x"})

	w := doUpload(t, h, "", proj, "sub", "hello.txt", []byte("world"))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		OK   bool   `json:"ok"`
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Path != "sub/hello.txt" || resp.Size != 5 {
		t.Errorf("unexpected resp %+v", resp)
	}
	got, err := os.ReadFile(filepath.Join(projDir, "sub", "hello.txt"))
	if err != nil || string(got) != "world" {
		t.Errorf("file content = %q err=%v", got, err)
	}
	// Perm must be 0o600.
	fi, _ := os.Stat(filepath.Join(projDir, "sub", "hello.txt"))
	if fi != nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 600", fi.Mode().Perm())
	}
}

func TestHandleFilesUpload_EmptyFile(t *testing.T) {
	h, proj, projDir := newProjectHandlersForTest(t, nil)
	w := doUpload(t, h, "", proj, "", "empty.txt", []byte(""))
	if w.Code != http.StatusOK {
		t.Fatalf("empty upload: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	fi, err := os.Stat(filepath.Join(projDir, "empty.txt"))
	if err != nil || fi.Size() != 0 {
		t.Errorf("empty file not written as 0 bytes: size=%v err=%v", fi, err)
	}
}

func TestHandleFilesUpload_UnicodeRoundTrip(t *testing.T) {
	h, proj, projDir := newProjectHandlersForTest(t, nil)
	name := "中文笔记.md"
	w := doUpload(t, h, "", proj, "", name, []byte("# 标题"))
	if w.Code != http.StatusOK {
		t.Fatalf("unicode upload: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(projDir, name))
	if err != nil || string(got) != "# 标题" {
		t.Errorf("unicode file round-trip failed: %q err=%v", got, err)
	}
}

func TestCreateWorkspaceFile_ExclAndPerm(t *testing.T) {
	// Direct unit test so both build variants (unix/windows) get a non-skipped
	// assertion on the O_EXCL + perm contract.
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	f, err := CreateWorkspaceFile(p, false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = f.Close()
	if fi, _ := os.Stat(p); fi != nil && runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 600", fi.Mode().Perm())
	}
	// Second create without overwrite must fail O_EXCL → ErrExist.
	if _, err := CreateWorkspaceFile(p, false); !os.IsExist(err) {
		t.Errorf("create existing without overwrite: want ErrExist, got %v", err)
	}
	// Overwrite truncates in place.
	f2, err := CreateWorkspaceFile(p, true)
	if err != nil {
		t.Fatalf("overwrite create: %v", err)
	}
	_ = f2.Close()
}

func TestHandleFilesUpload_RootDir(t *testing.T) {
	h, proj, projDir := newProjectHandlersForTest(t, nil)
	w := doUpload(t, h, "", proj, "", "top.txt", []byte("hi"))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(projDir, "top.txt")); err != nil {
		t.Errorf("root upload not written: %v", err)
	}
}

func TestHandleFilesUpload_NoOverwriteByDefault(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, map[string]string{"exists.txt": "old"})
	w := doUpload(t, h, "", proj, "", "exists.txt", []byte("new"))
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", w.Code)
	}
}

func TestHandleFilesUpload_OverwriteOptIn(t *testing.T) {
	h, proj, projDir := newProjectHandlersForTest(t, map[string]string{"exists.txt": "old"})
	w := doUpload(t, h, "overwrite=1", proj, "", "exists.txt", []byte("new!"))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}
	got, _ := os.ReadFile(filepath.Join(projDir, "exists.txt"))
	if string(got) != "new!" {
		t.Errorf("overwrite content = %q", got)
	}
}

func TestHandleFilesUpload_FilenameTraversalNeverEscapes(t *testing.T) {
	// Go's mime/multipart normalises a part filename via filepath.Base before
	// the handler sees it, so a traversal-laden filename can never carry a path
	// component to disk. This test pins that contract end-to-end: a hostile
	// filename either lands as a plain base name INSIDE the workspace or is
	// rejected — but never escapes. (validUploadLeaf is unit-tested directly in
	// TestValidUploadLeaf for the raw-string rejection it adds as defence in
	// depth.)
	h, proj, projDir := newProjectHandlersForTest(t, nil)
	parent := filepath.Dir(projDir)
	for _, fn := range []string{"../escape.txt", "../../sneaky.txt", "sub/../../evil.txt"} {
		t.Run(fn, func(t *testing.T) {
			doUpload(t, h, "", proj, "", fn, []byte("x"))
			// Whatever the status, nothing may appear outside the workspace.
			base := filepath.Base(fn)
			if _, err := os.Stat(filepath.Join(parent, base)); err == nil {
				t.Errorf("filename %q escaped the workspace as %q", fn, base)
			}
		})
	}
}

func TestHandleFilesUpload_DirTraversal(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, nil)
	for _, dir := range []string{"../", "../../etc", "/etc", "a/../../x"} {
		t.Run(dir, func(t *testing.T) {
			w := doUpload(t, h, "", proj, dir, "f.txt", []byte("x"))
			if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
				t.Errorf("dir %q: want 400/404, got %d", dir, w.Code)
			}
		})
	}
}

func TestHandleFilesUpload_SensitiveNamesBlocked(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, nil)
	cases := []struct{ dir, name string }{
		{"", ".env"},
		{"", "id_rsa"},
		{"", "deploy.pem"},
		{"", ".bashrc"},
		{"", ".profile"},
		{"", ".gitconfig"},
		{"", "authorized_keys"},
		{".ssh", "config"},
		{"secrets", "db.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.dir+"/"+tc.name, func(t *testing.T) {
			// The .ssh / secrets dirs don't exist; the name/segment deny runs
			// BEFORE the dir-resolve, so we expect 403 regardless.
			w := doUpload(t, h, "", proj, tc.dir, tc.name, []byte("x"))
			if w.Code != http.StatusForbidden {
				t.Errorf("%s/%s: want 403, got %d", tc.dir, tc.name, w.Code)
			}
		})
	}
}

func TestHandleFilesUpload_GitSubtreeBlocked(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, map[string]string{".git/keep": "x"})
	w := doUpload(t, h, "", proj, ".git/hooks", "post-checkout", []byte("#!/bin/sh"))
	if w.Code != http.StatusForbidden {
		t.Errorf(".git subtree write: want 403, got %d", w.Code)
	}
}

func TestHandleFilesUpload_SymlinkedParentIntoControlDirRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	// A workspace symlink `docs -> .git` must not let an upload to dir=docs
	// write into the .git control subtree (which the logical-path deny-list
	// would miss). The resolved-path re-check must catch it.
	h, proj, projDir := newProjectHandlersForTest(t, map[string]string{".git/keep": "x"})
	if err := os.Symlink(filepath.Join(projDir, ".git"), filepath.Join(projDir, "docs")); err != nil {
		t.Fatal(err)
	}
	w := doUpload(t, h, "", proj, "docs", "post-checkout", []byte("#!/bin/sh\nevil"))
	if w.Code != http.StatusForbidden {
		t.Errorf("symlinked-parent into .git: want 403, got %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(projDir, ".git", "post-checkout")); err == nil {
		t.Errorf("write slipped into .git via symlinked parent")
	}
}

func TestHandleFilesUpload_SymlinkedLeafRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("O_NOFOLLOW semantics differ on windows")
	}
	h, proj, projDir := newProjectHandlersForTest(t, nil)
	outside := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(outside, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-place a symlink leaf inside the workspace pointing outside.
	link := filepath.Join(projDir, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	// overwrite=1 must STILL refuse to follow the symlink (O_NOFOLLOW kept).
	w := doUpload(t, h, "overwrite=1", proj, "", "link.txt", []byte("evil"))
	if w.Code != http.StatusConflict {
		t.Errorf("symlinked leaf overwrite: want 409, got %d", w.Code)
	}
	got, _ := os.ReadFile(outside)
	if string(got) != "original" {
		t.Errorf("symlink target was clobbered: %q", got)
	}
}

func TestHandleFilesUpload_SymlinkedParentRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	h, proj, projDir := newProjectHandlersForTest(t, nil)
	outside := t.TempDir()
	link := filepath.Join(projDir, "outdir")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	w := doUpload(t, h, "", proj, "outdir", "f.txt", []byte("x"))
	if w.Code != http.StatusNotFound {
		t.Errorf("symlinked parent dir: want 404, got %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(outside, "f.txt")); err == nil {
		t.Errorf("write escaped through symlinked parent")
	}
}

func TestHandleFilesUpload_MissingDir(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, nil)
	w := doUpload(t, h, "", proj, "nonexistent", "f.txt", []byte("x"))
	if w.Code != http.StatusNotFound {
		t.Errorf("missing dir: want 404, got %d", w.Code)
	}
}

func TestHandleFilesUpload_RemoteNodeRejected(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, nil)
	w := doUpload(t, h, "node=worker-1", proj, "", "f.txt", []byte("x"))
	if w.Code != http.StatusBadRequest {
		t.Errorf("remote node: want 400, got %d", w.Code)
	}
}

func TestHandleFilesUpload_NoFilePart(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, nil)
	w := doUpload(t, h, "", proj, "", "\x00omit", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("no file part: want 400, got %d", w.Code)
	}
}

func TestHandleFilesUpload_UnknownProject(t *testing.T) {
	h, _, _ := newProjectHandlersForTest(t, nil)
	w := doUpload(t, h, "", "nonesuch", "", "f.txt", []byte("x"))
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown project: want 404, got %d", w.Code)
	}
}

func TestHandleFilesUpload_PublicTmpRejected(t *testing.T) {
	h, _, _ := newProjectHandlersForTest(t, nil)
	h.publicTmpEnabled = true
	w := doUpload(t, h, "", "__public_tmp__", "", "f.txt", []byte("x"))
	if w.Code != http.StatusForbidden {
		t.Errorf("public_tmp upload: want 403, got %d", w.Code)
	}
}

func TestValidUploadLeaf(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"hello.txt", "hello.txt", true},
		{"my file.go", "my file.go", true},
		{"中文.md", "中文.md", true},
		{"../x", "", false},
		{"a/b", "", false},
		{"a\\b", "", false},
		{".", "", false},
		{"..", "", false},
		{"", "", false},
		{strings.Repeat("a", 255), strings.Repeat("a", 255), true},
		{strings.Repeat("a", 256), "", false},
	}
	for _, tc := range cases {
		got, ok := validUploadLeaf(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("validUploadLeaf(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
