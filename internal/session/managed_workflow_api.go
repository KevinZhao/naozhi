package session

// managed_workflow_api.go — the workflow board's surface outside the session
// package (docs/rfc/workflow-dashboard.md §5.8.1). Every accessor is a
// lock-free atomic load except Subscribe, and all of them accept a nil board,
// which reads as a session without workflows.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/osutil"
)

// WorkflowBoard returns the session's workflow board; nil for a stub that
// never held a process.
func (s *ManagedSession) WorkflowBoard() *WorkflowBoard {
	return s.workflows.Load()
}

// Subscribe returns a channel that receives a value, coalesced, after each
// publication that changed what is on the wire, and the func that ends the
// subscription. A nil board's channel never becomes ready.
func (b *WorkflowBoard) Subscribe() (<-chan struct{}, func()) {
	if b == nil {
		return nil, func() {}
	}
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Published returns the board's current publication; nil for a nil board.
func (b *WorkflowBoard) Published() *workflow.Published {
	if b == nil {
		return nil
	}
	return b.cur.Load()
}

// Summaries returns the summaries the session snapshot carries: every
// unsettled workflow and the latest terminal ones. Shared and READ-ONLY.
func (b *WorkflowBoard) Summaries() []workflow.Summary {
	if b == nil {
		return nil
	}
	if p := b.summaries.Load(); p != nil {
		return *p
	}
	return nil
}

// Running reports whether a published workflow is running or paused.
func (b *WorkflowBoard) Running() bool {
	return b != nil && b.running.Load()
}

// LastObservedAt is the latest observation of a running workflow, or of any
// when none runs, unix ms; 0 when there is none.
func (b *WorkflowBoard) LastObservedAt() int64 {
	if b == nil {
		return 0
	}
	return b.lastObs.Load()
}

// WorkflowAgent locates a workflow agent by agentId, current or earlier
// attempt.
func (b *WorkflowBoard) WorkflowAgent(agentID string) (workflow.AgentLoc, bool) {
	return b.Published().Agent(agentID)
}

// ResultStatus is what Result found for a task.
type ResultStatus uint8

const (
	// ResultNone: the board has no such task, it has not ended, or it has no
	// run id, so no result file can exist.
	ResultNone ResultStatus = iota
	// ResultReady: the result file was read and its cache is returned.
	ResultReady
	// ResultUnavailable: an ended task with a run id whose file is not
	// readable now (run dir unresolved, file missing, not a regular file,
	// too large, naming another task), or whose read had no I/O slot or
	// outlasted ctx.
	ResultUnavailable
)

// Result returns the result file of an ended task, read at most once: a hit
// comes from the board's cache, a miss starts one read per task, which calls
// arriving meanwhile wait on. The read takes a global I/O slot without
// waiting for one; the file merges into the board like any other read, so
// when Result returns, Published already holds the rows and totals it
// brought.
func (b *WorkflowBoard) Result(ctx context.Context, taskID string) (*workflow.ResultCache, ResultStatus) {
	if b == nil {
		return nil, ResultNone
	}
	b.mu.Lock()
	st := b.last[taskID]
	if st == nil || !workflow.IsTerminal(st.pub.Status) || st.pub.RunID == "" {
		b.mu.Unlock()
		return nil, ResultNone
	}
	// A read landing (its Tracker merge still to publish) comes before the
	// cache it has already written.
	var done chan struct{}
	if w := b.resultWait[taskID]; w != nil {
		done = w.done
	} else {
		if c := b.cache[taskID]; c != nil {
			b.mu.Unlock()
			return c, ResultReady
		}
		rs := b.resolve[taskID]
		if rs == nil || !rs.ok || b.disk.read == nil || !b.io.pool.tryAcquire() {
			b.mu.Unlock()
			return nil, ResultUnavailable
		}
		done = b.readForResultLocked(st.pub, rs)
	}
	b.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return nil, ResultUnavailable
	}
	if c := b.cachedResult(taskID); c != nil {
		return c, ResultReady
	}
	return nil, ResultUnavailable
}

// TranscriptStatus is what AgentTranscript found for an agentId.
type TranscriptStatus uint8

const (
	// TranscriptNone: no workflow row of the board has the agentId.
	TranscriptNone TranscriptStatus = iota
	// TranscriptPending: a row has it, but the run directory is not resolved.
	TranscriptPending
	// TranscriptReady: the transcript can be opened.
	TranscriptReady
)

// AgentTranscript is a workflow agent's transcript for drill-in. Open opens
// it anew each call inside an os.Root at the projects root, a regular file
// only and without blocking; Path is where it sits, under the projects
// root's spelling; RunSessionID is the session its run directory hangs
// under, which the transcript's first line must name.
type AgentTranscript struct {
	Loc          workflow.AgentLoc
	Path         string
	Open         func() (*os.File, error)
	RunSessionID string
}

// AgentTranscript locates agentID's transcript, current or earlier attempt,
// from the board's publication.
func (b *WorkflowBoard) AgentTranscript(agentID string) (AgentTranscript, TranscriptStatus) {
	p := b.Published()
	loc, ok := p.Agent(agentID)
	if !ok || !validAgentFileID(agentID) {
		return AgentTranscript{}, TranscriptNone
	}
	i := slices.IndexFunc(p.Workflows, func(w *workflow.Workflow) bool { return w.TaskID == loc.TaskID })
	if i < 0 || p.Workflows[i].RunDir == "" {
		return AgentTranscript{Loc: loc}, TranscriptPending
	}
	root, runDir := b.projectsRoot, p.Workflows[i].RunDir
	rel, err := filepath.Rel(root, runDir)
	parts := strings.Split(rel, string(filepath.Separator))
	if err != nil || len(parts) < 2 || !claudefs.IsValidSessionID(parts[1]) {
		return AgentTranscript{Loc: loc}, TranscriptPending
	}
	rel = claudefs.SubagentJSONL(rel, agentID)
	open := func() (*os.File, error) {
		r, err := os.OpenRoot(root)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		f, _, err := osutil.OpenRegularIn(r, rel, 0)
		return f, err
	}
	return AgentTranscript{Loc: loc, Path: filepath.Join(root, rel), Open: open, RunSessionID: parts[1]}, TranscriptReady
}

// validAgentFileID reports whether id can name an agent file: letters and
// digits only, as CC's agentIds are.
func validAgentFileID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			return false
		}
	}
	return true
}
