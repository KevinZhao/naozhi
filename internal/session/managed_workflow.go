package session

// managed_workflow.go — the session's workflow board: it merges the Sets of
// the CLI processes the session held (workflow.Tracker) with the entries it
// kept from earlier ones and from sessions.json, stamps wire versions and
// publishes one immutable workflow.Published (docs/rfc/workflow-dashboard.md
// §5.8). Lock order: table lock → b.mu → Tracker.mu. Holding b.mu the board
// never takes the table lock and does no I/O; sessions_update goes out on
// the notifier's timer goroutine, disk work on the I/O dispatch.

import (
	"crypto/rand"
	"encoding/hex"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/cli/workflow"
)

// Board bounds (RFC §5.3) and the display set's terminal tail.
const (
	workflowBoardMaxUnsettled = 16
	workflowBoardMaxTerminal  = 5
	workflowSummaryTerminal   = 3
	// workflowPinMax bounds how long a running workflow keeps its session's
	// CLI alive after its last observed frame.
	workflowPinMax = 6 * time.Hour
)

// workflowNotifier is the optional hook a process offers for the Workflow
// tool runs its CLI reports; *cli.Process implements it. See bookWorkflows.
type workflowNotifier interface {
	Workflows() *workflow.Set
	SetOnWorkflowChange(fn func())
	KnowWorkflowTasks(ids []string)
	ApplyWorkflowResult(rf *workflow.ResultFile) bool
}

// WorkflowBoard is a session's workflow board. Readers use the lock-free
// accessors in managed_workflow_api.go; those and the session's entry
// points (bind, procEnded, restore) accept a nil board.
type WorkflowBoard struct {
	mu           sync.Mutex
	epoch        string
	projectsRoot string
	workspace    string
	// proc is the bound process that has not ended; set is the newest of its
	// Sets the board applied, applied that Set's Version. ended is the last
	// bound process whose end the board settled: rename hands it in again.
	proc    workflowNotifier
	set     *workflow.Set
	applied uint64
	ended   workflowNotifier
	// procGone is when the board last lost its live process (unix ms), 0
	// while it has one; bindAt / bindWrapped are the current bind's time
	// and whether its replay had wrapped.
	procGone    int64
	bindAt      int64
	bindWrapped bool
	// retained holds the entries no live Tracker reports: a former
	// process's, ones the Tracker evicted, ones restored from sessions.json.
	retained map[string]*retainedEntry
	// last is the published state per task; ver the newest wire version.
	last map[string]*wireState
	ver  uint64
	// resolve tracks each task's RunDir resolution; resolver nil leaves
	// RunDir empty.
	resolve  map[string]*resolveState
	resolver workflowRunResolver
	io       ioDispatch

	cur       atomic.Pointer[workflow.Published]
	summaries atomic.Pointer[[]workflow.Summary]
	running   atomic.Bool
	lastObs   atomic.Int64
	subs      map[chan struct{}]struct{}
	notify    boardNotifier
	now       func() time.Time
}

// retainedEntry is an entry the board keeps itself; from is the process
// whose Tracker last held it, nil for one restored from a Ref.
type retainedEntry struct {
	wf   *workflow.Workflow
	from workflowNotifier
}

// wireState is a task's last publication: pub as published (rows stamped,
// RunDir resolved), src the Agents slice its rows were stamped from, live
// whether the bound process's Set held it. sid / sidSrc are its SessionID
// before the resolved run dir overrode it.
type wireState struct {
	pub    *workflow.Workflow
	src    []workflow.Agent
	live   bool
	sid    string
	sidSrc uint8
}

// unresolved is st's entry as it was before the board's RunDir resolution
// applied: the form a retained entry keeps, so a later resolution sees the
// same source tuple.
func (st *wireState) unresolved() *workflow.Workflow {
	w := *st.pub
	w.SessionID, w.Src.SessionID, w.RunDir = st.sid, st.sidSrc, ""
	return &w
}

// newWorkflowBoard returns an empty board whose run dirs resolve under
// projectsRoot.
func newWorkflowBoard(projectsRoot string) *WorkflowBoard {
	var e [8]byte
	_, _ = rand.Read(e[:])
	b := &WorkflowBoard{
		epoch:        hex.EncodeToString(e[:]),
		projectsRoot: projectsRoot,
		retained:     map[string]*retainedEntry{},
		last:         map[string]*wireState{},
		resolve:      map[string]*resolveState{},
		subs:         map[chan struct{}]struct{}{},
		io:           ioDispatch{pool: workflowIO},
		now:          time.Now,
	}
	b.notify.init(b.now)
	b.cur.Store(workflow.NewPublished(b.epoch, nil, nil))
	return b
}

// bookWorkflows binds proc's workflows to s's board, creating the board for a
// session that never had one. onStructural runs for a change of the
// workflow set, a status or a run id, onCount for any other, at most every
// workflowSummaryMinInterval. Bound right before bookProcessEnd, so a
// process that already ended unbinds at once; one whose end the board
// already settled (a renamed dead CLI) stays unbound.
func bookWorkflows(s *ManagedSession, proc processIface, projectsRoot string, onStructural, onCount func()) {
	n, ok := proc.(workflowNotifier)
	if !ok {
		return
	}
	b := s.workflows.Load()
	if b == nil {
		b = newWorkflowBoard(projectsRoot)
		if !s.workflows.CompareAndSwap(nil, b) {
			b = s.workflows.Load()
		}
	}
	b.setNotify(onStructural, onCount)
	b.bind(n, s.Workspace())
}

// workflowPinned reports whether a running workflow, observed within
// workflowPinMax, keeps s's CLI from being released or expired.
func (s *ManagedSession) workflowPinned(now time.Time) bool {
	b := s.WorkflowBoard()
	return b.Running() && now.UnixMilli()-b.LastObservedAt() < workflowPinMax.Milliseconds()
}

// bind makes proc the board's live process. The board folds a process
// still bound into retained (running entries go snapshot_stale: whether its
// CLI died is for its end to tell), hands proc the task ids it knows and
// publishes proc's current Set. Binding the bound process, or the one whose
// end it last settled (its end is delivered once), is a no-op.
func (b *WorkflowBoard) bind(proc workflowNotifier, workspace string) {
	if b == nil || proc == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if proc == b.proc || proc == b.ended {
		return
	}
	if b.proc != nil {
		b.foldLiveLocked(b.proc, staleEntry)
	}
	b.proc, b.set, b.applied, b.procGone, b.ended = proc, nil, 0, 0, nil
	b.workspace, b.bindAt = workspace, b.now().UnixMilli()
	if ids := b.knownLocked(); len(ids) > 0 {
		proc.KnowWorkflowTasks(ids)
	}
	proc.SetOnWorkflowChange(func() { b.wake(proc) })
	set := proc.Workflows()
	b.bindWrapped = set.SeedWrapped
	b.applyLocked(set)
	b.publishLocked(true)
}

// wake publishes p's newest Set when p is the bound process and the Set is
// newer than the one applied; the Tracker calls it after every change.
func (b *WorkflowBoard) wake(p workflowNotifier) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p != b.proc || !b.applyLocked(p.Workflows()) {
		return
	}
	b.publishLocked(true)
}

// applyLocked takes set as the live Set when it is newer than the applied one.
func (b *WorkflowBoard) applyLocked(set *workflow.Set) bool {
	if set == nil || (set.Version <= b.applied && b.set != nil) {
		return false
	}
	b.applied, b.set = set.Version, set
	return true
}

// procEnded settles the entries of a process whose read loop exited. Only a
// CLI that is gone (neither Detached nor ShimLive) ends its running
// workflows as interrupted; otherwise they stay running as snapshot_stale
// for the reattached CLI to report. The bound process's final Set applies
// first, so a terminal frame its last wake missed is not overridden.
func (b *WorkflowBoard) procEnded(p workflowNotifier, end cli.ProcessEnd) {
	if b == nil || p == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	at := end.EndedAt
	if at.IsZero() {
		at = b.now()
	}
	mark := staleEntry
	if !end.Detached && !end.ShimLive {
		mark = interruptedAt(at.UnixMilli())
	}
	if p == b.proc {
		b.applyLocked(p.Workflows())
		b.publishLocked(true)
		b.foldLiveLocked(p, mark)
		p.SetOnWorkflowChange(nil)
		b.proc, b.set, b.procGone, b.ended = nil, nil, at.UnixMilli(), p
	} else {
		for _, r := range b.retained {
			if r.from == p {
				r.wf = mark(r.wf)
			}
		}
	}
	b.publishLocked(true)
}

// foldLiveLocked moves the live entries of the last publication into
// retained as from's, marked by mark.
func (b *WorkflowBoard) foldLiveLocked(from workflowNotifier, mark func(*workflow.Workflow) *workflow.Workflow) {
	for id, st := range b.last {
		if st.live {
			b.retained[id] = &retainedEntry{wf: mark(st.unresolved()), from: from}
			st.live = false
		}
	}
}

// staleEntry marks a running entry whose stream stopped with its CLI alive.
func staleEntry(w *workflow.Workflow) *workflow.Workflow {
	if !workflow.IsRunning(w.Status) || w.Degraded == workflow.DegradedSnapshotStale {
		return w
	}
	n := *w
	n.Degraded = workflow.DegradedSnapshotStale
	return &n
}

// interruptedAt marks a running entry whose CLI is gone.
func interruptedAt(ms int64) func(*workflow.Workflow) *workflow.Workflow {
	return func(w *workflow.Workflow) *workflow.Workflow {
		if !workflow.IsRunning(w.Status) {
			return w
		}
		return workflow.Interrupted(w, ms)
	}
}

// knownLocked lists the task ids the board holds, for a Tracker to build
// their entries from frames that alone would not say they are workflows.
func (b *WorkflowBoard) knownLocked() []string {
	ids := make([]string, 0, len(b.retained)+len(b.last))
	for id := range b.retained {
		ids = append(ids, id)
	}
	for id := range b.last {
		if b.retained[id] == nil {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// knownTaskIDs is knownLocked for a reconnect's replay seed.
func (b *WorkflowBoard) knownTaskIDs() []string {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.knownLocked()
}
