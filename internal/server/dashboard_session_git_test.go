package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSessionsGitRoute_EndToEnd exercises GET /api/sessions/git through the
// real mux + auth wrapper + production validateWorkspace wiring, which the
// package-level handler tests in internal/dashboard/session cannot reach (they
// inject a permissive validateWS stub).
func TestSessionsGitRoute_EndToEnd(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	t.Cleanup(srv.router.Shutdown)

	// A repo whose .git/HEAD names a branch, inside the server's allowed root.
	root := t.TempDir()
	srv.allowedRoot = root
	srv.sessionH.SetAllowedRootForTest(root)
	repo := filepath.Join(root, "proj")
	gitDir := filepath.Join(repo, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/feat/chip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const key = "dashboard:pj:abc0123456789012:general"
	srv.router.SetWorkspace("dashboard:pj:abc0123456789012", repo)

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/git?key="+key, nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		IsRepo   bool   `json:"is_repo"`
		Branch   string `json:"branch"`
		Repo     string `json:"repo"`
		Worktree string `json:"worktree"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if !body.IsRepo {
		t.Fatalf("is_repo = false, want true; body=%s", rec.Body.String())
	}
	if body.Branch != "feat/chip" {
		t.Errorf("branch = %q, want feat/chip", body.Branch)
	}
	if body.Repo != "proj" {
		t.Errorf("repo = %q, want proj", body.Repo)
	}
	if body.Worktree != "" {
		t.Errorf("worktree = %q, want empty for a main tree", body.Worktree)
	}
}

// TestSessionsGitRoute_DoesNotDiscloseRepoAboveAllowedRoot exercises the
// containment fix through the production wiring (real validateWorkspace + the
// real allowedRoot plumbing), which the gitinfo unit tests cannot cover.
//
// Scenario: the operator narrows allowed_root to a subdirectory of a repo — a
// natural way to expose only a docs/ or projects/ tree. validateWorkspace
// correctly admits the workspace, but the ancestor walk used to sail past the
// boundary and report the enclosing repo's absolute path and current branch.
func TestSessionsGitRoute_DoesNotDiscloseRepoAboveAllowedRoot(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	t.Cleanup(srv.router.Shutdown)

	base := t.TempDir()
	// The repo sits ABOVE the allowed root.
	gitDir := filepath.Join(base, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/embargoed-release\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(base, "projects")
	ws := filepath.Join(allowed, "docs")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	srv.allowedRoot = allowed
	srv.sessionH.SetAllowedRootForTest(allowed)

	const key = "dashboard:pj:abc0123456789012:general"
	srv.router.SetWorkspace("dashboard:pj:abc0123456789012", ws)

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/git?key="+key, nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "embargoed-release") {
		t.Errorf("response leaked a branch from above allowed_root: %s", body)
	}
	var parsed struct {
		IsRepo bool   `json:"is_repo"`
		Root   string `json:"root"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.IsRepo {
		t.Errorf("is_repo = true for a workspace whose repo is outside allowed_root (root=%q)", parsed.Root)
	}
}

// TestSessionsGitRoute_RequiresAuth pins that the endpoint sits behind the
// same RequireAuth wrapper as the rest of the /api/sessions group — the git
// state names filesystem paths, so an unauthenticated read must not pass.
func TestSessionsGitRoute_RequiresAuth(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "s3cret")
	t.Cleanup(srv.router.Shutdown)

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/git?key=a:b:c:general", nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("status = 200 without a token, want an auth rejection")
	}
}

// TestSessionsGitRoute_MethodNotAllowed pins that only GET is routed — a
// pattern typo like "/api/sessions/git" without a method would otherwise
// accept POST too.
func TestSessionsGitRoute_MethodNotAllowed(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	t.Cleanup(srv.router.Shutdown)

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/git?key=a:b:c:general", nil)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("POST returned 200, want 405")
	}
}
