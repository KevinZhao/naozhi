package project

import (
	"path/filepath"
	"strings"
)

// workspaceScanPath returns the form of abs that the credential-segment scan
// (isSensitiveDownloadPath) should inspect: the path RELATIVE to the resolved
// workspace root. Scanning the absolute path matched segments above the
// workspace too, so a project checked out under e.g. ~/secrets/ or
// ~/.docker/ had every file hidden from files/list and 403'd on download
// (#2433). Segments below the root — `secrets/db.yaml`, `.ssh/known_hosts`
// — are still scanned in full, and abs is expected to be symlink-resolved so
// an in-workspace `pub -> secrets` alias is caught by its real path.
//
// Reach: Manager.Scan auto-registers every non-hidden subdirectory of the
// workspace root as a project, so a root named `secrets` / `credentials` is
// now browsable without any operator action; dot-prefixed roots (`.aws`,
// `.ssh`) are only reachable when an operator configures them explicitly.
//
// Falls back to abs when the relative form cannot be computed or would
// escape the root (different volume, empty root, prefix guard not yet run):
// the scan must never become weaker than the absolute-path behaviour.
func workspaceScanPath(rootResolved, abs string) string {
	if rootResolved == "" {
		return abs
	}
	rel, err := filepath.Rel(rootResolved, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return abs
	}
	return rel
}
