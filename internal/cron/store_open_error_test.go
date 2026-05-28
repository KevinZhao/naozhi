//go:build !windows

package cron

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestR241SEC9_LoadJobsHardAbortsOnNonNotExistOpenError pins the #469
// contract: any non-ErrNotExist open failure aborts loadJobs with an
// error rather than continuing to a (nil, nil) "empty jobs" result.
//
// Pre-fix history: the original loadJobs ran Lstat then os.Open and only
// emitted slog.Warn on a non-ErrNotExist Lstat failure before continuing
// to Open — leaving a symlink-bypass window when the kernel returned
// pseudo-EBUSY (FUSE) or similar transient errors that could clear by
// the time Open ran. The current shape is OpenFile(O_NOFOLLOW) with
// hard-abort on any non-ErrNotExist error.
//
// Test approach: create a directory with mode 0 (unreadable). On Linux
// that yields EACCES from OpenFile, which is neither ErrNotExist nor
// ELOOP — exactly the path R241-SEC-9 is about. A correct implementation
// must propagate the error; the previous implementation's "warn and
// continue" shape would have eaten it and returned (nil, nil) so the
// next persist would clobber the real cron_jobs.json with `[]`.
func TestR241SEC9_LoadJobsHardAbortsOnNonNotExistOpenError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory mode 0 does not yield EACCES")
	}
	t.Parallel()
	dir := t.TempDir()
	// Build a path inside an unreadable parent so Open fails with EACCES,
	// not ErrNotExist. We chmod the parent to 0 after creating it so the
	// test cleanup (t.TempDir) can still recurse via the scheduler-side
	// chmod-back at the end.
	parent := filepath.Join(dir, "locked")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("Mkdir parent: %v", err)
	}
	path := filepath.Join(parent, "cron_jobs.json")
	if err := os.WriteFile(path, []byte(`[]`), 0o600); err != nil {
		t.Fatalf("WriteFile path: %v", err)
	}
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatalf("Chmod parent 0: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	m, err := loadJobs(path)
	if err == nil {
		t.Fatalf("expected non-NotExist open error to be propagated, got nil err with map=%v (#469: silent fall-through would let next persist clobber real store)", m)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("loadJobs returned ErrNotExist when the file exists in an unreadable dir; this is the silent fall-through bug R241-SEC-9 fixes (err=%v)", err)
	}
	if !strings.Contains(err.Error(), "open cron store") {
		t.Errorf("expected error to mention \"open cron store\" wrap, got %v", err)
	}
	if m != nil {
		t.Errorf("expected nil map on hard-abort, got %d entries", len(m))
	}
}
