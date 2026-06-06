package osutil

import (
	"os"
	"path/filepath"
	"strings"
)

// PathContainedInRoot reports whether resolved is the same path as root or
// lives somewhere beneath it. It is the single, shared implementation of the
// workspace / work_dir containment check that several trust boundaries rely
// on:
//
//   - internal/server/server_validate.go  validateWorkspace (dashboard resume / send)
//   - internal/cron/scheduler.go          workDirResolveUnderRoot (cron WorkDir)
//   - internal/dispatch/commands.go        the /cd command
//   - internal/server/agent_tailer_pathcheck.go jsonlPathUnderAllowedRoot
//     (NOTE: that caller requires root-itself to be *rejected*, so it keeps a
//     `path == root → false` short-circuit BEFORE calling this helper)
//   - internal/dashboard/cron/transcript.go the cron transcript path-escape gate
//
// CONTRACT: both arguments MUST already be filepath.EvalSymlinks-resolved
// (canonical) absolute paths. The inode-walk fallback below Stats resolved and
// its ancestors directly; if a caller passes an un-resolved path containing a
// symlink, the walk would Stat *through* that symlink and the containment
// answer would no longer reflect the post-resolution filesystem view. Every
// caller resolves both sides via EvalSymlinks before reaching here, which is
// also what makes the symlink-escape rejection hold: a symlink that points out
// of root resolves to its real out-of-root target, so neither the byte prefix
// nor the inode walk can map it back under root.
//
// Why the inode-walk fallback (R238-SEC-6): strings.HasPrefix is byte-wise and
// case-sensitive. On case-insensitive filesystems (macOS APFS/HFS+ default,
// Windows NTFS) EvalSymlinks preserves the user-typed case for components the
// kernel otherwise treats as equivalent — e.g. resolved="/Users/alice/work"
// vs root="/Users/Alice/Work" — so the byte prefix check would falsely reject
// a legitimate child. Falling back to an os.SameFile walk on resolved's
// ancestors matches actual filesystem containment semantics without baking in
// any OS-specific case-folding rules.
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
// any of its ancestors. Used as a fallback after a byte-wise HasPrefix check
// fails so the containment gate honours filesystem semantics on
// case-insensitive filesystems (macOS APFS/HFS+ default, Windows NTFS) where
// EvalSymlinks preserves user-typed case while the kernel still treats the
// path as equivalent. Walking parents one Stat at a time bounds the work to
// path depth and avoids os-specific case-folding rules. Returns false on any
// Stat error (root deleted mid-flight, permission denied, broken chain) so a
// failed ancestor probe never weakens the byte-wise gate's negative result.
func sameFileAncestor(resolved, root string) bool {
	// R103901-SEC-8: Lstat (not Stat) so a symlink at the final path
	// component is never followed when probing inode identity. Both args
	// arrive already EvalSymlinks-resolved (the package contract), so on the
	// normal path Lstat and Stat return identical inode info and SameFile
	// semantics are unchanged (including case-insensitive FS matching, which
	// folds in the kernel dir lookup, not the final-component follow). Lstat
	// closes the defence-in-depth gap where a crafted root/final-component
	// symlink could otherwise let SameFile match a target outside the subtree.
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
