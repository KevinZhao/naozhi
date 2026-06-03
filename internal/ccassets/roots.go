package ccassets

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/naozhi/naozhi/internal/assets"
)

// errPathEscape is returned when a Ref resolves outside its allowed root. It
// wraps assets.ErrNotFound so the handler maps it to 404 (don't leak whether
// a traversal target exists) — RFC §5.
var errPathEscape = fmt.Errorf("ccassets: path escapes allowed root: %w", assets.ErrNotFound)

// isUnderHome reports whether path is lexically under home (both are
// filepath.Clean'd). Returns false when home is empty. Used to guard plugin
// InstallPath values that arrive from user-writable JSON; prevents a crafted
// installPath="/" from escaping the home directory tree (R20260603-GO-2/3).
func isUnderHome(path, home string) bool {
	if home == "" || path == "" {
		return false
	}
	cleanHome := filepath.Clean(home)
	cleanPath := filepath.Clean(path)
	prefix := cleanHome + string(filepath.Separator)
	return strings.HasPrefix(cleanPath, prefix) || cleanPath == cleanHome
}

// projectDirRE locks the memory Source.Project segment to the alphabet Claude's
// project-dir encoder produces (leading "-", then alnum / "-"). Prevents a
// crafted Project value from carrying traversal into the memory root (§5).
var projectDirRE = regexp.MustCompile(`^-[A-Za-z0-9_-]+$`)

// skillRoot returns the absolute skills root for a given source kind, or ""
// if that source is unavailable (e.g. project source with empty RepoRoot).
//
// P0 scope: only user + project skill roots. Plugin / memory roots are added
// in later phases (RFC §7).
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
// resolution, that the result stays under root. Mirrors the ext/memory
// R242-SEC-7 double-check (RFC §5). The returned path is the symlink-resolved
// absolute path safe to read.
func resolveUnder(root, rel string) (string, error) {
	if root == "" {
		return "", errPathEscape
	}
	if strings.Contains(rel, "..") {
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

// rootForRef picks the single allowed root for a ReadRaw Ref and returns the
// rel path RELATIVE TO THAT ROOT (RFC §5). The root is always the deepest
// container we control (skills dir / plugin install dir / memory dir / home),
// never a parent — so a rel like "secret.txt" cannot reach a sibling, and
// resolveUnder additionally gates "..". The Asset.RelPath carries a display
// prefix that we strip so resolveUnder anchors at the right root.
func rootForRef(home, repoRoot string, ref assets.Ref) (root, rel string, err error) {
	switch ref.Source.Kind {
	case "user":
		// user-level skill/agent/command/mcp all live directly under home;
		// RelPath is already home-relative ("skills/x/SKILL.md", ".mcp.json").
		// Anchor at home and let resolveUnder gate traversal.
		root, rel = home, ref.RelPath

	case "project":
		// project assets under <repoRoot>; RelPath is repo-relative
		// (".claude/skills/x/SKILL.md").
		root, rel = repoRoot, ref.RelPath

	case "plugin":
		// plugin assets live under the plugin's installPath; resolve it from
		// the manifest so an uninstalled/unknown plugin is refused (§5).
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
		// memory under projects/<encoded>/memory/; the encoded segment must
		// match the alphabet so it can't carry traversal.
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
