package project

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/project"
)

// newIncludeRootHandlersForTest builds Handlers whose Manager has include_root
// enabled, so the root directory itself is a project named after its basename.
// Returns (handlers, rootProjectName, rootDir).
func newIncludeRootHandlersForTest(t *testing.T, rootFiles map[string]string) (*Handlers, string, string) {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	// A subdirectory project so the root is not the only entry (mirrors prod).
	sub := filepath.Join(root, "alpha")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "CLAUDE.md"), []byte("# alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, content := range rootFiles {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mgr, err := project.NewManager(root, project.PlannerDefaults{}, project.WithIncludeRoot(true))
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Scan(); err != nil {
		t.Fatal(err)
	}
	return &Handlers{projectMgr: mgr}, filepath.Base(root), root
}

// A plain file living directly under the workspace root is previewable through
// the root project — this is the whole point of include_root.
func TestIncludeRoot_GetRootFile_Served(t *testing.T) {
	h, rootName, _ := newIncludeRootHandlersForTest(t, map[string]string{
		"notes.md": "# hello\n",
	})
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+rootName+"&path=notes.md&mode=preview", nil)
	w := httptest.NewRecorder()
	h.HandleFileGet(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// A credential-named file under the root must not have its CONTENT served on
// the GET path, just like every other project (isSensitiveDownloadPath is
// universal). download/render are hard-403; preview/raw map .env to
// octet-stream so the body is never echoed (binary:true, empty content). The
// test pins this for the root project specifically.
func TestIncludeRoot_GetCredentialFile_NoContentLeak(t *testing.T) {
	h, rootName, _ := newIncludeRootHandlersForTest(t, map[string]string{
		".env": "SECRET=topsecret\n",
	})
	// download + render are explicitly forbidden by isSensitiveDownloadPath.
	for _, mode := range []string{"download", "render"} {
		req := httptest.NewRequest(http.MethodGet,
			"/api/projects/file?project="+rootName+"&path=.env&mode="+mode, nil)
		w := httptest.NewRecorder()
		h.HandleFileGet(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("mode=%s: status = %d, want 403 for credential file; body=%s", mode, w.Code, w.Body.String())
		}
	}
	// preview/raw must never echo the secret bytes regardless of status.
	for _, mode := range []string{"preview", "raw"} {
		req := httptest.NewRequest(http.MethodGet,
			"/api/projects/file?project="+rootName+"&path=.env&mode="+mode, nil)
		w := httptest.NewRecorder()
		h.HandleFileGet(w, req)
		if bytes.Contains(w.Body.Bytes(), []byte("topsecret")) {
			t.Errorf("mode=%s: .env secret content leaked in body=%s", mode, w.Body.String())
		}
	}
}

// The batch-exists path must NOT enumerate credential files under the root
// project: condition (b) of the design — isSensitiveDownloadPath now applies to
// restricted roots in the exists path, closing the {exists,size,mime} metadata
// leak that a normal project's exists path leaves open.
func TestIncludeRoot_ExistsCredentialFile_NotEnumerable(t *testing.T) {
	h, rootName, _ := newIncludeRootHandlersForTest(t, map[string]string{
		".env":         "SECRET=1\n",
		"id_rsa":       "-----BEGIN-----\n",
		"sub/notes.md": "ok\n",
	})
	body, _ := json.Marshal(existsReq{
		Project: rootName,
		Paths:   []string{".env", "id_rsa", "sub/notes.md"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/projects/files/exists", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleFilesExists(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Results map[string]struct {
			Exists bool `json:"exists"`
		} `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Results[".env"].Exists {
		t.Error(".env enumerable via exists API on root project; credential parity broken")
	}
	if resp.Results["id_rsa"].Exists {
		t.Error("id_rsa enumerable via exists API on root project; credential parity broken")
	}
	// A non-sensitive file must still resolve (the feature still works).
	if !resp.Results["sub/notes.md"].Exists {
		t.Error("sub/notes.md should be enumerable under root project")
	}
}

// Sanity: with include_root OFF, the root basename is not a project, so the
// same request 404s — confirms the gates are only reachable behind the flag.
func TestIncludeRoot_DisabledRootName404(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(filepath.Join(root, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr, err := project.NewManager(root, project.PlannerDefaults{}) // no WithIncludeRoot
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Scan(); err != nil {
		t.Fatal(err)
	}
	h := &Handlers{projectMgr: mgr}
	req := httptest.NewRequest(http.MethodGet,
		"/api/projects/file?project="+filepath.Base(root)+"&path=notes.md&mode=preview", nil)
	w := httptest.NewRecorder()
	h.HandleFileGet(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (root not a project when flag off)", w.Code)
	}
}
