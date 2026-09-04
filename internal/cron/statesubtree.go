package cron

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// stateSubtree resolves a sibling subtree of the cron store file
// (<store-dir>/<parts...>). Returns "" when persistence is disabled so every
// caller folds its storePath=="" early-return into this helper; all sandbox-
// state writers derive their paths here so the symlink guard has one chokepoint.
func (s *Scheduler) stateSubtree(parts ...string) string {
	if s.storePath == "" {
		return ""
	}
	return filepath.Join(append([]string{filepath.Dir(s.storePath)}, parts...)...)
}

// mkdirStateSubtree creates a state subtree (0700) under the cron store
// directory and refuses it if ANY component below the store dir is a symlink
// or non-directory (#2166): MkdirAll follows an existing symlink, and a
// final-component-only Lstat misses a symlinked ANCESTOR. Each level is
// created with a non-following os.Mkdir then Lstat'd before descending;
// Mkdir-then-Lstat (not Lstat-then-Mkdir) closes the TOCTOU window. The store
// dir itself is trusted (config-supplied), mirroring runstore's root guard.
func (s *Scheduler) mkdirStateSubtree(dir string) error {
	base := filepath.Dir(s.storePath)
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == ".." || rel == "." || filepath.IsAbs(rel) ||
		rel == "" || hasParentTraversal(rel) {
		// dir is not strictly below the store dir — refuse rather than guess.
		return &fs.PathError{Op: "mkdirStateSubtree", Path: dir, Err: fs.ErrInvalid}
	}
	cur := base
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
			slog.Error("cron sandbox: state subtree component is a symlink or non-directory; refusing to write through it",
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
