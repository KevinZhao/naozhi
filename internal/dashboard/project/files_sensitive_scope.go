package project

import (
	"path/filepath"
	"strings"
)

// workspaceScanPath returns the path the credential-segment scan
// (isSensitiveDownloadPath) should inspect: abs RELATIVE to the resolved
// workspace root. Scanning the absolute path also matched segments above the
// workspace, so a project under ~/secrets/ or ~/.docker/ had every file hidden
// and 403'd (#2433); segments below the root are still scanned in full, and
// abs is expected to be symlink-resolved so an in-workspace `pub -> secrets`
// alias is caught. Falls back to abs when the relative form cannot be
// computed or would escape the root, so the scan never becomes weaker than
// the absolute-path behaviour.
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
