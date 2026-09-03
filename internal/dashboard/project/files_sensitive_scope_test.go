package project

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/project"
)

// newNamedProjectHandlersForTest is newProjectHandlersForTest with a caller-
// chosen project directory name, so tests can place the workspace root
// itself under a credential-looking segment (e.g. "secrets").
func newNamedProjectHandlersForTest(t *testing.T, projName string, files map[string]string) (*Handlers, string) {
	t.Helper()
	root := t.TempDir()
	projDir := filepath.Join(root, projName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "CLAUDE.md"), []byte("# p"), 0o644); err != nil {
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
	return &Handlers{projectMgr: mgr}, projDir
}

func TestWorkspaceScanPath(t *testing.T) {
	t.Parallel()
	root := filepath.Join(string(filepath.Separator), "home", "u", "secrets")
	cases := []struct {
		abs  string
		want string
	}{
		{filepath.Join(root, "app.go"), "app.go"},
		{filepath.Join(root, "secrets", "db.yaml"), filepath.Join("secrets", "db.yaml")},
		{root, "."},
		// Outside the root (should never happen after the prefix guard):
		// fall back to the absolute path so the scan never gets weaker.
		{filepath.Join(string(filepath.Separator), "etc", "passwd"), filepath.Join(string(filepath.Separator), "etc", "passwd")},
	}
	for _, tc := range cases {
		if got := workspaceScanPath(root, tc.abs); got != tc.want {
			t.Errorf("workspaceScanPath(%q, %q) = %q, want %q", root, tc.abs, got, tc.want)
		}
	}
	if got := workspaceScanPath("", "/x/secrets/y"); got != "/x/secrets/y" {
		t.Errorf("empty root must pass abs through, got %q", got)
	}
}

// #2433 item 4: a workspace whose root (or an ancestor) is itself named like
// a credential segment must still be browsable — only segments BELOW the
// workspace root are subject to the sensitive scan.
func TestHandleFilesList_RootNamedSecrets_NotHidden(t *testing.T) {
	h, _ := newNamedProjectHandlersForTest(t, "secrets", map[string]string{
		"app.go":          "package main",
		"secrets/db.yaml": "pw",
		".ssh/id_x":       "k",
	})
	w, resp := doList(t, h, "project=secrets")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	names := entryNames(resp.Entries)
	if _, ok := names["app.go"]; !ok {
		t.Errorf("app.go must be listed when only the ROOT is named secrets; got %v", resp.Entries)
	}
	// Negative: in-workspace credential subtrees stay hidden.
	for _, banned := range []string{"secrets", ".ssh"} {
		if _, ok := names[banned]; ok {
			t.Errorf("%q subtree inside the workspace must still be omitted", banned)
		}
	}
}

func TestHandleFileGet_RootNamedSecrets_Served(t *testing.T) {
	h, _ := newNamedProjectHandlersForTest(t, "secrets", map[string]string{
		"app.go":            "package main\n",
		"page.html":         "<html><body>ok</body></html>\n",
		"secrets/db.yaml":   "pw: 1\n",
		"secrets/page.html": "<html>pw</html>\n",
	})
	get := func(path, mode string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet,
			"/api/projects/file?project=secrets&path="+path+"&mode="+mode, nil)
		w := httptest.NewRecorder()
		h.HandleFileGet(w, req)
		return w
	}
	for _, mode := range []string{"preview", "raw", "render", "download"} {
		path := "app.go"
		if mode == "render" { // render only accepts HTML/SVG
			path = "page.html"
		}
		w := get(path, mode)
		if w.Code != http.StatusOK {
			t.Fatalf("mode=%s: want 200 for %s under a root named secrets, got %d body=%s", mode, path, w.Code, w.Body.String())
		}
		if mode == "preview" {
			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp["binary"] == true || resp["content"] != "package main\n" {
				t.Fatalf("preview must return real content, got %v", resp)
			}
		}
	}
	// Negative: the in-workspace secrets/ subtree is still refused.
	for _, mode := range []string{"raw", "download"} {
		if w := get("secrets/db.yaml", mode); w.Code != http.StatusForbidden {
			t.Errorf("mode=%s: secrets/db.yaml inside the workspace must be 403, got %d", mode, w.Code)
		}
	}
	if w := get("secrets/page.html", "render"); w.Code != http.StatusForbidden {
		t.Errorf("mode=render: secrets/page.html inside the workspace must be 403, got %d", w.Code)
	}
	w := get("secrets/db.yaml", "preview")
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["binary"] != true || resp["content"] != "" {
		t.Errorf("preview of in-workspace secrets/db.yaml must stay masked, got %v", resp)
	}
}
