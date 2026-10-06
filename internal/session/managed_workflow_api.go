package session

// managed_workflow_api.go — the workflow board's surface outside the session
// package (docs/rfc/workflow-dashboard.md §5.8.1). Every accessor is a
// lock-free atomic load except Subscribe, and all of them accept a nil board,
// which reads as a session without workflows.

import "github.com/naozhi/naozhi/internal/cli/workflow"

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
