package project

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/gitinfo"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/sessionkey"
)

// sessionRootPrefix marks a root=session:<key> file-API root: the working tree
// of any session rather than a registered project (planner RFC §10.2). Project
// names cannot contain ':', so the two namespaces never collide.
const sessionRootPrefix = "session:"

// isRemoteNode reports whether a file request names another node: such a
// request is never served from this host's disk, even when a local project or
// session carries the same name.
func isRemoteNode(nodeID string) bool { return nodeID != "" && nodeID != "local" }

// errRemoteFilesUnsupported answers a file request for a node that cannot
// serve its files.
const errRemoteFilesUnsupported = "remote node does not support file access"

// browseRoot is the directory a file read or listing is served from.
type browseRoot struct {
	path string
	// restricted applies the include_root gates (foreign-private, denied
	// names, irregular types, audit log). A session cwd can be as broad as
	// $HOME, so session roots always get them.
	restricted bool
	// label names the root in logs: the project name or the session key.
	label string
	// session is true for a root=session:<key> root.
	session bool
}

// fileRefusal is a file request refused with an HTTP status; the reverse
// RPCs carry the same value to the primary (node.FileRefusal).
type fileRefusal = node.FileRefusal

func refuse(status int, msg string) *fileRefusal { return &fileRefusal{Status: status, Error: msg} }

func writeRefusal(w http.ResponseWriter, rf *fileRefusal) {
	httputil.WriteJSONStatus(w, rf.Status, map[string]string{"error": rf.Error})
}

// resolveBrowseRoot picks the root of a file read or listing: root=session:<key>
// when given, else project=<name>. On failure it has already written the
// response and returns false.
func (h *Handlers) resolveBrowseRoot(w http.ResponseWriter, q url.Values) (browseRoot, bool) {
	root, rf := h.browseRootOf(q.Get("root"), q.Get("project"))
	if rf != nil {
		writeRefusal(w, rf)
		return browseRoot{}, false
	}
	return root, true
}

// browseRootOf resolves a root=session:<key> root when root is set, else the
// project name.
func (h *Handlers) browseRootOf(root, name string) (browseRoot, *fileRefusal) {
	if root != "" {
		key, ok := strings.CutPrefix(root, sessionRootPrefix)
		if !ok {
			return browseRoot{}, refuse(http.StatusBadRequest, "invalid root")
		}
		if err := sessionkey.ValidateSessionKey(key); err != nil {
			return browseRoot{}, refuse(http.StatusBadRequest, "invalid session key")
		}
		path := h.sessionRootPath(key)
		if path == "" {
			// Unknown key, no workspace and a workspace outside allowed_root
			// collapse to one 404 so probing yields no oracle.
			return browseRoot{}, refuse(http.StatusNotFound, "session not found")
		}
		return browseRoot{path: path, restricted: true, label: key, session: true}, nil
	}

	if h.deps.ProjectMgr == nil {
		return browseRoot{}, refuse(http.StatusBadRequest, "projects not configured")
	}
	if name == "" {
		return browseRoot{}, refuse(http.StatusBadRequest, "project is required")
	}
	// Same trust-boundary gate as HandleFilesExists.
	if err := validateProjectName(name); err != nil {
		return browseRoot{}, refuse(http.StatusBadRequest, "invalid project name")
	}
	p := h.deps.ProjectMgr.Get(name)
	if p == nil {
		return browseRoot{}, refuse(http.StatusNotFound, "project not found")
	}
	// A registered subdirectory project is readable by definition; only the
	// include_root whole-workspace project is restricted.
	return browseRoot{path: p.Path, restricted: p.IsRoot, label: name}, nil
}

// rootOfRPC is browseRootOf for a primary's file RPC.
func (h *Handlers) rootOfRPC(r node.FileRoot) (browseRoot, *fileRefusal) {
	if r.Key != "" {
		return h.browseRootOf(sessionRootPrefix+r.Key, "")
	}
	return h.browseRootOf("", r.Project)
}

// sessionRootPath returns the directory a session's files are browsed from:
// the git working tree (a linked worktree's own root) holding its workspace,
// else the workspace itself; "" when the key names no local session or its
// workspace no longer passes validation. Only the session's own workspace
// counts — a chat-level or router default would let an arbitrary key resolve.
func (h *Handlers) sessionRootPath(key string) string {
	if h.deps.Router == nil || h.deps.ValidateWS == nil {
		return ""
	}
	sess := h.deps.Router.SessionFor(key)
	if sess == nil {
		return ""
	}
	ws := sess.Workspace()
	if ws == "" {
		return ""
	}
	// Re-validate against allowed_root: the value was checked when it was set,
	// but a tightened allowed_root can leave a stale one.
	wsPath, err := h.deps.ValidateWS(ws, h.deps.AllowedRoot)
	if err != nil {
		return ""
	}
	// allowed_root bounds the walk up to the tree root, as in GET
	// /api/sessions/git; the process side relativizes output paths the same
	// way (cli SetOnActivity).
	return gitinfo.TreeRoot(wsPath, h.deps.AllowedRoot)
}
