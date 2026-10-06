//go:build unix

package osutil

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestOpenRegular_FIFODoesNotBlock pins O_NONBLOCK: opening a FIFO for reading
// blocks in open(2) until a writer appears, so without it the Fstat check
// never runs and the caller's goroutine parks forever. Wall-clock is the
// property under test; the timeout names the lost flag instead of hanging.
func TestOpenRegular_FIFODoesNotBlock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "wf.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := OpenRegular(path, 0)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Errorf("err = %v, want ErrNotRegular", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("OpenRegular blocked >5s on a FIFO — O_NONBLOCK lost")
		// Unpark the open so the goroutine does not outlive the test.
		if w, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
			w.Close()
		}
	}
}

// TestOpenRegular_RefusesSymlink pins O_NOFOLLOW: a final-component symlink to
// a regular file is not opened, so a path cannot be redirected after the
// caller vetted it.
func TestOpenRegular_RefusesSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "t.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	if f, _, err := OpenRegular(link, 0); err == nil {
		f.Close()
		t.Error("OpenRegular followed a final-component symlink")
	}
}

// TestOpenRegularIn_FIFODoesNotBlock pins that os.Root passes O_NONBLOCK to
// the final component: a FIFO planted as a result file or a run directory
// fails at once instead of parking the caller until a writer appears.
func TestOpenRegularIn_FIFODoesNotBlock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "wf.json"), 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	done := make(chan [2]error, 1)
	go func() {
		_, _, errFile := OpenRegularIn(root, "wf.json", 0)
		_, errDir := OpenDirIn(root, "wf.json")
		done <- [2]error{errFile, errDir}
	}()
	select {
	case errs := <-done:
		if !errors.Is(errs[0], ErrNotRegular) || !errors.Is(errs[1], ErrNotDir) {
			t.Errorf("errs = %v, want ErrNotRegular and ErrNotDir", errs)
		}
	case <-time.After(5 * time.Second):
		t.Error("OpenRegularIn / OpenDirIn blocked >5s on a FIFO")
		if w, err := os.OpenFile(filepath.Join(dir, "wf.json"), os.O_WRONLY, 0); err == nil {
			w.Close()
		}
	}
}

// TestOpenRegularIn_FIFOSwappedAfterLstat: the open itself must not block
// either. With the Lstat seam out of the way (a FIFO reached through the
// root's own OpenFile with OpenRegularIn's flags), the open returns.
func TestOpenRegularIn_FIFOSwappedAfterLstat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "wf.json"), 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	done := make(chan error, 1)
	go func() {
		f, err := root.OpenFile("wf.json", openInFlags, 0)
		if err == nil {
			_, _, err = checkRegular(f, 0)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Errorf("err = %v, want ErrNotRegular", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("os.Root.OpenFile blocked >5s on a FIFO: O_NONBLOCK did not reach the final component")
		if w, err := os.OpenFile(filepath.Join(dir, "wf.json"), os.O_WRONLY, 0); err == nil {
			w.Close()
		}
	}
}

// TestOpenRegularIn_RefusesSymlinks: a final-component symlink is refused
// even when it stays inside the root, and so is a middle directory swapped
// for a symlink out of it after the path was resolved.
func TestOpenRegularIn_RefusesSymlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run", "real.jsonl"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.jsonl", filepath.Join(dir, "run", "agent-a.jsonl")); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	if err := os.Symlink("run", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "wf.json"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "swapped")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if f, _, err := OpenRegularIn(root, filepath.Join("run", "agent-a.jsonl"), 0); err == nil {
		f.Close()
		t.Error("OpenRegularIn followed a final-component symlink")
	}
	if f, _, err := OpenRegularIn(root, filepath.Join("swapped", "wf.json"), 0); err == nil {
		f.Close()
		t.Error("OpenRegularIn left the root through a middle symlink")
	}
	if d, err := OpenDirIn(root, "alias"); err == nil {
		d.Close()
		t.Error("OpenDirIn followed a final-component symlink")
	}
	if d, err := OpenDirIn(root, "swapped"); err == nil {
		d.Close()
		t.Error("OpenDirIn left the root through a symlink")
	}
}

// TestOpenRegularIn_SwappedForSymlink: a regular file replaced by a
// symlink between the Lstat and the open is refused; the fd opened is not
// the file that was vetted.
func TestOpenRegularIn_SwappedForSymlink(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"wf.json", "other.json"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	afterLstatIn = func() {
		if err := os.Remove(filepath.Join(dir, "wf.json")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink("other.json", filepath.Join(dir, "wf.json")); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterLstatIn = func() {} })
	if f, _, err := OpenRegularIn(root, "wf.json", 0); !errors.Is(err, ErrNotRegular) {
		if f != nil {
			f.Close()
		}
		t.Errorf("err = %v, want ErrNotRegular for a file swapped after the Lstat", err)
	}
}

// TestOpenRegularIn_Socket: a socket is refused before any open, as
// ErrNotRegular like every other non-regular file.
func TestOpenRegularIn_Socket(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("/tmp", "orin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Skipf("unix socket unsupported here: %v", err)
	}
	defer ln.Close()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, _, err := OpenRegularIn(root, "s", 0); !errors.Is(err, ErrNotRegular) {
		t.Errorf("err = %v, want ErrNotRegular", err)
	}
}
