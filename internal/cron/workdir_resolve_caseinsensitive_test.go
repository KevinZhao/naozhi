package cron

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWorkDirResolveUnderRoot_CaseInsensitiveChild is the cron counterpart of
// the dashboard resume bug (see internal/server validate_workspace_test.go and
// osutil.PathContainedInRoot): on a case-insensitive filesystem (macOS APFS,
// Windows NTFS) EvalSymlinks preserves the caller's case, so a WorkDir that
// differs from allowedRoot only by case used to fail the byte-wise prefix
// check and refuse to run. The shared inode-walk fallback now admits it.
//
// Detects case-insensitivity at runtime via a Stat of a case variant rather
// than keying on runtime.GOOS, then Skips on a case-sensitive fs where the
// bug cannot be reproduced. Also asserts the returned path is the
// EvalSymlinks-resolved form, guarding that the fix only changed the
// containment decision and not the resolved value cron hands to the CLI.
func TestWorkDirResolveUnderRoot_CaseInsensitiveChild(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	child := filepath.Join(root, "Proj")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	lowerChild := filepath.Join(root, "proj")
	if _, err := os.Stat(lowerChild); err != nil {
		t.Skip("filesystem is case-sensitive; case-fold containment not exercisable here")
	}
	// allowedRoot in mixed case, workDir handed in the lowercase spelling.
	resolved, ok := workDirResolveUnderRoot(lowerChild, child, child)
	if !ok {
		t.Fatalf("case-variant workDir must be accepted on case-insensitive fs")
	}
	want, err := filepath.EvalSymlinks(lowerChild)
	if err != nil {
		t.Fatalf("EvalSymlinks(lowerChild): %v", err)
	}
	if resolved != want {
		t.Errorf("resolved = %q, want EvalSymlinks form %q", resolved, want)
	}
}

// TestWorkDirResolveUnderRoot_SymlinkEscape locks down that the inode-walk
// fallback did NOT weaken the escape gate: a symlink inside allowedRoot that
// points out of the root resolves (EvalSymlinks) to its real out-of-root
// target, which neither the byte prefix nor the inode walk can map back under
// root. Mirrors server's TestValidateWorkspace_SymlinkEscape on the cron side.
func TestWorkDirResolveUnderRoot_SymlinkEscape(t *testing.T) {
	t.Parallel()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	root := filepath.Join(tmp, "root")
	outside := filepath.Join(tmp, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	trap := filepath.Join(root, "escape")
	if err := os.Symlink(outside, trap); err != nil {
		t.Fatal(err)
	}
	if _, ok := workDirResolveUnderRoot(trap, root, root); ok {
		t.Fatalf("symlink escape out of root must be rejected")
	}
}
