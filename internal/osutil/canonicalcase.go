package osutil

import (
	"os"
	"path/filepath"
	"strings"
)

// CanonicalCase returns path with every existing component spelled with its
// on-disk case. On case-insensitive filesystems (macOS APFS, Windows) a
// differently-cased spelling of the same tree otherwise forks parallel
// identities (Claude project slugs, shim --cwd, string-equality checks), so
// callers canonicalize once at the workspace-resolution choke point.
//
// Only absolute paths are touched; symlinks are not resolved; an exact-case
// entry wins over a folded match; from the first missing or unreadable
// component onward the input spelling is kept verbatim. Costs one os.ReadDir
// per existing component — not for hot paths.
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
