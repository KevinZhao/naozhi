package cron

import (
	"os"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/osutil"
)

// workDirReachable reports whether workDir exists and is a directory right
// now, so fresh-mode Reset does not destroy a session only to fail on spawn.
// Empty workDir means "use router default" and is always reachable.
//
// 注意：仅做 stat 可达性 + IsDir 检查，**不**强制 allowedRoot 内含；依赖
// "必须在工作根之内"的调用者必须额外调 workDirUnderRoot。
func workDirReachable(workDir string) bool {
	if workDir == "" {
		return true
	}
	info, err := os.Stat(workDir)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// workDirUnderRoot reports whether workDir resolves (after symlink evaluation)
// to a path at or under allowedRoot. EvalSymlinks runs per call for both sides
// so the check reflects current filesystem state, closing the TOCTOU between
// creation-time validateWorkspace and execute-time binding (and a retargeted
// allowedRoot symlink). Both arguments must be absolute. allowedRootResolved,
// when non-empty, is a fallback used only if EvalSymlinks(allowedRoot) fails.
func workDirUnderRoot(workDir, allowedRoot, allowedRootResolved string) bool {
	_, ok := workDirResolveUnderRoot(workDir, allowedRoot, allowedRootResolved)
	return ok
}

// workDirResolveUnderRoot is the variant of workDirUnderRoot that also returns
// the symlink-resolved workDir, so callers hand the CLI the same path that was
// validated (closing the EvalSymlinks TOCTOU window). Empty workDir / root
// returns ("", true) so the caller leaves opts.Workspace untouched.
//
// SHARED-ALGORITHM-WITH-SERVER (#1316): the EvalSymlinks → resolve-root →
// prefix algorithm lives in osutil.ResolveWorkspaceUnderRoot, shared with the
// server's validateWorkspace so a containment fix cannot drift between the two
// call sites; cron keeps the (resolved, ok) shape, the server maps sentinel
// errors onto HTTP status codes.
func workDirResolveUnderRoot(workDir, allowedRoot, allowedRootResolved string) (string, bool) {
	return osutil.ResolveWorkspaceUnderRoot(
		workDir, allowedRoot, allowedRootResolved, filepath.EvalSymlinks)
}
