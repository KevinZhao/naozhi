package testhelper

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SkipRepoDir reports whether a walk of the repository at root should skip
// directory d at path: dot directories (.git, agent worktrees under .claude,
// caches), node_modules, testdata, vendor, the old clone at <root>/naozhi,
// and any nested checkout, meaning a directory holding a .git entry. Root
// itself is never skipped, so a root given as "." or "../.." still walks.
func SkipRepoDir(root, path string, d fs.DirEntry) bool {
	if path == root {
		return false
	}
	switch name := d.Name(); {
	case strings.HasPrefix(name, "."), name == "node_modules", name == "testdata", name == "vendor":
		return true
	case path == filepath.Join(root, "naozhi"):
		return true
	}
	_, err := os.Lstat(filepath.Join(path, ".git"))
	return err == nil
}
