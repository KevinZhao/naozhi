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
	if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil {
		root = resolvedRoot
	}
	abs = resolveExistingAncestor(abs)
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
// Falls through to the cleaned input when no ancestor resolves
// (constructed paths under a non-existent mountpoint, etc.) so the
// historical lexical HasPrefix behaviour is preserved as a last resort.
func resolveExistingAncestor(abs string) string {
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	parent := abs
	tail := ""
	for {
		next := filepath.Dir(parent)
		if next == parent {
			// Hit filesystem root without finding an existing ancestor;
			// give up and return the original cleaned value.
			return abs
		}
		base := filepath.Base(parent)
		if tail == "" {
			tail = base
		} else {
			tail = filepath.Join(base, tail)
		}
		parent = next
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(resolved, tail)
		}
	}
}
