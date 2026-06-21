package project

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

// doDirList issues GET /api/projects/dir and returns the recorder.
func doDirList(t *testing.T, h *Handlers, project, path string) *httptest.ResponseRecorder {
	t.Helper()
	u := "/api/projects/dir?project=" + url.QueryEscape(project)
	if path != "" {
		u += "&path=" + url.QueryEscape(path)
	}
	req := httptest.NewRequest(http.MethodGet, u, nil)
	rec := httptest.NewRecorder()
	h.HandleDirList(rec, req)
	return rec
}

func decodeDirList(t *testing.T, rec *httptest.ResponseRecorder) dirListResp {
	t.Helper()
	var resp dirListResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode dir list: %v (body=%s)", err, rec.Body.String())
	}
	return resp
}

// TestHandleDirList_Traversal pins that hostile paths are rejected, never
// listed — the same traversal/escape contract HandleFileGet enforces.
func TestHandleDirList_Traversal(t *testing.T) {
	h, projName, _ := newProjectHandlersForTest(t, map[string]string{"src/foo.go": "x"})
	for _, rel := range []string{"../", "a/../../x", "/etc", "x\x00", ".."} {
		rec := doDirList(t, h, projName, rel)
		if rec.Code == http.StatusOK {
			t.Errorf("path %q should be rejected, got 200 (body=%s)", rel, rec.Body.String())
		}
	}
}

// TestHandleDirList_RootListing lists a project root and asserts directories
// sort before files and the root flags are set.
func TestHandleDirList_RootListing(t *testing.T) {
	h, projName, _ := newProjectHandlersForTest(t, map[string]string{
		"zeta.txt":      "z",
		"alpha.txt":     "a",
		"src/inner.go":  "package src",
		"docs/guide.md": "# g",
	})
	rec := doDirList(t, h, projName, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("root listing status = %d, body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeDirList(t, rec)
	if !resp.AtRoot || resp.Path != "" || resp.Parent != "" {
		t.Errorf("root flags wrong: at_root=%v path=%q parent=%q", resp.AtRoot, resp.Path, resp.Parent)
	}
	// Expect: docs/, src/ (dirs, alpha), then CLAUDE.md, alpha.txt, zeta.txt.
	// Assert all dirs precede all files.
	seenFile := false
	var names []string
	for _, e := range resp.Entries {
		names = append(names, e.Name)
		if !e.IsDir {
			seenFile = true
		} else if seenFile {
			t.Errorf("directory %q sorted after a file; entries=%v", e.Name, names)
		}
	}
	// docs and src must both be present and flagged as dirs.
	dirs := map[string]bool{}
	for _, e := range resp.Entries {
		if e.IsDir {
			dirs[e.Name] = true
		}
	}
	if !dirs["docs"] || !dirs["src"] {
		t.Errorf("expected docs/ and src/ dirs, got %v", names)
	}
}

// TestHandleDirList_Subdir navigates into a subdirectory and checks the
// parent/at_root affordance fields.
func TestHandleDirList_Subdir(t *testing.T) {
	h, projName, _ := newProjectHandlersForTest(t, map[string]string{
		"src/deep/leaf.go": "package deep",
	})
	rec := doDirList(t, h, projName, "src/deep")
	if rec.Code != http.StatusOK {
		t.Fatalf("subdir status = %d, body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeDirList(t, rec)
	if resp.AtRoot {
		t.Error("subdir should not be at_root")
	}
	if resp.Path != "src/deep" {
		t.Errorf("path = %q, want src/deep", resp.Path)
	}
	if resp.Parent != "src" {
		t.Errorf("parent = %q, want src", resp.Parent)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].Name != "leaf.go" || resp.Entries[0].IsDir {
		t.Errorf("entries = %+v, want single file leaf.go", resp.Entries)
	}
}

// TestHandleDirList_SensitiveFilesHidden asserts credential files are omitted
// from the listing while ordinary files remain.
func TestHandleDirList_SensitiveFilesHidden(t *testing.T) {
	h, projName, _ := newProjectHandlersForTest(t, map[string]string{
		".env":       "SECRET=1",
		"id_rsa":     "KEY",
		"server.pem": "CERT",
		"main.go":    "package main",
	})
	rec := doDirList(t, h, projName, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	resp := decodeDirList(t, rec)
	for _, e := range resp.Entries {
		switch e.Name {
		case ".env", "id_rsa", "server.pem":
			t.Errorf("sensitive file %q must not appear in listing", e.Name)
		}
	}
	// main.go (and CLAUDE.md) must still be present.
	found := false
	for _, e := range resp.Entries {
		if e.Name == "main.go" {
			found = true
		}
	}
	if !found {
		t.Error("non-sensitive main.go should be listed")
	}
}

// TestHandleDirList_SymlinkSkipped ensures symlinked entries are dropped (they
// could point outside the root and would be rejected on navigation anyway).
func TestHandleDirList_SymlinkSkipped(t *testing.T) {
	h, projName, projDir := newProjectHandlersForTest(t, map[string]string{
		"real.txt": "ok",
	})
	// Symlink pointing outside the project root.
	link := filepath.Join(projDir, "escape")
	if err := os.Symlink(os.TempDir(), link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	rec := doDirList(t, h, projName, "")
	resp := decodeDirList(t, rec)
	for _, e := range resp.Entries {
		if e.Name == "escape" {
			t.Error("symlink entry must be skipped from listing")
		}
	}
}

// TestHandleDirList_UnknownProject returns 404 for an unregistered project and
// rejects __public_tmp__ when the gate is off.
func TestHandleDirList_UnknownProject(t *testing.T) {
	h, _, _ := newProjectHandlersForTest(t, nil)
	for _, name := range []string{"nope", publicTmpProject, "__workspace__"} {
		rec := doDirList(t, h, name, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("project %q: status = %d, want 404", name, rec.Code)
		}
	}
}

// TestHandleDirList_WorkspacePseudoProject lists the default workspace via the
// __workspace__ pseudo-project (root = router.DefaultWorkspace()).
func TestHandleDirList_WorkspacePseudoProject(t *testing.T) {
	h, _, _ := newProjectHandlersForTest(t, nil)
	wsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsDir, "subproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "note.txt"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.router = session.NewRouter(session.RouterConfig{Workspace: wsDir})

	rec := doDirList(t, h, workspaceProject, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("__workspace__ status = %d, body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeDirList(t, rec)
	if !resp.AtRoot {
		t.Error("workspace root should be at_root")
	}
	var sawDir, sawFile bool
	for _, e := range resp.Entries {
		if e.Name == "subproj" && e.IsDir {
			sawDir = true
		}
		if e.Name == "note.txt" && !e.IsDir {
			sawFile = true
		}
	}
	if !sawDir || !sawFile {
		t.Errorf("workspace listing missing entries: %+v", resp.Entries)
	}
}

// TestHandleDirList_PublicTmpForeignPrivateDirHidden pins that a 0700
// directory owned by a foreign UID is omitted from the __public_tmp__ listing
// — /tmp is world-listable (sticky 1777) so os.ReadDir returns its name, but
// the foreign-private gate must hide it (enumeration parity with
// HandleFilesExists / HandleFileGet, R245-SEC-7). The gate applies to
// directories, not just files.
func TestHandleDirList_PublicTmpForeignPrivateDirHidden(t *testing.T) {
	h, _, _ := newProjectHandlersForTest(t, nil)
	h.publicTmpEnabled = true

	// Make any inode we create appear foreign-owned (see the same trick in
	// TestHandleFileGet_PublicTmpRejectsForeignPrivate).
	origEUID := processEUID
	processEUID = ^uint32(0)
	t.Cleanup(func() { processEUID = origEUID })

	dir, err := os.MkdirTemp("/tmp", "naozhi-dirlist-foreign-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(dir)

	rec := doDirList(t, h, publicTmpProject, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("public tmp listing status = %d, body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeDirList(t, rec)
	for _, e := range resp.Entries {
		if e.Name == base {
			t.Errorf("foreign-private 0700 dir %q must be hidden from __public_tmp__ listing", base)
		}
	}
}

// TestParentRel covers the workspace-relative parent derivation.
func TestParentRel(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"foo":         "",
		"foo/bar":     "foo",
		"a/b/c":       "a/b",
		"deep/nested": "deep",
	}
	for in, want := range cases {
		if got := parentRel(in); got != want {
			t.Errorf("parentRel(%q) = %q, want %q", in, got, want)
		}
	}
}
