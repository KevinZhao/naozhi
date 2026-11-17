//go:build unix

package jsonfile

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestLoadRefusesFIFOWithoutBlocking: a FIFO at the path must be rejected by the
// Fstat check, not wedge the caller in open(2) waiting for a writer. Callers
// such as the attachment tracker run Load on a single worker goroutine, and ctx
// cannot interrupt a blocked open.
func TestLoadRefusesFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.meta")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := Load[payload](path, opts(1<<20))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Errorf("err = %v, want a not-a-regular-file error", err)
		}
	case <-time.After(2 * time.Second):
		// Opening the write end releases the blocked reader so the goroutine
		// does not outlive the test.
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
		<-done
		t.Fatal("Load blocked in open(2) on a FIFO")
	}
}
