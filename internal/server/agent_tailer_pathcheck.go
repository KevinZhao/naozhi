// Phase 4c-prep / R-tailer-pathcheck-extract (2026-05-28):
// agent_tailer.go 中两个路径检验 pure func 抽到独立文件：
//
//   - jsonlPathUnderAllowedRoot — defence-in-depth 边界检查（R232-SEC-14）
//   - resolveExistingAncestor   — symlink 对齐辅助（R260528-SEC-3 followup）
//
// 这两个函数与 agentTailer / tailerRegistry 没有 receiver 关系，是天然
// 独立的路径校验 helper；只在 ensureTailer 一处调用。同包可见性让
// caller 无需任何改动。
package server

import (
	"path/filepath"
	"strings"
)

// jsonlPathUnderAllowedRoot returns true when jsonlPath is anchored under
// allowedRoot. Pure prefix check is unsafe ("/var/foo" prefix-matches
// "/var/fooBar"), so anchor on the cleaned root + os.PathSeparator. This
// guard is defence-in-depth (ensureTailer's caller already operates on
// CLI-emitted paths under the workspace), not a TOCTOU-safe gate.
// R232-SEC-14.
//
// R260528-SEC-3: also EvalSymlinks both sides before the prefix check to
// align with the dashboard_cron_transcript handler's stricter pattern.
// macOS canonicalises /var → /private/var, and any host where allowedRoot
// contains a symlinked component (Docker bind-mounts, AMI-customised
// layouts) drifts under EvalSymlinks; without the symmetric resolve the
// prefix check would reject every legitimate path on those hosts.
//
// R260528-SEC-3 followup (PR #1383 review): EvalSymlinks fails on paths
// that don't yet exist (the "tail-before-write" case where the CLI emits
// a JSONLPath ahead of the first write hitting disk). If only one side
// resolves — typically the allowedRoot does, the not-yet-existing
// jsonlPath does not — the asymmetric resolve flips a legitimate path
// into a HasPrefix mismatch. Walk up jsonlPath's parents to find the
// nearest existing ancestor, EvalSymlinks that, then re-join the
// unresolved tail. Both sides end up in the same canonical-form world
// and the prefix check holds whether or not the leaf has been created.
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
	// R20260531070014-SEC-6 (#1533): if NO ancestor of abs resolved, abs is
	// still the raw lexical input while root may have been canonicalised by
	// EvalSymlinks. A lexical HasPrefix between a resolved root and an
	// unresolved abs is unsound — a symlinked component on the root side can
	// make a path that is NOT actually under the allowed root match the
	// prefix (the 'tail-before-write' TOCTOU window for agent transcript
	// paths). Only the symmetric case is safe:
	//   - both resolved        → both canonical, prefix check sound
	//   - both unresolved       → both lexical, prefix check sound
	// When only one side resolved we cannot compare them, so reject. This is
	// defence-in-depth on CLI-emitted workspace paths, so rejecting the rare
	// "allowedRoot has a symlink AND the entire tail mountpoint is absent"
	// case is acceptable — such a path cannot correspond to a real file
	// under the resolved root anyway.
	if absResolved != rootResolved {
		return false
	}
	if abs == root {
		return false
	}
	prefix := root + string(filepath.Separator)
	return strings.HasPrefix(abs, prefix)
}

// resolveExistingAncestor returns the input cleaned + symlink-resolved as
// far as the filesystem permits. If the leaf doesn't exist, walks parents
// until one does, EvalSymlinks that, then re-joins the unresolved tail.
// This keeps "path not yet materialised" callers (CLI emitting a
// JSONLPath before the first write lands) on the same canonical-form
// surface as the resolved allowedRoot, so a symlink anywhere on the
// allowedRoot side cannot produce a one-sided canonicalisation that
// flips a legitimate jsonlPath into a HasPrefix mismatch.
//
// Returns (path, resolved). resolved reports whether ANY ancestor (the
// leaf itself or a parent up the chain) was successfully EvalSymlinks'd.
// When no ancestor resolves (constructed paths under a non-existent
// mountpoint, etc.) it returns the cleaned input with resolved=false so the
// caller can detect the asymmetric-canonicalisation hazard rather than
// silently falling back to an unsound lexical prefix check against a
// resolved root. R20260531070014-SEC-6 (#1533).
func resolveExistingAncestor(abs string) (string, bool) {
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, true
	}
	parent := abs
	tail := ""
	for {
		next := filepath.Dir(parent)
		if next == parent {
			// Hit filesystem root without finding an existing ancestor;
			// give up and return the original cleaned value, flagged
			// unresolved so the caller rejects the asymmetric compare.
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
}
