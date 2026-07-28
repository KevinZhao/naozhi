//go:build !windows

package gitinfo

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestDetect_FifoHeadDoesNotBlock is a regression test for a real defect found
// in review: readFileCapped originally used os.Open, and opening a FIFO for
// reading blocks inside open(2) until a writer appears. A `.git/HEAD` fifo in
// an operator workspace therefore pinned the dashboard handler goroutine
// forever — the fstat IsRegular guard could never run, because open never
// returned. openGitMeta's O_NONBLOCK is what makes the guard reachable.
//
// The test asserts on wall-clock because "does not block" is precisely the
// property under test; a regression fails via the timeout rather than hanging
// the suite (go test's own panic-on-timeout would also catch it, but far later
// and without naming the cause).
func TestDetect_FifoHeadDoesNotBlock(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(gitDir, "HEAD"), 0o644); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	done := make(chan bool, 1)
	go func() {
		_, ok := Detect(root, "")
		done <- ok
	}()

	select {
	case ok := <-done:
		if ok {
			t.Error("Detect reported a valid repo for a fifo HEAD, want not-a-repo")
		}
	case <-time.After(5 * time.Second):
		// Deliberately not t.Fatal: the goroutine is still parked in open(2)
		// and Fatal from the test goroutine is fine, but we want the message to
		// name the flag whose loss caused it.
		t.Error("Detect blocked >5s on a fifo HEAD — openGitMeta lost O_NONBLOCK")
	}
}

// TestDetect_SymlinkedHeadRejected pins openGitMeta's O_NOFOLLOW: a HEAD that
// is a symlink out of the git dir must not be read, so a workspace cannot use
// it to make the endpoint report the contents of an arbitrary file.
func TestDetect_SymlinkedHeadRejected(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	target := filepath.Join(base, "elsewhere")
	if err := os.WriteFile(target, []byte("ref: refs/heads/leaked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "repo")
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(gitDir, "HEAD")); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	st, ok := Detect(root, "")
	if ok {
		t.Errorf("ok=true through a symlinked HEAD (branch=%q), want rejection", st.Branch)
	}
}
