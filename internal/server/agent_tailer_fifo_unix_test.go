//go:build unix

package server

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/osutil"
)

// TestTailer_FIFOSwapLeavesOthersRunning is §11.2's drill-in row on the
// registry: one tailer's transcript swapped for a FIFO mid-tail neither
// parks the single pollLoop — another tailer's new line still arrives —
// nor its own close. Both opener kinds: the default and a workflow's.
func TestTailer_FIFOSwapLeavesOthersRunning(t *testing.T) {
	t.Parallel()
	for _, workflowOpener := range []bool{false, true} {
		r := newTailerRegistry("")
		swapped := writeJSONL(t, t.TempDir(), "a1", false)
		other := writeJSONL(t, t.TempDir(), "b1", false)
		var wf *workflowTail
		if workflowOpener {
			wf = &workflowTail{open: func() (*os.File, error) {
				f, _, err := osutil.OpenRegular(swapped, 0)
				return f, err
			}, done: func() (string, bool) { return "", false }}
		}
		r.ensureTailer("k", "ta", "", swapped, wf)
		r.ensureTailer("k", "tb", "", other, nil)
		c, out := newCapturedClient(t, nil)
		for _, id := range []string{"ta", "tb"} {
			if !r.attach(tailerKey{"k", id}, c) {
				t.Fatal("attach failed")
			}
		}
		waitEvent(t, out, "tb", "b1")
		if err := os.Remove(swapped); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(swapped, 0o600); err != nil {
			t.Skipf("mkfifo unsupported here: %v", err)
		}
		t.Cleanup(func() {
			if w, err := os.OpenFile(swapped, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				w.Close()
			}
		})
		// b3 is written after b2 arrived, so a whole tick polled ta in between.
		for _, text := range []string{"b2", "b3"} {
			writeJSONL(t, filepath.Dir(other), text, true)
			waitEvent(t, out, "tb", text)
		}
		closed := make(chan struct{})
		go func() { r.closeTask("k", "ta", ""); close(closed) }()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("closeTask blocked behind the FIFO'd tailer")
		}
		r.Shutdown()
	}
}

// waitEvent waits up to 3s for task's agent_event whose summary is text.
func waitEvent(t *testing.T, out <-chan node.ServerMsg, task, text string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-out:
			if m.Type == "agent_event" && m.TaskID == task && m.Event != nil && m.Event.Summary == text {
				return
			}
		case <-deadline:
			t.Fatalf("no agent_event %q for %s", text, task)
		}
	}
}

// TestTailer_WorkflowOpenerFirstOpen: a workflow tailer's first open goes
// through its opener too, never the path it was given.
func TestTailer_WorkflowOpenerFirstOpen(t *testing.T) {
	t.Parallel()
	r := newTailerRegistry("")
	defer r.Shutdown()
	real := writeJSONL(t, t.TempDir(), "via the opener", false)
	opens := 0
	tl, ok := r.ensureTailer("k", "t1", "", filepath.Join(t.TempDir(), "agent-elsewhere.jsonl"), &workflowTail{
		open: func() (*os.File, error) { opens++; return os.Open(real) },
		done: func() (string, bool) { return "", false },
	})
	if !ok {
		t.Fatal("ensureTailer failed")
	}
	tl.pollOnce()
	tl.mu.Lock()
	got := tl.buffered
	tl.mu.Unlock()
	if opens != 1 || len(got) != 1 || got[0].Summary != "via the opener" {
		t.Errorf("%d opens, buffered %+v; want one open, the opener's line", opens, got)
	}
}
