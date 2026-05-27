package persist

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestTruncateFile_CreatesMissingFile locks in R20260527122801-CR-5 (#?):
// truncateFile must succeed against a missing path because
// reconcileIdxAheadOfLog can call us against a logfile that a concurrent
// rotate (or external operator action) deleted between our os.Stat and
// our open. Pre-fix the file was opened with os.O_WRONLY (no O_CREATE),
// so the missing-file branch returned ENOENT and Recover failed the
// whole Persister startup — a single torn rotate could brick the
// service across restart.
//
// The fix adds O_CREATE so a missing log/idx path is materialised as an
// empty 0o600 file (matching the per-shard mode set by Persister at
// initial file-creation time, so crash recovery does not silently widen
// permissions).
func TestTruncateFile_CreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "ghost.log")

	// Sanity: file does not exist yet.
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("precondition: expected ghost.log to not exist, got Stat err=%v", err)
	}

	if err := truncateFile(missing, 0); err != nil {
		t.Fatalf("truncateFile against missing path returned %v; want nil (O_CREATE missing?)", err)
	}

	fi, err := os.Stat(missing)
	if err != nil {
		t.Fatalf("post-truncate Stat: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("size = %d, want 0", fi.Size())
	}
	if fi.IsDir() {
		t.Errorf("created path is a directory; expected regular file")
	}
	// On Windows file mode bits are not faithfully reported via os.Stat,
	// so guard the mode assertion to POSIX-class platforms.
	if runtime.GOOS != "windows" {
		if got, want := fi.Mode().Perm(), os.FileMode(0o600); got != want {
			t.Errorf("mode = %v, want %v (recovery must not silently widen permissions)", got, want)
		}
	}
}
