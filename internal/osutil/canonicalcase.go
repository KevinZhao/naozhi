package osutil

import (
	"os"
	"path/filepath"
	"strings"
)

// CanonicalCase returns path with every existing component spelled with its
// on-disk case.
//
// Motivation (incident 2026-07-14): on case-insensitive filesystems (macOS
// APFS default, Windows) a config that says /Users/x/Workspace while the
// directory on disk is /Users/x/workspace silently forks two parallel
// identities for the same tree — Claude CLI project slugs
// (~/.claude/projects/-Users-x-Workspace-* vs -Users-x-workspace-*), kiro
// session cwd metadata, shim --cwd argv, and every string-equality check
// treat them as distinct even though all file operations succeed. Callers
// canonicalize once at the workspace-resolution choke point so a single
// spelling propagates everywhere downstream.
//
// Rules:
//   - Only absolute paths are canonicalized; "" and relative paths return
//     unchanged (callers resolve those first).
//   - Symlinks are NOT resolved — only the spelling of each component is
//     normalized. Orthogonal to filepath.EvalSymlinks (which, on
//     case-insensitive filesystems, keeps the caller's case anyway).
//   - An exact-case directory entry wins over a case-insensitive match, so
//     on case-sensitive filesystems (Linux) with sibling entries differing
//     only in case the input spelling is always preserved.
//   - From the first component that does not exist on disk (target not
//     created yet) the input spelling is kept verbatim for the remainder.
//   - On any I/O error (unreadable parent, permission denied) the input is
//     likewise kept from that point on — canonicalization must never turn a
//     working spawn path into a broken one.
//
// Cost: one os.ReadDir per existing component. Intended for low-frequency
// call sites (session spawn, config load) — do not put this on a hot path.
func CanonicalCase(path string) string {
	if path == "" || !filepath.IsAbs(path) {
		return path
	}
	clean := filepath.Clean(path)
	vol := filepath.VolumeName(clean) // "" on unix, "C:" on windows
	sep := string(filepath.Separator)
	rest := strings.TrimPrefix(clean[len(vol):], sep)
	if rest == "" {
		return clean // root ("/" or "C:\")
	}
	comps := strings.Split(rest, sep)
	cur := vol + sep
	for i, comp := range comps {
		fixed, ok := canonicalComponent(cur, comp)
		if !ok {
			// Missing or unreadable: keep the caller's spelling for this
			// component and everything after it.
			parts := append([]string{cur}, comps[i:]...)
			return filepath.Join(parts...)
		}
		cur = filepath.Join(cur, fixed)
	}
	return cur
}

// canonicalComponent returns the on-disk spelling of comp inside dir.
// ok=false when dir cannot be read or no entry matches case-insensitively.
// An exact-case match always wins over a folded match so case-sensitive
// filesystems with entries differing only in case keep the input spelling.
func canonicalComponent(dir, comp string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	folded := ""
	for _, e := range entries {
		name := e.Name()
		if name == comp {
			return comp, true
		}
		if folded == "" && strings.EqualFold(name, comp) {
			folded = name
		}
	}
	if folded != "" {
		return folded, true
	}
	return "", false
}
