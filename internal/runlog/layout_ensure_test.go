package runlog

import (
	"os"
	"path/filepath"
	"testing"
)

// The three properties below moved here from internal/cron when cron adopted
// this layout (#2709). They are the layout's contract, not cron's, and cron's
// copies were deleted in the same change rather than left as a second water
// level: ensurejobdir_root_fsync_test.go's durable-subdir / retry-after-failure
// / pre-planted-symlink cases and ensurejobdir_recheck_symlink_test.go's
// recheck case (which is TestEnsureOwnerDir_RevalidatesAfterSwap next door).

// TestEnsureOwnerDir_CreatesSubdirThenTakesFastPath pins R249-ARCH-10 (#976).
// The fsync itself is not portably observable, so the observable half is: the
// first call creates the directory, and the second takes the marker fast path
// instead of paying MkdirAll + the root fsync again.
//
// The fast path is a syscall cache, and this test documents its one visible
// consequence honestly: a directory deleted behind the layout's back is NOT
// recreated until something drops the marker. Writes then fail and are counted,
// and the owner-lifecycle callers (cron's DeleteJob / dropOrphanRun,
// runhistory's Invalidate) call ForgetOwner, which is what clears it.
func TestEnsureOwnerDir_CreatesSubdirThenTakesFastPath(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "runs")
	l := newLayout(t, root)

	dir, err := l.EnsureOwnerDir("owner1")
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("first ensure must create the owner dir: %v", err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := l.EnsureOwnerDir("owner1"); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("second ensure re-created the dir; the marker fast path did not engage")
	}
}

// TestEnsureOwnerDir_FailureLeavesNoMarker: a transient MkdirAll failure must
// not poison the cache, or the owner is locked out of persistence for the
// process lifetime.
func TestEnsureOwnerDir_FailureLeavesNoMarker(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "runs")
	l := newLayout(t, root)

	// A regular file where the owner's directory belongs makes MkdirAll fail
	// with ENOTDIR — and is itself a non-directory the guard refuses first.
	blocked := filepath.Join(root, "owner1")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := l.EnsureOwnerDir("owner1"); err == nil {
		t.Fatal("a regular file in the owner's place must be refused")
	}

	// Clear the obstruction: a retry must now succeed, which it cannot do if
	// the failed attempt left a marker behind.
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	dir, err := l.EnsureOwnerDir("owner1")
	if err != nil {
		t.Fatalf("retry after a cleared failure: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Errorf("retry did not create the owner dir: %v", err)
	}
}

// TestEnsureOwnerDir_RejectsPrePlantedSymlink pins R250531-SEC-4 (#1504): a
// local attacker who pre-creates the owner directory as a symlink BEFORE the
// first write must not be able to redirect records there. MkdirAll silently
// succeeds on an existing symlink-to-directory, so the Lstat is the only thing
// standing in the way. (RevalidatesAfterSwap is the same guard on the second
// call; this one is the first.)
func TestEnsureOwnerDir_RejectsPrePlantedSymlink(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "runs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(root, "owner1")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	l := newLayout(t, root)
	if _, err := l.EnsureOwnerDir("owner1"); err == nil {
		t.Error("a pre-planted symlink must be refused on the FIRST call")
	}
	if err := l.WriteRecord("owner1", "rec1", []byte(`{}`)); err == nil {
		t.Error("WriteRecord must not write through a pre-planted symlink")
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Errorf("%d entries landed at the symlink target", len(ents))
	}
}
