package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// benchSnapshot is a decoded task_progress frame with n agents shaped like a
// batch workflow's, the last `running` of them moving with tick.
func benchSnapshot(n, running, tick int, eq bool) clievent.Event {
	items := make([]clievent.WorkflowItem, 0, n+6)
	for i := 1; i <= 6; i++ {
		items = append(items, clievent.WorkflowItem{Type: clievent.WorkflowItemPhase, Index: i, Title: fmt.Sprintf("Phase %d", i)})
	}
	summary := "ready"
	if eq {
		summary = "GOTOOLCHAIN=go1.26.6 go test -run=TestX ./internal/cli/..."
	}
	for i := 1; i <= n; i++ {
		it := clievent.WorkflowItem{
			Type: clievent.WorkflowItemAgent, Index: i, Label: fmt.Sprintf("impl:%d.0", i), PhaseIndex: i%6 + 1,
			AgentID: fmt.Sprintf("a%016x", i), Model: "claude-opus-5-5[1m]", State: "done", Attempt: 1,
			QueuedAt: 1791029368726, StartedAt: 1791029368736, LastProgressAt: 1791029804410, DurationMs: 435671,
			Tokens: 29976, ToolCalls: 12, LastToolName: "Bash", LastToolSummary: summary,
		}
		if i > n-running {
			it.State, it.Tokens, it.LastToolSummary = "progress", int64(100*tick), fmt.Sprintf("%s step %d", summary, tick)
		}
		items = append(items, it)
	}
	return clievent.Event{Type: "system", SubType: "task_progress", TaskID: "wbig00001", Description: "Implement: impl:1.0", WorkflowProgress: items, SessionID: wfSID}
}

// benchBoardWake measures what the read loop pays per snapshot past the
// decode: Tracker.Observe plus the board's wake (merge, row stamps,
// publication, summaries), at steady state.
func benchBoardWake(b *testing.B, n int, eq bool) {
	proc := newWFProc()
	board := newWorkflowBoard("/projects")
	board.bind(proc, "/ws")
	start := wfStarted("wbig00001", "batch")
	proc.observe(time.Now(), start)
	for tick := range 2 {
		ev := benchSnapshot(n, 3, tick, eq)
		proc.observe(time.Now(), ev)
	}
	evs := make([]clievent.Event, b.N)
	for i := range evs {
		evs[i] = benchSnapshot(n, 3, i+2, eq)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range evs {
		proc.tr.Observe(&evs[i], time.Now())
	}
	b.StopTimer()
	if w := board.Published().Workflows; len(w) != 1 || len(w[0].Agents) != n {
		b.Fatalf("published %d entries", len(w))
	}
}

func BenchmarkWorkflowBoard_Wake398(b *testing.B)    { benchBoardWake(b, 398, false) }
func BenchmarkWorkflowBoard_Wake2000Eq(b *testing.B) { benchBoardWake(b, 2000, true) }
