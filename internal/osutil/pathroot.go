package osutil

import (
	"os"
	"path/filepath"
	"strings"
)

// PathContainedInRoot reports whether resolved is root itself or lives
// beneath it. It is the single containment check behind validateWorkspace,
// cron WorkDir, the /cd command, the agent tailer path gate and the cron
// transcript gate (the tailer rejects root-itself before calling here).
//
// CONTRACT: both arguments MUST already be filepath.EvalSymlinks-resolved
// absolute paths; a symlink pointing out of root then resolves to its real
// target so neither the byte prefix nor the inode walk maps it back inside.
// The os.SameFile ancestor-walk fallback admits legitimate children on
// case-insensitive filesystems where EvalSymlinks kept user-typed case.
func PathContainedInRoot(resolved, root string) bool {
	if resolved == root {
		return true
	}
	if strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return true
	}
	return sameFileAncestor(resolved, root)
}

// sameFileAncestor reports whether root names the same inode as resolved or
// any of its ancestors — the case-insensitive-filesystem fallback after the
// byte-wise prefix check fails. Any Lstat error returns false so a failed
// probe never weakens the byte-wise gate's negative result.
func sameFileAncestor(resolved, root string) bool {
	// Lstat, not Stat: never follow a symlink at the final component when
	// probing inode identity, so a crafted final-component symlink cannot make
	// SameFile match a target outside the subtree. Args are already
	// EvalSymlinks-resolved, so on the normal path Lstat == Stat.
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return false
	}
	cur := filepath.Clean(resolved)
	for {
		info, err := os.Lstat(cur)
		if err == nil && os.SameFile(info, rootInfo) {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur { // reached filesystem root, stop.
			return false
		}
		cur = parent
	}
}
