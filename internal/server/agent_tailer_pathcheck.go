// ensureTailer 用的路径校验 pure helper：jsonlPathUnderAllowedRoot（allowed_root
// 边界 defence-in-depth）+ resolveExistingAncestor（symlink 对齐）。
package server

import (
	"path/filepath"
	"strings"
)

// jsonlPathUnderAllowedRoot returns true when jsonlPath is anchored under
// allowedRoot. Anchors on cleaned root + separator (a bare prefix check
// would let "/var/fooBar" match "/var/foo"). Both sides are EvalSymlinks'd
// first — allowedRoot may contain a symlinked component (macOS /var,
// bind-mounts) — and the not-yet-existing jsonlPath ("tail-before-write")
// is resolved via its nearest existing ancestor so the two sides compare
// in the same canonical form. Defence-in-depth, not a TOCTOU-safe gate.
func jsonlPathUnderAllowedRoot(jsonlPath, allowedRoot string) bool {
	abs := filepath.Clean(jsonlPath)
	if !filepath.IsAbs(abs) {
		return false
	}
	root := filepath.Clean(allowedRoot)
	rootResolved := false
	if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil {
		root = resolvedRoot
		rootResolved = true
	}
	abs, absResolved := resolveExistingAncestor(abs)
	// A lexical HasPrefix between a resolved root and an unresolved abs (or
	// vice versa) is unsound — a symlinked root component could make a path
	// outside the root match. Only compare when both sides are in the same
	// form; otherwise reject (#1533).
	if absResolved != rootResolved {
		return false
	}
	if abs == root {
		return false
	}
	prefix := root + string(filepath.Separator)
	return strings.HasPrefix(abs, prefix)
}

// resolveExistingAncestor returns the input symlink-resolved as far as the
// filesystem permits: if the leaf doesn't exist, walks parents until one
// does, EvalSymlinks that, then re-joins the unresolved tail.
//
// Returns (path, resolved); resolved=false (cleaned input) when no ancestor
// resolved, so the caller can detect the asymmetric-canonicalisation hazard
// (#1533). The walk is bounded by maxAncestorDepth so a pathologically deep
// path cannot drive O(depth) EvalSymlinks syscalls; hitting the cap yields
// the same (abs, false) verdict as reaching the filesystem root (#1891).
func resolveExistingAncestor(abs string) (string, bool) {
	const maxAncestorDepth = 64
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, true
	}
	parent := abs
	tail := ""
	for depth := 0; depth < maxAncestorDepth; depth++ {
		next := filepath.Dir(parent)
		if next == parent {
			// Hit filesystem root without an existing ancestor.
			return abs, false
		}
		base := filepath.Base(parent)
		if tail == "" {
			tail = base
		} else {
			tail = filepath.Join(base, tail)
		}
		parent = next
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(resolved, tail), true
		}
	}
	// Depth cap hit; same verdict as reaching the filesystem root.
	return abs, false
}
