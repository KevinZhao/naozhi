package project

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSessionRootHandlers builds Handlers with no project manager and a fake
// router whose sessions sit in a git repo (cwd = repo/sub), a plain directory,
// outside allowed_root, and nowhere. outside/ holds the symlink targets.
func newSessionRootHandlers(t *testing.T) *Handlers {
	t.Helper()
	tmp := t.TempDir()
	allowed := filepath.Join(tmp, "allowed")
	repo := filepath.Join(allowed, "repo")
	outside := filepath.Join(tmp, "outside")
	files := map[string]string{
		filepath.Join(repo, ".git", "HEAD"):      "ref: refs/heads/main\n",
		filepath.Join(repo, "README.md"):         "hello",
		filepath.Join(repo, "sub", "a.txt"):      "in sub",
		filepath.Join(repo, ".env"):              "SECRET=1",
		filepath.Join(allowed, "plain", "b.txt"): "plain",
		filepath.Join(outside, "x.txt"):          "outside",
	}
	for p, content := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(outside, "x.txt"), filepath.Join(repo, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "linkdir")); err != nil {
		t.Fatal(err)
	}

	validate := func(ws, root string) (string, error) {
		resolved, err := filepath.EvalSymlinks(ws)
		if err != nil {
			return "", err
		}
		rootResolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return "", err
		}
		if resolved != rootResolved && !strings.HasPrefix(resolved, rootResolved+string(filepath.Separator)) {
			return "", errors.New("outside allowed root")
		}
		return resolved, nil
	}
	return New(Deps{
		Router: &fakeRouter{workspaces: map[string]string{
			"dashboard:direct:repo:general":  filepath.Join(repo, "sub"),
			"dashboard:direct:plain:general": filepath.Join(allowed, "plain"),
			"dashboard:direct:out:general":   outside,
			"dashboard:direct:gone:general":  "",
		}},
		AllowedRoot: allowed,
		ValidateWS:  validate,
	})
}

func TestHandleFileGet_SessionRoot(t *testing.T) {
	t.Parallel()
	h := newSessionRootHandlers(t)
	const repoKey = "session:dashboard:direct:repo:general"

	cases := []struct {
		name, root, path, mode string
		want                   int
		body                   string
	}{
		{"git root above cwd", repoKey, "README.md", "preview", http.StatusOK, "hello"},
		{"inside cwd", repoKey, "sub/a.txt", "preview", http.StatusOK, "in sub"},
		{"download mode", repoKey, "README.md", "download", http.StatusOK, "hello"},
		{"plain dir", "session:dashboard:direct:plain:general", "b.txt", "preview", http.StatusOK, "plain"},
		{"traversal", repoKey, "../../outside/x.txt", "preview", http.StatusNotFound, ""},
		{"absolute path", repoKey, "/etc/passwd", "preview", http.StatusNotFound, ""},
		{"symlink file escape", repoKey, "escape", "preview", http.StatusNotFound, ""},
		{"symlink dir escape", repoKey, "linkdir/x.txt", "raw", http.StatusNotFound, ""},
		{"sensitive file preview", repoKey, ".env", "preview", http.StatusOK, `"binary":true`},
		{"sensitive file raw", repoKey, ".env", "raw", http.StatusForbidden, ""},
		{"sensitive file download", repoKey, ".env", "download", http.StatusForbidden, ""},
		{"unknown session", "session:dashboard:direct:nope:general", "README.md", "preview", http.StatusNotFound, ""},
		{"workspace outside allowed root", "session:dashboard:direct:out:general", "x.txt", "preview", http.StatusNotFound, ""},
		{"session without workspace", "session:dashboard:direct:gone:general", "x.txt", "preview", http.StatusNotFound, ""},
		{"unknown root kind", "cron:job", "README.md", "preview", http.StatusBadRequest, ""},
		{"control byte in key", "session:a\x01b", "README.md", "preview", http.StatusBadRequest, ""},
		{"missing path", repoKey, "", "preview", http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{"root": {tc.root}, "path": {tc.path}, "mode": {tc.mode}}
			w := httptest.NewRecorder()
			h.HandleFileGet(w, httptest.NewRequest(http.MethodGet, "/api/projects/file?"+q.Encode(), nil))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
			if tc.body != "" && !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("body %q does not contain %q", w.Body.String(), tc.body)
			}
			// Neither the credential nor anything outside the root may leak.
			if b := w.Body.String(); strings.Contains(b, "SECRET") || strings.Contains(b, "outside") {
				t.Fatalf("body leaks protected content: %q", b)
			}
		})
	}
}

func TestHandleFilesList_SessionRoot(t *testing.T) {
	t.Parallel()
	h := newSessionRootHandlers(t)
	const repoKey = "session:dashboard:direct:repo:general"

	cases := []struct {
		name, root, dir string
		want            int
	}{
		{"root", repoKey, "", http.StatusOK},
		{"subdir", repoKey, "sub", http.StatusOK},
		{"traversal", repoKey, "..", http.StatusNotFound},
		{"symlink dir escape", repoKey, "linkdir", http.StatusNotFound},
		{"unknown session", "session:dashboard:direct:nope:general", "", http.StatusNotFound},
		{"workspace outside allowed root", "session:dashboard:direct:out:general", "", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{"root": {tc.root}, "dir": {tc.dir}, "show_hidden": {"1"}}
			w := httptest.NewRecorder()
			h.HandleFilesList(w, httptest.NewRequest(http.MethodGet, "/api/projects/files/list?"+q.Encode(), nil))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
		})
	}

	// The root listing names the git tree, flags the escaping symlinks and
	// never enumerates the credential file, even with show_hidden=1.
	q := url.Values{"root": {repoKey}, "show_hidden": {"1"}}
	w := httptest.NewRecorder()
	h.HandleFilesList(w, httptest.NewRequest(http.MethodGet, "/api/projects/files/list?"+q.Encode(), nil))
	var res struct {
		RootName string      `json:"root_name"`
		Entries  []listEntry `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.RootName != "repo" {
		t.Errorf("root_name = %q, want repo", res.RootName)
	}
	got := map[string]listEntry{}
	for _, e := range res.Entries {
		got[e.Name] = e
	}
	if _, ok := got[".env"]; ok {
		t.Error(".env enumerated in a session-root listing")
	}
	for _, name := range []string{"README.md", "sub"} {
		if _, ok := got[name]; !ok {
			t.Errorf("%s missing from listing %v", name, res.Entries)
		}
	}
	for _, name := range []string{"escape", "linkdir"} {
		if !got[name].Symlink {
			t.Errorf("%s not flagged as symlink: %+v", name, got[name])
		}
	}
}
