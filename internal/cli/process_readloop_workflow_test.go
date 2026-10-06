package cli

import (
	"log/slog"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// A workflow snapshot (~286KB decoded at 400 agents) must not outlive its
// frame: eventCh can hold 1024 undrained Events while the session is idle,
// and the readEventBuf slot keeps the last decode until a later frame
// overwrites it — which never happens once the Process is dead.
func TestHandleShimStdout_WorkflowSnapshotNotRetained(t *testing.T) {
	t.Parallel()
	snapshot := readWorkflowProbe(t)[12] // line 13: the final 5-item snapshot
	log := slog.New(slog.DiscardHandler)

	t.Run("delivered", func(t *testing.T) {
		t.Parallel()
		p, srv := shimTestPair(&ClaudeProtocol{})
		t.Cleanup(func() { srv.Close() })
		if out := p.handleShimStdout(shimMsg{Type: "stdout", Seq: 1, Line: snapshot}, log); out != shimDispatchContinue {
			t.Fatalf("outcome = %v, want continue", out)
		}
		var ev clievent.Event
		select {
		case ev = <-p.eventCh:
		default:
			t.Fatal("snapshot frame never reached eventCh")
		}
		if ev.TaskID != "w113pvmto" || ev.Description != "Sum: C" {
			t.Fatalf("precondition: delivered %+v, want the probe's line 13", ev)
		}
		if ev.WorkflowProgress != nil {
			t.Errorf("eventCh Event kept %d snapshot items", len(ev.WorkflowProgress))
		}
		assertReadEventBufCleared(t, p)
	})

	t.Run("killed mid-dispatch", func(t *testing.T) {
		t.Parallel()
		p, srv := shimTestPair(&ClaudeProtocol{})
		t.Cleanup(func() { srv.Close() })
		close(p.killCh)
		if out := p.handleShimStdout(shimMsg{Type: "stdout", Seq: 1, Line: snapshot}, log); out != shimDispatchReturn {
			t.Fatalf("outcome = %v, want return (deliverEvent saw killCh)", out)
		}
		assertReadEventBufCleared(t, p)
	})
}

func assertReadEventBufCleared(t *testing.T, p *Process) {
	t.Helper()
	if p.readEventBuf[0].TaskID != "w113pvmto" {
		t.Fatalf("precondition: readEventBuf[0] = %+v, want the decoded snapshot frame", p.readEventBuf[0])
	}
	for i := range p.readEventBuf {
		if n := len(p.readEventBuf[i].WorkflowProgress); p.readEventBuf[i].WorkflowProgress != nil {
			t.Errorf("readEventBuf[%d] kept %d snapshot items", i, n)
		}
	}
}
