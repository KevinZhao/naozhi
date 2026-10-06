//go:build unix

package osutil

import (
	"errors"
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
