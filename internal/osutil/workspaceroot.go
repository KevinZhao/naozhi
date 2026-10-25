package osutil

import (
	"path/filepath"
)

// ResolveWorkspaceUnderRoot is the shared EvalSymlinks → resolve-root →
// containment check for cron and the HTTP server (#1316). resolveSymlinks is
// injected so callers can share a resolver / cache.
//
//   - workDir or allowedRoot empty → ("", true): no constraint.
//   - workDir not absolute, or failing EvalSymlinks → ("", false).
//   - allowedRoot failing EvalSymlinks falls back to allowedRootResolved (the
//     cached construction-time resolution); if that is also empty → ("", false),
//     since comparing against a raw root string opens a symlink escape (#795).
//   - Success → (EvalSymlinks-resolved workDir, true).
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
	// PathContainedInRoot (not a byte-wise prefix check): its inode-walk
	// fallback admits a legitimate child on a case-insensitive fs. Both args
	// are EvalSymlinks-resolved above, so symlink-escape rejection holds and
	// the cron WorkDir boundary matches the server / dispatch / transcript gates.
	if !PathContainedInRoot(resolved, rootResolved) {
		return "", false
	}
	return resolved, true
}
