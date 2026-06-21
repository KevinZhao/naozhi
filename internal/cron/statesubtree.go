package cron

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// stateSubtree resolves a sibling subtree of the cron store file
// (<store-dir>/<parts...>). Returns "" when persistence is disabled
// (store-less test fixtures skip the §6.x state machinery entirely), so
// every caller can fold its "if s.storePath == ”" early-return into this
// one helper.
//
// #2175 (DRY): five sandbox-state writers (sandboxpending, sandboxattention,
// sandboxevents/<jobID>, runsnapshots, runsnapshots/blobs) previously
// open-coded filepath.Join(filepath.Dir(s.storePath), …). Centralising the
// derivation keeps them from drifting and gives the symlink guard (#2166) a
// single MkdirAll chokepoint to wrap.
func (s *Scheduler) stateSubtree(parts ...string) string {
	if s.storePath == "" {
		return ""
	}
	return filepath.Join(append([]string{filepath.Dir(s.storePath)}, parts...)...)
}

// mkdirStateSubtree creates a state subtree (0700) under the cron store
// directory and refuses it if ANY component below the store dir resolved to a
// symlink or a non-directory.
//
// #2166: MkdirAll silently follows an existing symlink, so a planted
// `<stateDir>/<subtree> → /elsewhere` would redirect every sandbox-state write
// (pending reconcile handles, attention queue, event logs, replay snapshots)
// into an attacker-chosen directory. A plain MkdirAll-then-Lstat only validates
// the FINAL component, so a symlinked ANCESTOR (e.g. `sandboxevents` →
// /elsewhere, with the per-job leaf created inside the target) slips through.
// We instead create each level below the trusted store dir one segment at a
// time with a non-following os.Mkdir, then Lstat that exact segment before
// descending: Mkdir-then-Lstat (NOT Lstat-then-Mkdir) closes the TOCTOU window,
// and Lstat does not follow the segment, so a symlink surfaces as
// fs.ModeSymlink and we bail. The store dir itself is trusted (guarded at
// store init / supplied by config), mirroring the runs/ root + per-job guard
// precedent in runstore.go (newRunStore + ensureJobDir). Single-operator hosts
// are low risk; multi-tenant deployments are not.
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
