package osutil

import (
	"errors"
	"io/fs"
	"path/filepath"
)

// stateDirWalkFileBudget caps how many entries StateDirSize will visit before
// aborting. A healthy naozhi state dir holds far fewer; hitting this cap
// surfaces the partial total alongside ErrStateDirScanTruncated.
const stateDirWalkFileBudget = 50000

// ErrStateDirScanTruncated signals the walk exceeded stateDirWalkFileBudget.
// The returned size is a lower bound.
var ErrStateDirScanTruncated = errors.New("state dir scan truncated at file budget")

// StateDirSize walks path and returns total bytes of regular files beneath it.
// Per-entry permission errors are skipped; a missing root propagates so callers
// can silently skip the warn on first-run systems.
func StateDirSize(path string) (int64, error) {
	return stateDirSizeWithBudget(path, stateDirWalkFileBudget)
}

// stateDirSizeWithBudget is StateDirSize with the entry cap as a parameter, so the
// truncation path can be tested without materialising 50,050 files. That fixture
// took 18s of the ~200s CI `test` job whose Epic A #2524 target is 150s, and it
// tested the same branch a 150-file budget reaches.
//
// The budget stays a const for production: StateDirSize is the only exported entry
// point and it passes it, so no caller can widen or narrow the cap at runtime.
func stateDirSizeWithBudget(path string, budget int) (int64, error) {
	var total int64
	var seen int
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if d == nil {
				return err // root missing / unreadable — propagate
			}
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		seen++
		if seen > budget {
			return ErrStateDirScanTruncated
		}
		if d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
