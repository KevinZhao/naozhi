package cli

// process_workflow.go — the Process's workflow Tracker: what feeds it (the
// read loop, the reconnect replay, oversized snapshot lines) and the accessors
// the session board reads it through (docs/rfc/workflow-dashboard.md §5.2).

import (
	"bytes"
	"expvar"
	"runtime/debug"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/shim"
)

// workflowLinesOversize counts shim lines over maxScannerBufBytes that held a
// task_progress frame, i.e. a workflow snapshot too large to read.
var workflowLinesOversize = expvar.NewInt("naozhi_cli_workflow_lines_oversize_total")

// noWorkflows is what a Process without a Tracker (a test fixture) reports.
var noWorkflows = &workflow.Set{}

// Workflows returns the CLI's current workflow Set, never nil. Lock-free.
func (p *Process) Workflows() *workflow.Set {
	if p.workflows == nil {
		return noWorkflows
	}
	return p.workflows.Load()
}

// SetOnWorkflowChange sets the callback woken after every workflow change.
// It carries no data (the callee Loads Workflows) and runs on the writer's
// goroutine outside the Tracker's lock. Safe at any time; nil clears it.
func (p *Process) SetOnWorkflowChange(fn func()) {
	if fn == nil {
		p.onWorkflowChange.Store(nil)
		return
	}
	p.onWorkflowChange.Store(&fn)
}

// KnowWorkflowTasks hands in task ids the session already knows as
// workflows, so their frames build entries without a task_started.
func (p *Process) KnowWorkflowTasks(ids []string) {
	if p.workflows != nil {
		p.workflows.KnowTasks(ids)
	}
}

// ApplyWorkflowResult merges a run's result file into the entry its taskId
// names (workflow.Tracker.ApplyResultFile). The change callback runs before
// it returns, so the caller must not hold a lock that callback takes.
func (p *Process) ApplyWorkflowResult(rf *workflow.ResultFile) bool {
	return p.workflows != nil && p.workflows.ApplyResultFile(rf)
}

func (p *Process) workflowChanged() {
	if fn := p.onWorkflowChange.Load(); fn != nil {
		(*fn)()
	}
}

// observeWorkflow feeds one dispatched frame to the Tracker, which consumes
// its snapshot and sets ev.WorkflowTask.
func (p *Process) observeWorkflow(ev *clievent.Event, now time.Time) {
	if p.workflows != nil {
		p.workflows.Observe(ev, now)
	}
}

// seedWorkflows builds the Tracker from a reconnect's replayed backlog; it
// must run before the read loop starts so no live frame precedes the replay.
// It runs outside the read loop's recover, and the shim keeps the ring, so a
// seed panic would recur on every restart: it is absorbed and the Process
// starts with a fresh Tracker that knows the ids and reads as wrapped (the
// replay was lost, so live frames get the longer unclaimed window).
func (p *Process) seedWorkflows(replays []shim.ServerMsg, lastSeq int64, dec workflow.Decoder, known []string) {
	if p.workflows == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			p.slogger().Error("workflow seed panic recovered; tracker starts unseeded",
				"panic", r, "stack", string(debug.Stack()))
			metrics.PanicRecoveredTotal.Add(1)
			p.workflows = workflow.New(p.workflowChanged)
			p.workflows.SeedFromReplay(workflow.Replay{Wrapped: true}, dec, known)
		}
	}()
	lines := make([]string, 0, len(replays))
	for i := range replays {
		if replays[i].Type == "replay" {
			lines = append(lines, replays[i].Line)
		}
	}
	p.workflows.SeedFromReplay(workflow.Replay{Lines: lines, Wrapped: replayWrapped(replays, lastSeq)}, dec, known)
}

// Heads of an oversized line as the shim frames it: the CLI line is a JSON
// string inside the envelope, so its own quotes arrive escaped.
const (
	oversizePeekBytes      = 1024
	oversizeEnvelopeHead   = `{"type":"stdout",`
	oversizeEnvelopeLine   = `"line":"`
	oversizeProgressHead   = `{\"type\":\"system\",\"subtype\":\"task_progress\"`
	oversizeTaskIDKey      = `\"task_id\":\"`
	oversizeTaskIDQuote    = `\"`
	oversizeMaxTaskIDBytes = 32
)

// noteOversizeLine looks at the head of a shim line too long to read. A
// task_progress frame is counted, and when its task is a tracked workflow
// that entry keeps its rows and shows snapshot_dropped.
func (p *Process) noteOversizeLine(line []byte, now time.Time) {
	id, progress := oversizeProgressTaskID(line[:min(len(line), oversizePeekBytes)])
	if !progress {
		return
	}
	workflowLinesOversize.Add(1)
	if id != "" && p.workflows != nil {
		p.workflows.NoteDropped(id, now)
	}
}

// oversizeProgressTaskID reports whether head opens a stdout envelope whose
// CLI line is a task_progress frame, and that frame's task id ("" when it is
// not in head or not shaped like one). The frame head is anchored, as in
// ReadEvent's fast paths: tool input can hold the same keys unescaped. The
// first escaped task_id key is the frame's own; one inside a string value
// arrives escaped twice and does not match.
func oversizeProgressTaskID(head []byte) (id string, progress bool) {
	if !bytes.HasPrefix(head, []byte(oversizeEnvelopeHead)) {
		return "", false
	}
	i := bytes.Index(head, []byte(oversizeEnvelopeLine))
	if i < 0 {
		return "", false
	}
	frame := head[i+len(oversizeEnvelopeLine):]
	if !bytes.HasPrefix(frame, []byte(oversizeProgressHead)) {
		return "", false
	}
	rest := frame[len(oversizeProgressHead):]
	k := bytes.Index(rest, []byte(oversizeTaskIDKey))
	if k < 0 {
		return "", true
	}
	rest = rest[k+len(oversizeTaskIDKey):]
	n := 0
	for n < len(rest) && n <= oversizeMaxTaskIDBytes && (rest[n] >= 'a' && rest[n] <= 'z' || rest[n] >= '0' && rest[n] <= '9') {
		n++
	}
	if n > oversizeMaxTaskIDBytes || !bytes.HasPrefix(rest[n:], []byte(oversizeTaskIDQuote)) {
		return "", true
	}
	return string(rest[:n]), true
}
