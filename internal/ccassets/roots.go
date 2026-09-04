package ccassets

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/naozhi/naozhi/internal/assets"
)

// errPathEscape wraps assets.ErrNotFound so the handler maps a traversal
// attempt to 404 and does not leak whether the target exists.
var errPathEscape = fmt.Errorf("ccassets: path escapes allowed root: %w", assets.ErrNotFound)

// isUnderHome reports whether path is lexically under home (false when home
// is empty). Guards plugin InstallPath values from user-writable JSON.
func isUnderHome(path, home string) bool {
	if home == "" || path == "" {
		return false
	}
	cleanHome := filepath.Clean(home)
	cleanPath := filepath.Clean(path)
	prefix := cleanHome + string(filepath.Separator)
	return strings.HasPrefix(cleanPath, prefix) || cleanPath == cleanHome
}

// projectDirRE locks Source.Project to the project-dir encoder's alphabet (no traversal).
var projectDirRE = regexp.MustCompile(`^-[A-Za-z0-9_-]+$`)

// skillRoot returns the skills root for a source kind, or "" if unavailable.
func skillRoot(home, repoRoot, sourceKind string) string {
	switch sourceKind {
	case "user":
		if home == "" {
			return ""
		}
		return filepath.Join(home, "skills")
	case "project":
		if repoRoot == "" {
			return ""
		}
		return filepath.Join(repoRoot, ".claude", "skills")
	default:
		return ""
	}
}

// resolveUnder joins root+rel and verifies, both lexically and after symlink
// resolution, that the result stays under root; returns the resolved path.
func resolveUnder(root, rel string) (string, error) {
	if root == "" {
		return "", errPathEscape
	}
	if strings.Contains(rel, "..") {
		return "", errPathEscape
	}
	// rel is user-controlled: reject NUL and absolute paths (an absolute rel
	// joined with root silently discards root on some platforms). (#2250)
	if strings.ContainsRune(rel, 0) || filepath.IsAbs(rel) {
		return "", errPathEscape
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	prefixNoSep := strings.TrimRight(filepath.Clean(resolvedRoot), string(filepath.Separator))
	prefix := prefixNoSep + string(filepath.Separator)

	clean := filepath.Clean(filepath.Join(resolvedRoot, rel))
	if !strings.HasPrefix(clean, prefix) && clean != prefixNoSep {
		return "", errPathEscape
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(resolved, prefix) && resolved != prefixNoSep {
		return "", errPathEscape
	}
	return resolved, nil
}

// rootForRef picks the single allowed root for a ReadRaw Ref and the rel path
// RELATIVE TO THAT ROOT. The root is always the deepest container we control
// (plugin install dir / memory dir / home), never a parent, so a rel cannot
// reach a sibling; resolveUnder additionally gates "..".
func rootForRef(home, repoRoot string, ref assets.Ref) (root, rel string, err error) {
	switch ref.Source.Kind {
	case "user":
		root, rel = home, ref.RelPath

	case "project":
		root, rel = repoRoot, ref.RelPath

	case "plugin":
		// Resolve installPath from the manifest so an unknown plugin is refused.
		ip, e := readInstalledPlugins(home)
		if e != nil || ip == nil {
			return "", "", errPathEscape
		}
		recs := ip.Plugins[ref.Source.Plugin]
		if len(recs) == 0 || recs[0].InstallPath == "" {
			return "", "", errPathEscape
		}
		installPath := recs[0].InstallPath
		if !isUnderHome(installPath, home) {
			return "", "", errPathEscape
		}
		root, rel = installPath, ref.RelPath

	case "memory_project":
		// The encoded project segment must match the alphabet (no traversal).
		if home == "" || !projectDirRE.MatchString(ref.Source.Project) {
			return "", "", errPathEscape
		}
		root = filepath.Join(home, "projects", ref.Source.Project, "memory")
		rel = ref.RelPath

	default:
		return "", "", errPathEscape
	}

	if root == "" || rel == "" {
		return "", "", errPathEscape
	}
	return root, rel, nil
}
