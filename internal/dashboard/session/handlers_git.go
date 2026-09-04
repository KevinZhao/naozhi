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
	// Workspace is the session's resolved cwd (already exposed via
	// SessionSnapshot.Workspace), echoed so the chip tooltip needs no second lookup.
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
// session's workspace sits on. Read-only and best-effort: any resolution
// failure (unknown key, workspace outside allowedRoot, not a repo) returns 200
// with is_repo=false, because a 4xx would surface as a console error on every
// plain-folder workspace. Local-node only (mirrors HandleRuns); the frontend
// skips the call for node != "local".
func (h *Handlers) HandleGit(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key parameter", http.StatusBadRequest)
		return
	}
	// Same key hygiene as the events / runs endpoints: caps length and rejects
	// control bytes before the key reaches slog.
	if err := sessionpkg.ValidateSessionKey(key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}

	ws := h.resolveSessionWorkspace(key)
	if ws == "" {
		httputil.WriteJSON(w, gitStateView{})
		return
	}
	// Re-validate against allowedRoot before touching the filesystem: the value
	// was validated at SetWorkspace time, but a tightened allowedRoot can leave
	// a stale entry, and this handler must not read outside the declared tree.
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

	// allowedRoot bounds gitinfo's ancestor walk and gitdir-pointer follow;
	// otherwise Detect could walk past the boundary and disclose a parent repo's
	// path + branch (e.g. allowed_root=<repo>/docs). Empty means no containment
	// policy, which gitinfo mirrors as unbounded. The bound must be
	// symlink-resolved like wsPath, with the same raw-path fallback validateWS uses.
	st, ok := gitinfo.Detect(wsPath, resolveRootForBound(h.allowedRoot))
	if !ok {
		// Not a git checkout — common and legitimate; echo the workspace.
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

// resolveRootForBound symlink-resolves allowedRoot so it compares lexically
// against the resolved workspace path. Mirrors validateWorkspace, including the
// "EvalSymlinks failed → raw path" fallback: an unresolvable root must still
// bound the walk; falling back to "" would silently unbound it.
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

// resolveSessionWorkspace returns the cwd a session key runs in, with the same
// precedence as resolveAttachmentWorkspace in internal/server: live session
// workspace, then chat-level override, then router default. Duplicated rather
// than imported because this package must not reverse-import internal/server.
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
