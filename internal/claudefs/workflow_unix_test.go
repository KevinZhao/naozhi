//go:build unix

package claudefs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestLocateWorkflowRun_FIFODoesNotBlock: a session's subagents/workflows
// swapped for a FIFO fails the directory open at once instead of parking
// the scan until a writer appears.
func TestLocateWorkflowRun_FIFODoesNotBlock(t *testing.T) {
	t.Parallel()
	l := newWFLayout(t)
	runs := filepath.Dir(l.runDir)
	if err := os.RemoveAll(runs); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(runs, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	done := make(chan string, 1)
	go func() {
		got, _ := LocateWorkflowRun(l.root, []string{l.projectDir}, wfTestSID, []string{wfTestAgent})
		done <- got
	}()
	select {
	case got := <-done:
		if got != "" {
			t.Errorf("located %q through a FIFO", got)
		}
	case <-time.After(5 * time.Second):
		t.Error("LocateWorkflowRun blocked >5s on a FIFO runs dir")
		if w, err := os.OpenFile(runs, os.O_WRONLY, 0); err == nil {
			w.Close()
		}
	}
}
