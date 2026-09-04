// Workspace 信任边界：validateWorkspace（IsAbs + EvalSymlinks + Stat + 根
// containment）、classifyWorkspaceErr（sentinel → HTTP status/public msg）、
// validateRemoteWorkspace（跨节点 RPC 语法检查）、pathErrReason。
package server

import (
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session"
)

// Sentinel errors returned by validateWorkspace. They intentionally carry no
// path detail so error messages never leak host filesystem layout to
// dashboard / IM clients; the slog.Debug lines are the operator-side surface.
// Distinct sentinels let handlers tell "path missing" from "outside root".
var (
	ErrWorkspaceInvalid     = errors.New("workspace not a valid path")
	ErrWorkspaceNotExist    = errors.New("workspace path does not exist")
	ErrWorkspaceNotDir      = errors.New("workspace path is not a directory")
	ErrWorkspaceOutsideRoot = errors.New("workspace outside allowed root")
)

// validateWorkspace checks that workspace is an existing directory within allowedRoot.
// Returns the cleaned, symlink-resolved path or one of the Err* sentinels above.
//
// EvalSymlinks runs before Stat so both observe the same entry (no TOCTOU
// window). Both wsPath and allowedRoot are symlink-resolved before the
// containment check (a symlinked root component like `/home → /var/home`
// would otherwise always fail); containment is the shared
// osutil.PathContainedInRoot, same as cron.workDirResolveUnderRoot.
// Returned errors never include the path or os.PathError.
func validateWorkspace(workspace, allowedRoot string) (string, error) {
	if workspace == "" {
		return "", ErrWorkspaceInvalid
	}
	// Reject relative input upfront so a relative allowedRoot could never
	// admit `../etc/passwd` style traversal.
	if !filepath.IsAbs(workspace) {
		return "", ErrWorkspaceInvalid
	}
	wsPath := filepath.Clean(workspace)
	resolved, err := filepath.EvalSymlinks(wsPath)
	if err != nil {
		slog.Debug("validateWorkspace: EvalSymlinks failed",
			"path", wsPath, "reason", pathErrReason(err))
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrWorkspaceNotExist
		}
		return "", ErrWorkspaceInvalid
	}
	wsPath = resolved
	info, err := os.Stat(wsPath)
	if err != nil {
		slog.Debug("validateWorkspace: Stat failed",
			"path", wsPath, "reason", pathErrReason(err))
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrWorkspaceNotExist
		}
		return "", ErrWorkspaceInvalid
	}
	if !info.IsDir() {
		slog.Debug("validateWorkspace: Stat failed",
			"path", wsPath, "reason", "not_a_directory")
		return "", ErrWorkspaceNotDir
	}
	if allowedRoot != "" {
		// EvalSymlinks failure on root falls back to the raw path — matches
		// cron.workDirUnderRoot.
		rootResolved, err := filepath.EvalSymlinks(allowedRoot)
		if err != nil {
			slog.Debug("validateWorkspace: allowedRoot EvalSymlinks failed; falling back to raw",
				"root", allowedRoot, "reason", pathErrReason(err))
			rootResolved = allowedRoot
		}
		// PathContainedInRoot falls back to an inode walk when the byte prefix
		// fails (case-insensitive fs); both sides must be EvalSymlinks-resolved,
		// which is its input contract and what keeps symlink-escape rejection.
		if !osutil.PathContainedInRoot(wsPath, rootResolved) {
			return "", ErrWorkspaceOutsideRoot
		}
	}
	return wsPath, nil
}

// classifyWorkspaceErr maps a validateWorkspace sentinel onto (HTTP status,
// public message). The reason tag is short ASCII the dashboard's
// localizeAPIError shows verbatim, so it must never carry a host path.
func classifyWorkspaceErr(err error) (int, string) {
	switch {
	case errors.Is(err, ErrWorkspaceOutsideRoot):
		return http.StatusForbidden, "work_dir outside allowed root"
	case errors.Is(err, ErrWorkspaceNotExist):
		return http.StatusBadRequest, "work_dir does not exist"
	case errors.Is(err, ErrWorkspaceNotDir):
		return http.StatusBadRequest, "work_dir is not a directory"
	case errors.Is(err, ErrWorkspaceInvalid):
		return http.StatusBadRequest, "work_dir is not a valid path"
	default:
		// Unknown error type → conservative generic 403.
		return http.StatusForbidden, "invalid work_dir"
	}
}

// validateRemoteWorkspace is the primary-side syntactic check applied to a
// workspace string forwarded to a remote reverse node via RPC "send". The
// primary cannot Stat the remote filesystem, but must still reject unsafe
// shapes (relative, NUL/control bytes, non-UTF8, traversal, unbounded length)
// because the remote node's own check uses the node's defaults, not this
// primary's allowedRoot.
func validateRemoteWorkspace(workspace string) error {
	// Delegates to the session-layer validator so the HTTP and RPC trust
	// boundaries cannot drift.
	return session.ValidateRemoteWorkspacePath(workspace)
}

// pathErrReason returns a short, path-free tag describing a filesystem error
// so debug logs do not echo the workspace path twice via *os.PathError.
func pathErrReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, fs.ErrNotExist):
		return "not_exist"
	case errors.Is(err, fs.ErrPermission):
		return "permission_denied"
	default:
		return "fs_error"
	}
}
