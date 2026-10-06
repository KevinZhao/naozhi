//go:build unix

package session

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// TestWorkflowBoard_RealDiskResult: on a real projects root a result file
// that appears after the board's backfill is served by Result; the same
// file swapped for a FIFO fails at once instead of parking the caller.
func TestWorkflowBoard_RealDiskResult(t *testing.T) {
	root, ws, resultPath := realRunLayout(t)
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(resultPath); err != nil {
		t.Fatal(err)
	}
	r := newWFRig(t)
	r.b.projectsRoot, r.b.disk = root, workflowDiskFS
	r.b.restore("k", []workflow.Ref{{TaskID: probeTask, RunID: wfRun, SessionID: wfSID, Status: workflow.StatusCompleted, EndedAt: wfT0}}, ws, r.now())
	testhelper.Eventually(t, func() bool { return r.entry(t, probeTask).RunDir != "" }, 5*time.Second, "run dir never resolved")
	r.settleIO(t)

	if err := syscall.Mkfifo(resultPath, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	t.Cleanup(func() {
		if w, err := os.OpenFile(resultPath, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
	})
	done := make(chan ResultStatus, 1)
	go func() {
		_, st := r.b.Result(context.Background(), probeTask)
		done <- st
	}()
	select {
	case st := <-done:
		if st != ResultUnavailable {
			t.Errorf("FIFO result file: %v, want ResultUnavailable", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Result parked on a FIFO")
	}

	if err := os.Remove(resultPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	c, st := r.b.Result(context.Background(), probeTask)
	if st != ResultReady || c == nil {
		t.Fatalf("regular result file: %v, %v", c, st)
	}
	if w := r.entry(t, probeTask); len(w.Agents) != 3 || w.Source != workflow.SourceResultFile {
		t.Errorf("%d rows, source %s after Result", len(w.Agents), w.Source)
	}
}
