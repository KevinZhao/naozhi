package runstore

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// mkdirSubtree creates dir (0700) below root and refuses it if ANY component
// below root is a symlink or non-directory (#2166): MkdirAll follows an
// existing symlink, and a final-component-only Lstat misses a symlinked
// ANCESTOR. Each level is created with a non-following os.Mkdir then Lstat'd
// before descending; Mkdir-then-Lstat (not Lstat-then-Mkdir) closes the TOCTOU
// window. root itself is trusted (config-supplied), mirroring Store's root
// guard.
func mkdirSubtree(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || rel == "." || filepath.IsAbs(rel) ||
		rel == "" || hasParentTraversal(rel) {
		// dir is not strictly below root — refuse rather than guess.
		return &fs.PathError{Op: "mkdirStateSubtree", Path: dir, Err: fs.ErrInvalid}
	}
	cur := root
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == "" {
			continue
		}
		cur = filepath.Join(cur, seg)
		if err := os.Mkdir(cur, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
		fi, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			slog.Error("cron: state subtree component is a symlink or non-directory; refusing to write through it",
				"dir", dir, "component", cur, "mode", fi.Mode().String())
			return &fs.PathError{Op: "mkdirStateSubtree", Path: cur, Err: fs.ErrInvalid}
		}
	}
	return nil
}

// hasParentTraversal reports whether rel contains a ".." segment.
func hasParentTraversal(rel string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}
