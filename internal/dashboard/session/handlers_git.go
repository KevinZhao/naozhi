package session

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/gitinfo"
	"github.com/naozhi/naozhi/internal/osutil"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

// gitStateView is the wire shape for GET /api/sessions/git. IsRepo is the
// only field the client can rely on being present — when false every other
// field is empty and the dashboard renders no chip.
type gitStateView struct {
	IsRepo bool `json:"is_repo"`
	// Workspace is the session's resolved cwd. Already exposed on
	// /api/sessions (SessionSnapshot.Workspace), so this adds no new surface;
	// it is echoed here so the chip's tooltip can name the directory the
	// branch was read from without a second lookup.
	Workspace string `json:"workspace,omitempty"`
	// Root is the working-tree root — differs from Workspace when the session
	// runs in a subdirectory of the repo.
	Root string `json:"root,omitempty"`
	// Repo is the repository identity (main working tree's directory name),
	// shared by a linked worktree and its parent.
	Repo     string `json:"repo,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Detached bool   `json:"detached,omitempty"`
	HeadSHA  string `json:"head_sha,omitempty"`
}

// HandleGit serves GET /api/sessions/git?key= — the git branch / worktree the
// session's workspace sits on, so the operator can tell at a glance which
// checkout a conversation is editing.
//
// Read-only and best-effort: any resolution failure (unknown key, workspace
// outside allowedRoot, not a git repo) returns 200 with is_repo=false rather
// than an error status. The chip is decoration; a 4xx here would surface as a
// console error on every non-git workspace, which is the normal case for
// plain document folders.
//
// Local-node only, mirroring HandleRuns: a remote session's workspace lives
// on that node's filesystem, so resolving it here would read the wrong tree.
// The frontend skips the call for node != "local".
func (h *Handlers) HandleGit(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}
	// Same key hygiene the events / runs endpoints enforce (R172-SEC-L2):
	// caps length and rejects control bytes before the key reaches slog.
	if err := sessionpkg.ValidateSessionKey(key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}

	ws := h.resolveSessionWorkspace(key)
	if ws == "" {
		httputil.WriteJSON(w, gitStateView{})
		return
	}
	// Re-validate the stored workspace against allowedRoot before touching the
	// filesystem: the value was validated at SetWorkspace time, but a config
	// change (allowedRoot tightened since) can leave a stale entry behind, and
	// this handler must not read outside the operator's declared tree.
	// A nil validateWS (hand-built Handlers in tests) fails closed.
	if h.validateWS == nil {
		httputil.WriteJSON(w, gitStateView{})
		return
	}
	wsPath, err := h.validateWS(ws, h.allowedRoot)
	if err != nil {
		slog.Debug("git state: workspace validation failed",
			"err", err, "workspace", osutil.SanitizeForLog(ws, 256))
		httputil.WriteJSON(w, gitStateView{})
		return
	}

	// allowedRoot bounds gitinfo's ancestor walk and its gitdir-pointer follow.
	// Without it, validateWS would prove only that the WORKSPACE is in-tree
	// while Detect walked up past the boundary and reported a parent repo's
	// path + branch — e.g. allowed_root=<repo>/docs still disclosing <repo> and
	// its current branch. Empty allowedRoot means the deployment declared no
	// containment policy at all, which gitinfo mirrors as unbounded.
	//
	// The bound must be symlink-resolved to compare against wsPath, which
	// validateWorkspace already returns resolved. validateWorkspace resolves
	// allowedRoot internally for its own check but doesn't hand it back, so
	// repeat it here with the same EvalSymlinks-failure fallback (raw path) it
	// uses — otherwise a symlinked root component (/home → /var/home) would
	// make every lookup fail closed instead of reporting the branch.
	st, ok := gitinfo.Detect(wsPath, resolveRootForBound(h.allowedRoot))
	if !ok {
		// Not a git checkout — a legitimate, common state. Echo the workspace
		// so the client can still show the directory if it wants to.
		httputil.WriteJSON(w, gitStateView{Workspace: wsPath})
		return
	}
	httputil.WriteJSON(w, gitStateView{
		IsRepo:    true,
		Workspace: wsPath,
		Root:      st.Root,
		Repo:      st.Repo,
		Branch:    st.Branch,
		Worktree:  st.Worktree,
		Detached:  st.Detached,
		HeadSHA:   st.HeadSHA,
	})
}

// resolveRootForBound symlink-resolves allowedRoot so it can be compared
// lexically against the resolved workspace path. Mirrors validateWorkspace's
// own handling, including the "EvalSymlinks failed → use the raw path" fallback
// (a root that cannot be resolved must still bound the walk, just less
// precisely — falling back to "" would silently unbound it).
func resolveRootForBound(allowedRoot string) string {
	if allowedRoot == "" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(allowedRoot)
	if err != nil {
		return allowedRoot
	}
	return resolved
}

// resolveSessionWorkspace returns the cwd a session key runs in, using the
// same precedence as resolveAttachmentWorkspace in internal/server: the live
// session's own workspace first (that is the directory the CLI process
// actually has open), then the chat-level override, then the router default.
//
// Duplicating the precedence rather than importing it is deliberate — the
// server-side helper hangs off *Hub, and this package must not reverse-import
// internal/server (Phase 3e boundary).
func (h *Handlers) resolveSessionWorkspace(key string) string {
	if h.router == nil {
		return ""
	}
	if sess := h.router.SessionFor(key); sess != nil {
		if ws := sess.Workspace(); ws != "" {
			return ws
		}
	}
	// The trailing ":<agent>" segment is a per-agent discriminator, not part
	// of the workspace-override key, so strip it before the router lookup.
	chatKey := key
	if idx := strings.LastIndexByte(key, ':'); idx > 0 {
		chatKey = key[:idx]
	}
	return h.router.Workspace(chatKey)
}
