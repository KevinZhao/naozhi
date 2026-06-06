package osutil

import (
	"path/filepath"
	"strings"
)

// PathUnderRoot reports whether resolved is identical to rootResolved or is a
// path strictly under it (rootResolved + separator prefix). Both arguments are
// expected to be already symlink-resolved / cleaned absolute paths; this is the
// pure lexical containment check shared by every "is this path inside the
// sandbox root" decision in the codebase.
//
// SHARED-ALGORITHM (R20260527122801-ARCH-4 / #1316): cron's
// workDirResolveUnderRoot (internal/cron/scheduler.go) and the server's
// validateWorkspace (internal/server/server_validate.go) historically each
// open-coded the same `resolved == root || strings.HasPrefix(resolved,
// root+sep)` test. Both now route through this single helper so a fix to the
// containment rule (e.g. case-folding on case-insensitive FS, UNC handling)
// lands in one place instead of silently re-opening the symlink-swap escape on
// whichever copy was missed.
func PathUnderRoot(resolved, rootResolved string) bool {
	if resolved == rootResolved {
		return true
	}
	return strings.HasPrefix(resolved, rootResolved+string(filepath.Separator))
}

// ResolveWorkspaceUnderRoot is the canonical low-level workspace-vs-root check
// extracted under R20260527122801-ARCH-4 (#1316) so cron and the HTTP server no
// longer carry two copies of the same EvalSymlinks → resolve-root → prefix
// algorithm.
//
// Contract:
//   - workDir or allowedRoot empty → returns ("", true): "no constraint", the
//     caller leaves the workspace untouched (router default applies). This
//     mirrors cron's prior empty-input short-circuit; server callers that need
//     to treat empty workDir as invalid must check that BEFORE calling here.
//   - workDir not absolute → ("", false).
//   - workDir fails EvalSymlinks (missing dir / EACCES) → ("", false): refuse
//     rather than silently re-create the sandbox escape.
//   - allowedRoot fails EvalSymlinks → fall back to allowedRootResolved (the
//     construction-time cached resolution); if that is also empty, ("", false)
//     — comparing a symlink-resolved workDir against a raw root string opens a
//     TOCTOU/symlink escape window. R243-SEC-9 (#795).
//   - On success → (resolvedWorkDir, true) where resolvedWorkDir is the
//     EvalSymlinks-resolved (and therefore filepath.Clean'd) path.
//
// resolveSymlinks is injected so callers can share their own resolver / cache;
// production passes filepath.EvalSymlinks.
func ResolveWorkspaceUnderRoot(
	workDir, allowedRoot, allowedRootResolved string,
	resolveSymlinks func(string) (string, error),
) (string, bool) {
	if workDir == "" || allowedRoot == "" {
		return "", true
	}
	if !filepath.IsAbs(workDir) {
		return "", false
	}
	resolved, err := resolveSymlinks(workDir)
	if err != nil {
		return "", false
	}
	rootResolved, err := resolveSymlinks(allowedRoot)
	if err != nil {
		if allowedRootResolved == "" {
			return "", false
		}
		rootResolved = allowedRootResolved
	}
	// Containment via PathContainedInRoot (not the byte-wise PathUnderRoot):
	// the inode-walk fallback admits a legitimate child on a case-insensitive
	// fs (macOS APFS, Windows NTFS) where EvalSymlinks kept user-typed case.
	// Both args are already EvalSymlinks-resolved above (PathContainedInRoot's
	// input contract), so the symlink-escape rejection is preserved. This keeps
	// the cron WorkDir boundary in lockstep with server validateWorkspace /
	// dispatch /cd / the cron transcript gate, which all call PathContainedInRoot
	// directly.
	if !PathContainedInRoot(resolved, rootResolved) {
		return "", false
	}
	return resolved, true
}
