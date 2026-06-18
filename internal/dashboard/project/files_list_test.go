package project

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// listResp mirrors the JSON shape HandleFilesList writes.
type listResp struct {
	Dir       string      `json:"dir"`
	Entries   []listEntry `json:"entries"`
	Truncated bool        `json:"truncated"`
}

func doList(t *testing.T, h *Handlers, query string) (*httptest.ResponseRecorder, listResp) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/projects/files/list?"+query, nil)
	w := httptest.NewRecorder()
	h.HandleFilesList(w, req)
	var resp listResp
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode list resp: %v (body=%s)", err, w.Body.String())
		}
	}
	return w, resp
}

func entryNames(entries []listEntry) map[string]listEntry {
	m := make(map[string]listEntry, len(entries))
	for _, e := range entries {
		m[e.Name] = e
	}
	return m
}

func TestHandleFilesList_RootAndSubdir(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, map[string]string{
		"README.md":      "hi",
		"src/main.go":    "package main",
		"src/util/x.go":  "package util",
		"docs/guide.txt": "doc",
	})

	// Root listing (dir empty) — must NOT go through resolveProjectFileWithRoot
	// (which rejects "" / "."). CLAUDE.md is created by the helper too.
	w, resp := doList(t, h, "project="+proj)
	if w.Code != http.StatusOK {
		t.Fatalf("root list: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if resp.Dir != "." {
		t.Errorf("root dir = %q, want %q", resp.Dir, ".")
	}
	names := entryNames(resp.Entries)
	for _, want := range []string{"README.md", "src", "docs", "CLAUDE.md"} {
		if _, ok := names[want]; !ok {
			t.Errorf("root listing missing %q (got %v)", want, names)
		}
	}
	// Dirs sort first.
	if len(resp.Entries) > 0 && !resp.Entries[0].IsDir {
		t.Errorf("expected a directory first, got %+v", resp.Entries[0])
	}
	if d := names["src"]; !d.IsDir {
		t.Errorf("src should be is_dir")
	}

	// Subdir listing.
	w, resp = doList(t, h, "project="+proj+"&dir=src")
	if w.Code != http.StatusOK {
		t.Fatalf("subdir list: want 200, got %d", w.Code)
	}
	if resp.Dir != "src" {
		t.Errorf("subdir dir = %q, want src", resp.Dir)
	}
	names = entryNames(resp.Entries)
	if _, ok := names["main.go"]; !ok {
		t.Errorf("src listing missing main.go")
	}
	if d, ok := names["util"]; !ok || !d.IsDir {
		t.Errorf("src listing missing util dir")
	}
}

func TestHandleFilesList_CredentialFilesOmitted(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, map[string]string{
		"app.go":     "package main",
		".env":       "SECRET=1",
		"id_rsa":     "-----BEGIN-----",
		"deploy.pem": "key",
		"notes.txt":  "ok",
	})
	w, resp := doList(t, h, "project="+proj)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	names := entryNames(resp.Entries)
	for _, banned := range []string{".env", "id_rsa", "deploy.pem"} {
		if _, ok := names[banned]; ok {
			t.Errorf("credential file %q must not be enumerated", banned)
		}
	}
	for _, want := range []string{"app.go", "notes.txt"} {
		if _, ok := names[want]; !ok {
			t.Errorf("ordinary file %q should be listed", want)
		}
	}
}

func TestHandleFilesList_SensitiveDirOmitted(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, map[string]string{
		"ok.txt":               "hi",
		".ssh/authorized_keys": "ssh-rsa",
		"secrets/db.yaml":      "pw",
	})
	// Root: .ssh and secrets dirs are sensitive segments → omitted.
	_, resp := doList(t, h, "project="+proj)
	names := entryNames(resp.Entries)
	if _, ok := names[".ssh"]; ok {
		t.Errorf(".ssh dir must be omitted from listing")
	}
	if _, ok := names["secrets"]; ok {
		t.Errorf("secrets dir must be omitted from listing")
	}
}

func TestHandleFilesList_Traversal(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, nil)
	cases := []string{
		"../",
		"../../etc",
		"/etc",
		"a/../../x",
	}
	for _, dir := range cases {
		t.Run(dir, func(t *testing.T) {
			w, _ := doList(t, h, "project="+proj+"&dir="+dir)
			if w.Code != http.StatusNotFound && w.Code != http.StatusBadRequest {
				t.Errorf("dir=%q: want 404/400, got %d", dir, w.Code)
			}
		})
	}
}

func TestHandleFilesList_SymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	h, proj, projDir := newProjectHandlersForTest(t, map[string]string{"keep.txt": "x"})
	// A symlink whose target is outside the workspace must not be navigable,
	// and listing a directory THROUGH it must 404.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "loot.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(projDir, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	// Root listing: the symlink is surfaced but flagged, never followed.
	// Assert it is PRESENT first — an `if ok` guard would silently pass if a
	// regression dropped symlink children, turning the security assertion into
	// a no-op.
	_, resp := doList(t, h, "project="+proj)
	names := entryNames(resp.Entries)
	e, ok := names["escape"]
	if !ok {
		t.Fatalf("escape symlink missing from listing (got %v)", names)
	}
	if !e.Symlink {
		t.Errorf("symlink child must carry symlink:true")
	}
	if e.IsDir {
		t.Errorf("symlink child must not be reported as is_dir")
	}

	// Listing THROUGH the symlink must be refused (O_NOFOLLOW / prefix check).
	w, _ := doList(t, h, "project="+proj+"&dir=escape")
	if w.Code != http.StatusNotFound {
		t.Errorf("listing through symlink: want 404, got %d", w.Code)
	}
}

func TestHandleFilesList_FileNotDir(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, map[string]string{"a.txt": "hi"})
	w, _ := doList(t, h, "project="+proj+"&dir=a.txt")
	if w.Code != http.StatusNotFound {
		t.Errorf("listing a file as dir: want 404, got %d", w.Code)
	}
}

func TestHandleFilesList_RemoteNodeRejected(t *testing.T) {
	h, proj, _ := newProjectHandlersForTest(t, nil)
	w, _ := doList(t, h, "project="+proj+"&node=worker-1")
	if w.Code != http.StatusBadRequest {
		t.Errorf("remote node: want 400, got %d", w.Code)
	}
}

func TestHandleFilesList_UnknownProject(t *testing.T) {
	h, _, _ := newProjectHandlersForTest(t, nil)
	w, _ := doList(t, h, "project=nonesuch")
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown project: want 404, got %d", w.Code)
	}
}

func TestHandleFilesList_InvalidProjectName(t *testing.T) {
	h, _, _ := newProjectHandlersForTest(t, nil)
	// A control character is what validateProjectName rejects (a non-existent
	// but well-formed name like "../evil" is a legitimate 404, not a 400).
	w, _ := doList(t, h, "project=%07bad")
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid project name: want 400, got %d", w.Code)
	}
}

func TestHandleFilesList_PublicTmpRejected(t *testing.T) {
	h, _, _ := newProjectHandlersForTest(t, nil)
	h.publicTmpEnabled = true
	w, _ := doList(t, h, "project=__public_tmp__")
	if w.Code != http.StatusBadRequest {
		t.Errorf("public_tmp list: want 400, got %d", w.Code)
	}
}

func TestHandleFilesList_Truncation(t *testing.T) {
	files := make(map[string]string, maxListEntries+50)
	for i := 0; i < maxListEntries+10; i++ {
		files["f"+itoa(i)+".txt"] = "x"
	}
	h, proj, _ := newProjectHandlersForTest(t, files)
	_, resp := doList(t, h, "project="+proj)
	if !resp.Truncated {
		t.Errorf("expected truncated:true for %d entries", len(files))
	}
	if len(resp.Entries) > maxListEntries {
		t.Errorf("entries = %d, must be capped at %d", len(resp.Entries), maxListEntries)
	}
}

func TestHandleFilesList_IrregularTypeSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fifo not supported on windows")
	}
	h, proj, projDir := newProjectHandlersForTest(t, map[string]string{"real.txt": "x"})
	fifo := filepath.Join(projDir, "pipe")
	if err := mkfifoForTest(fifo); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	_, resp := doList(t, h, "project="+proj)
	names := entryNames(resp.Entries)
	if _, ok := names["pipe"]; ok {
		t.Errorf("fifo must be skipped from listing")
	}
	if _, ok := names["real.txt"]; !ok {
		t.Errorf("regular file should remain")
	}
}

// itoa avoids importing strconv just for the truncation test loop.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
