// Phase 5-prep / R-server-validate-extract (2026-05-28):
// Workspace 验证 helpers + 4 个 sentinel errors 抽到独立文件。
// 纯物理切分、零行为变化。
//
// 这一组实现了 Server 的 workspace 信任边界：
//   - validateWorkspace        — 主入口（IsAbs + EvalSymlinks + Stat + 根 prefix 检查）
//   - classifyWorkspaceErr     — 把 sentinel 翻译成 (HTTP status, public msg)
//   - validateRemoteWorkspace  — 跨节点 RPC 入口的语法检查
//   - pathErrReason            — 文件系统错误归类（用于 slog 不复读 path）
//
// 加 4 个 sentinel error（ErrWorkspace*）。
//
// 所有调用方（cron / send / takeover handler）通过 package-level 可见性
// 继续访问，无需改动。
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

// Sentinel errors returned by validateWorkspace. Handlers map these onto
// status codes + machine-readable reason tags; they intentionally carry no
// path detail so error messages never leak host filesystem layout to
// authenticated dashboard / IM clients (the slog Debug line in
// validateWorkspace is the operator-side diagnostic surface).
//
// Why sentinels instead of one generic string: the previous design returned
// the same "workspace is not a valid directory" for IsAbs / EvalSymlinks /
// Stat / prefix-mismatch failures, leaving the dashboard to render a single
// "无权限或参数越界" toast for four very different operator-actionable
// causes. Cron handlers in particular have to distinguish "path doesn't
// exist on this host" (operator typo) from "path outside allowedRoot"
// (real boundary violation) so users can self-correct.
var (
	ErrWorkspaceInvalid     = errors.New("workspace not a valid path")
	ErrWorkspaceNotExist    = errors.New("workspace path does not exist")
	ErrWorkspaceNotDir      = errors.New("workspace path is not a directory")
	ErrWorkspaceOutsideRoot = errors.New("workspace outside allowed root")
)

// validateWorkspace checks that workspace is an existing directory within allowedRoot.
// Returns the cleaned, symlink-resolved path or one of the Err* sentinels above.
//
// Ordering: EvalSymlinks is performed first so the root-prefix check sees the
// canonical resolved path; only then do we Stat the resolved form. This
// collapses the TOCTOU window where a symlink swap between an initial Stat
// and a later EvalSymlinks could cause the two calls to observe different
// filesystem entries.
//
// Symmetry with cron.workDirUnderRoot: both wsPath AND allowedRoot are
// resolved via EvalSymlinks before the containment check. Without this, a host
// where allowedRoot itself contains a symlinked component (e.g. `/home →
// /var/home` on some distros, Docker bind-mounts, AMI-customized layouts)
// would always fail the prefix check because resolved wsPath lands under
// the canonical path while allowedRoot stays in the un-resolved form.
//
// The containment test itself is the shared osutil.PathContainedInRoot —
// the same implementation cron.workDirResolveUnderRoot now calls, so the
// SHARED-ALGORITHM-WITH-SERVER contract is enforced by a single function
// rather than two copies kept in sync by comment.
//
// Errors are sentinels — the resolved path and underlying os.PathError are
// NOT included so a dashboard or IM user cannot enumerate host filesystem
// layout via crafted workspace queries. Diagnostic detail goes to slog.Debug.
func validateWorkspace(workspace, allowedRoot string) (string, error) {
	if workspace == "" {
		return "", ErrWorkspaceInvalid
	}
	// Explicit absolute-path contract: filepath.Clean preserves relative input
	// verbatim, and when allowedRoot is absolute the HasPrefix check below
	// will always fail for a relative workspace — correct today but implicit.
	// Reject upfront so a future relative allowedRoot cannot silently admit
	// `../etc/passwd` style traversal.
	if !filepath.IsAbs(workspace) {
		return "", ErrWorkspaceInvalid
	}
	wsPath := filepath.Clean(workspace)
	resolved, err := filepath.EvalSymlinks(wsPath)
	if err != nil {
		// *os.PathError echoes the same path back through err.Error() which
		// lands in debug logs twice. Reduce to a structural kind so operators
		// can still distinguish "not exist" from "permission denied" without
		// a duplicate path column.
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
		// Resolve allowedRoot the same way wsPath was resolved above so a
		// symlinked root component (e.g. `/home → /var/home`) doesn't cause
		// every call to fail the prefix check. EvalSymlinks failure on root
		// falls back to the raw path — matches cron.workDirUnderRoot.
		rootResolved, err := filepath.EvalSymlinks(allowedRoot)
		if err != nil {
			slog.Debug("validateWorkspace: allowedRoot EvalSymlinks failed; falling back to raw",
				"root", allowedRoot, "reason", pathErrReason(err))
			rootResolved = allowedRoot
		}
		// Containment honours filesystem semantics, not byte identity: the
		// shared osutil.PathContainedInRoot falls back to an inode walk when
		// the byte-wise prefix fails, so a case-insensitive fs (macOS APFS,
		// Windows NTFS) where EvalSymlinks preserved user-typed case no longer
		// rejects a legitimate child. Both sides are already EvalSymlinks-
		// resolved above, which is the helper's input contract and what keeps
		// the symlink-escape rejection intact.
		if !osutil.PathContainedInRoot(wsPath, rootResolved) {
			return "", ErrWorkspaceOutsideRoot
		}
	}
	return wsPath, nil
}

// classifyWorkspaceErr maps a validateWorkspace sentinel onto (HTTP status,
// public message). Centralising the mapping here keeps every handler
// (cron, send, takeover) returning consistent status codes and reason
// tags. The reason tag is short ASCII so the dashboard's localizeAPIError
// can show it verbatim in the tail without leaking host filesystem paths.
//
// Why two channels (status code + tag string): clients need the status
// code for retry/redirect logic and the tag to render an actionable
// message. "invalid work_dir" alone forced operators to read server logs
// to know whether they typo'd the path, picked a non-existent project,
// or hit the allowedRoot boundary.
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
		// Defensive: unknown error type → conservative 403 generic.
		// Should never happen because validateWorkspace only returns the
		// sentinels above.
		return http.StatusForbidden, "invalid work_dir"
	}
}

// validateRemoteWorkspace is the primary-side syntactic check applied to a
// workspace string that will be forwarded to a remote reverse node via the
// RPC "send" method. The primary cannot Stat the remote filesystem, but it
// can and should reject obviously unsafe inputs — absolute path shape, no
// NUL, no control bytes, bounded length, no traversal markers — before
// relaying. Without this guard, an authenticated dashboard user could
// submit `../../../etc` as a workspace to a node whose defaultWorkspace is
// empty and have the remote connector happily bind it. The remote node
// runs its own EvalSymlinks check, but that check uses the node's own
// defaults; sharing the primary's allowedRoot across nodes is not always
// possible (nodes may have different filesystem layouts). R61-SEC-2.
func validateRemoteWorkspace(workspace string) error {
	// Delegate to the canonical session-layer validator so the two trust
	// boundaries (primary HTTP / RPC) cannot drift. session.ValidateRemote-
	// WorkspacePath additionally does a utf8.ValidString sweep which the
	// previous inline byte-level scan here missed — an attacker could
	// submit a non-UTF8 byte sequence like 0xFF 0xFE that passes the
	// `< 0x20` check yet corrupts slog TextHandler output downstream.
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
