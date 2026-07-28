package session

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

const gitTestKey = "dashboard:pj:abc0123456789012:general"

// passthroughValidateWS is the permissive stand-in for
// internal/server.validateWorkspace: it accepts any absolute path under root
// (or any absolute path when root is empty) so these tests exercise the git
// resolution rather than re-testing path containment, which
// internal/server/server_validate_test.go already covers.
func passthroughValidateWS(ws, root string) (string, error) {
	if !filepath.IsAbs(ws) {
		return "", errors.New("not absolute")
	}
	clean := filepath.Clean(ws)
	if root != "" && clean != root && !strings.HasPrefix(clean, root+string(filepath.Separator)) {
		return "", errors.New("outside root")
	}
	return clean, nil
}

// newGitHandler builds Handlers over a real Router whose chat-level workspace
// override points at ws, with a permissive validateWS wired in.
func newGitHandler(t *testing.T, ws string) *Handlers {
	t.Helper()
	r := sessionpkg.NewRouter(sessionpkg.RouterConfig{
		MaxProcs:  4,
		StorePath: filepath.Join(t.TempDir(), "sessions.json"),
	})
	t.Cleanup(r.Shutdown)
	if ws != "" {
		// The override key is the chat key (session key minus the agent tail).
		chatKey := gitTestKey[:strings.LastIndexByte(gitTestKey, ':')]
		r.SetWorkspace(chatKey, ws)
	}
	h := New(Deps{Router: r, ValidateWS: passthroughValidateWS})
	return h
}

func doGit(t *testing.T, h *Handlers, query string) (*http.Response, gitStateView) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/git?"+query, nil)
	rec := httptest.NewRecorder()
	h.HandleGit(rec, req)
	res := rec.Result()
	var body gitStateView
	if res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return res, body
}

// makeRepo lays out a minimal main-worktree checkout on the given branch.
func makeRepo(t *testing.T, dir, branch string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHandleGit_MissingKey(t *testing.T) {
	t.Parallel()
	h := newGitHandler(t, "")
	res, _ := doGit(t, h, "")
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a missing key", res.StatusCode)
	}
}

func TestHandleGit_InvalidKeyRejected(t *testing.T) {
	t.Parallel()
	h := newGitHandler(t, "")
	// Control byte in the key — must never reach slog / the router.
	res, _ := doGit(t, h, "key=a%00b")
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a control-byte key", res.StatusCode)
	}
}

func TestHandleGit_ReportsBranchForWorkspace(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	repo := filepath.Join(base, "myrepo")
	makeRepo(t, repo, "master")

	h := newGitHandler(t, repo)
	res, body := doGit(t, h, "key="+gitTestKey)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if !body.IsRepo {
		t.Fatal("is_repo = false, want true")
	}
	if body.Branch != "master" {
		t.Errorf("branch = %q, want master", body.Branch)
	}
	if body.Repo != "myrepo" {
		t.Errorf("repo = %q, want myrepo", body.Repo)
	}
	if body.Worktree != "" {
		t.Errorf("worktree = %q, want empty for a main tree", body.Worktree)
	}
	if body.Workspace != repo {
		t.Errorf("workspace = %q, want %q", body.Workspace, repo)
	}
}

func TestHandleGit_ReportsLinkedWorktree(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	mainTree := filepath.Join(base, "naozhi")
	wtTree := filepath.Join(mainTree, ".claude", "worktrees", "topic")
	wtGitDir := filepath.Join(mainTree, ".git", "worktrees", "topic")
	for _, d := range []string{wtTree, wtGitDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wtGitDir, "HEAD"), []byte("ref: refs/heads/worktree-topic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// commondir is what marks a real linked worktree (git writes "../.." here);
	// gitinfo requires it so a directory coincidentally named "worktrees" is
	// not misread as one.
	if err := os.WriteFile(filepath.Join(wtGitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtTree, ".git"), []byte("gitdir: "+wtGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := newGitHandler(t, wtTree)
	_, body := doGit(t, h, "key="+gitTestKey)
	if !body.IsRepo {
		t.Fatal("is_repo = false, want true")
	}
	if body.Worktree != "topic" {
		t.Errorf("worktree = %q, want topic", body.Worktree)
	}
	if body.Branch != "worktree-topic" {
		t.Errorf("branch = %q, want worktree-topic", body.Branch)
	}
	if body.Repo != "naozhi" {
		t.Errorf("repo = %q, want naozhi", body.Repo)
	}
}

func TestHandleGit_NonRepoWorkspaceIsOKWithIsRepoFalse(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	h := newGitHandler(t, ws)
	res, body := doGit(t, h, "key="+gitTestKey)
	// Best-effort contract: a plain folder is a normal state, not an error.
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a non-repo workspace", res.StatusCode)
	}
	if body.IsRepo {
		t.Error("is_repo = true for a plain directory")
	}
	if body.Workspace != ws {
		t.Errorf("workspace = %q, want the resolved dir echoed back", body.Workspace)
	}
}

func TestHandleGit_UnknownKeyFallsBackToRouterDefault(t *testing.T) {
	t.Parallel()
	// No override for this key and no default cwd → nothing to resolve.
	h := newGitHandler(t, "")
	res, body := doGit(t, h, "key=feishu:p2p:nobody:general")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if body.IsRepo || body.Workspace != "" {
		t.Errorf("got %+v, want the empty is_repo=false shape", body)
	}
}

func TestHandleGit_WorkspaceOutsideAllowedRootIsNotRead(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	repo := filepath.Join(base, "outside")
	makeRepo(t, repo, "secret-branch")

	h := newGitHandler(t, repo)
	// Tighten allowedRoot after the override was stored — the stale-entry case.
	h.allowedRoot = filepath.Join(base, "allowed")
	if err := os.MkdirAll(h.allowedRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	res, body := doGit(t, h, "key="+gitTestKey)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if body.IsRepo || body.Branch != "" || body.Workspace != "" {
		t.Errorf("got %+v, want nothing disclosed for an out-of-root workspace", body)
	}
}

func TestHandleGit_NilValidateWSFailsClosed(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	makeRepo(t, repo, "master")

	h := newGitHandler(t, repo)
	h.validateWS = nil // hand-built Handlers / un-wired deps

	res, body := doGit(t, h, "key="+gitTestKey)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if body.IsRepo || body.Workspace != "" {
		t.Errorf("got %+v, want the empty shape when validateWS is unwired", body)
	}
}

func TestHandleGit_LiveSessionWorkspaceWinsOverChatOverride(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	overrideRepo := filepath.Join(base, "override")
	liveRepo := filepath.Join(base, "live")
	makeRepo(t, overrideRepo, "override-branch")
	makeRepo(t, liveRepo, "live-branch")

	r := sessionpkg.NewRouter(sessionpkg.RouterConfig{
		MaxProcs:  4,
		StorePath: filepath.Join(t.TempDir(), "sessions.json"),
	})
	t.Cleanup(r.Shutdown)
	chatKey := gitTestKey[:strings.LastIndexByte(gitTestKey, ':')]
	r.SetWorkspace(chatKey, overrideRepo)
	// A live session carries the cwd its CLI process actually runs in; that
	// must beat the chat-level override.
	sess := r.InjectSession(gitTestKey, &sessionpkg.TestProcess{AliveVal: true})
	sess.SetWorkspaceForTest(liveRepo)

	h := New(Deps{Router: r, ValidateWS: passthroughValidateWS})
	_, body := doGit(t, h, "key="+gitTestKey)
	if body.Branch != "live-branch" {
		t.Errorf("branch = %q, want live-branch (the live session's cwd)", body.Branch)
	}
}
