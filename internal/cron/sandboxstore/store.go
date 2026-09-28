// Package sandboxstore is the on-disk half of cron's sandbox placement: the
// confirmation queue (sandboxattention/), the run-input snapshots
// (runsnapshots/) and the per-run event logs (sandboxevents/). It owns the
// paths, the file formats and every read and write of them. Deciding when to
// write, and what a record means for a run, stays in cron.
//
// Layout under the cron state directory:
//
//	sandboxattention/<runID>.json
//	runsnapshots/<jobID>/<runID>.json
//	runsnapshots/blobs/<sha256>
//	sandboxevents/<jobID>/<runID>.ndjson
//
// The package must not import internal/cron.
package sandboxstore

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/naozhi/naozhi/internal/datadir"
)

// Store reads and writes the sandbox state subtrees under Root. The zero value
// (Root "") has persistence disabled: writers do nothing and readers report
// "absent", which is what store-less schedulers and test fixtures rely on.
type Store struct {
	// Root is the cron state directory, the directory holding the cron store
	// file.
	Root string
}

// ErrInvalidID reports a job or run id that is not the scheduler's hex shape.
// Ids become file names, so anything else is refused before any disk IO.
var ErrInvalidID = errors.New("cron sandbox: invalid attention run id")

// validID reports whether s is a scheduler id: 1–64 lowercase hex chars, the
// shape cron.IsValidID accepts. cron's TestSandboxStore_IDShapeMatchesCron pins
// the two to the same answers.
func validID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// Subtree resolves a path below Root. Returns "" when persistence is disabled,
// so callers fold that check into the path; every state writer derives its
// path here so the symlink guard has one chokepoint.
func (st Store) Subtree(parts ...string) string {
	return datadir.FromRoot(st.Root).Join(parts...)
}

// MkdirSubtree creates dir (0700) below Root and refuses it if ANY component
// below Root is a symlink or non-directory (#2166): MkdirAll follows an
// existing symlink, and a final-component-only Lstat misses a symlinked
// ANCESTOR. Each level is created with a non-following os.Mkdir then Lstat'd
// before descending; Mkdir-then-Lstat (not Lstat-then-Mkdir) closes the TOCTOU
// window. Root itself is trusted (config-supplied), mirroring runstore's root
// guard.
func (st Store) MkdirSubtree(dir string) error {
	base := st.Root
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == ".." || rel == "." || filepath.IsAbs(rel) ||
		rel == "" || hasParentTraversal(rel) {
		// dir is not strictly below Root — refuse rather than guess.
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
