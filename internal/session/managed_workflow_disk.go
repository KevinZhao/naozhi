package session

// managed_workflow_disk.go — the workflow board's disk reconciliation (RFC
// docs/rfc/workflow-dashboard.md §5.6(4)(6b), §5.9 R0/R3/R4/R5): where a
// run's directory is, the result file CC writes when the run ends, the run
// directory scan for an entry whose run id the replay lost, and the 30s
// sweeper that settles what the stream never did. All file access goes
// through the I/O dispatch, rooted at the projects root.

import (
	"errors"
	"expvar"
	"os"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/session/sessiontable"
)

// Sweeper thresholds (RFC §5.6(6b), §5.9).
const (
	// workflowQuietAfter: an unsettled entry unobserved this long has its
	// result file looked for, at most every workflowQuietAfter.
	workflowQuietAfter = 60 * time.Second
	// A terminal entry without its result file retries every
	// workflowResultRetry for workflowResultRetryFor after it ended.
	workflowResultRetry    = 30 * time.Second
	workflowResultRetryFor = 10 * time.Minute
	// workflowOrphanAfter: with no live process this long (3 reconcile
	// ticks), an unsettled entry is interrupted (R4).
	workflowOrphanAfter = 90 * time.Second
	// workflowUnclaimedAfter: a running entry the bound process's Tracker
	// never reported this long after the bind becomes unknown (R5); the
	// longer window applies when the bind's replay had wrapped.
	workflowUnclaimedAfter        = 90 * time.Second
	workflowUnclaimedAfterWrapped = 10 * time.Minute
)

// workflowRunScanCapped counts run directory scans given up on a session
// with too many runs.
var workflowRunScanCapped = expvar.NewInt("naozhi_session_workflow_run_scan_capped_total")

// workflowDisk is how a board reaches the disk; tests swap it.
type workflowDisk struct {
	resolve workflowRunResolver
	read    func(projectsRoot string, run workflowRun) (*workflow.ResultFile, error)
	locate  func(projectsRoot, workspace, sessionID string, agentIDs []string) (string, error)
}

// workflowDiskFS is the real disk.
var workflowDiskFS = workflowDisk{resolve: resolveWorkflowRun, read: readWorkflowResult, locate: locateWorkflowRun}

func resolveWorkflowRun(root string, src workflowRunSource) (workflowRun, bool) {
	return claudefs.ResolveWorkflowRunDir(root, claudefs.WorkflowRunSource{
		TranscriptDir: src.TranscriptDir,
		ProjectDirs:   claudefs.WorkspaceProjectDirs(root, src.Workspace),
		SessionID:     src.SessionID,
		RunID:         src.RunID,
	})
}

// readWorkflowResult reads a run's result file through an os.Root at the
// projects root, so no component of the resolved path is trusted again.
func readWorkflowResult(root string, run workflowRun) (*workflow.ResultFile, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, _, err := osutil.OpenRegularIn(r, run.ResultRel, workflow.MaxResultFileBytes)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return workflow.ReadResultFile(f)
}

func locateWorkflowRun(root, workspace, sessionID string, agentIDs []string) (string, error) {
	return claudefs.LocateWorkflowRun(root, claudefs.WorkspaceProjectDirs(root, workspace), sessionID, agentIDs)
}

// missVerdict is what a sweeper read that finds no result file for an
// unsettled entry concludes.
type missVerdict uint8

const (
	missNothing   missVerdict = iota
	missOrphan                // R4: no live process for workflowOrphanAfter → interrupted
	missUnclaimed             // R5: the live process never claimed it → unknown
)

// diskWorkLocked starts the disk work p is due at publication: the result
// file once the entry is terminal (the board's first read of a restored
// terminal entry, too), the reconciliation read a new bind owes, and the
// run directory scan for a live entry whose run id a wrapped replay lost.
func (b *WorkflowBoard) diskWorkLocked(p *workflow.Workflow, live bool) {
	id := p.TaskID
	rs := b.resolve[id]
	if rs == nil {
		return
	}
	if b.reconcile && p.RunID != "" && !p.ResultLoaded {
		rs.owed = true
	}
	if rs.ok && !p.ResultLoaded && (rs.owed || (workflow.IsTerminal(p.Status) && !rs.readEnded)) {
		b.enqueueReadLocked(p, rs, missNothing)
	}
	if p.RunID == "" && p.SessionID != "" && live && b.bindWrapped && !b.scanned[id] {
		var agents []string
		for i := range p.Agents {
			if a := p.Agents[i].AgentID; a != "" {
				agents = append(agents, a)
			}
		}
		if len(agents) > 0 {
			b.scanned[id] = true
			b.enqueueLocateLocked(id, p.SessionID, agents)
		}
	}
}

// enqueueReadLocked reads p's result file. A file whose taskId names the
// entry merges; for a live entry the Tracker merges it, after the result
// cache is written, so a published source of result_file always has its
// result. Otherwise miss, still true when the read returns, settles it.
func (b *WorkflowBoard) enqueueReadLocked(p *workflow.Workflow, rs *resolveState, miss missVerdict) {
	id, root, run, read := p.TaskID, b.projectsRoot, rs.run, b.disk.read
	if read == nil {
		return
	}
	rs.readAt, rs.owed = b.now().UnixMilli(), false
	if workflow.IsTerminal(p.Status) {
		rs.readEnded = true
	}
	b.enqueueLocked(ioJob{task: id, kind: ioRead, work: func() ioApply {
		rf, err := read(root, run)
		return func() func() { return b.applyReadLocked(id, rs, rf, err, miss) }
	}})
}

// applyReadLocked lands a result file read for task id under rs. A file
// naming the entry merges: the result cache is written first, then the
// Tracker merges a live entry (the returned func, run once b.mu is
// released; Result calls meanwhile wait on resultWait) or the board merges
// its own. Anything else leaves the entry as it is, except that a miss
// verdict still true now settles it.
func (b *WorkflowBoard) applyReadLocked(id string, rs *resolveState, rf *workflow.ResultFile, err error, miss missVerdict) func() {
	st := b.last[id]
	if b.resolve[id] != rs || st == nil || st.pub.ResultLoaded {
		return nil
	}
	if err != nil || rf.TaskID != id || !rf.Terminal() {
		if miss != missNothing && b.missLocked(id, b.now().UnixMilli()) == miss {
			// This read counts as the settled entry's first.
			rs.readEnded = miss == missOrphan
			b.settleLocked(id, miss)
			b.publishLocked(true)
		}
		return nil
	}
	if st.live && b.proc != nil {
		b.cache[id] = workflow.NewResultCache(rf)
		proc := b.proc
		merged := b.resultWait[id] // a Result read's, which clears it itself
		own := merged == nil
		if own {
			merged = make(chan struct{})
			b.resultWait[id] = merged
		}
		return func() {
			if own {
				defer func() {
					b.mu.Lock()
					delete(b.resultWait, id)
					b.mu.Unlock()
					close(merged)
				}()
			}
			if !proc.ApplyWorkflowResult(rf) {
				b.mu.Lock()
				b.mergeRetainedLocked(id, rf)
				b.mu.Unlock()
			}
		}
	}
	b.mergeRetainedLocked(id, rf)
	return nil
}

// readForResultLocked reads p's result file for a Result call, on a global
// slot the caller took, and returns the channel closed once the read has
// landed (its Tracker merge included). The same bookkeeping and landing as
// enqueueReadLocked, but outside the board's two-job bound: the caller
// already holds the slot, and the one read per task is the singleflight.
func (b *WorkflowBoard) readForResultLocked(p *workflow.Workflow, rs *resolveState) chan struct{} {
	id, root, run, read := p.TaskID, b.projectsRoot, rs.run, b.disk.read
	rs.readAt, rs.owed, rs.readEnded = b.now().UnixMilli(), false, true
	done := make(chan struct{})
	b.resultWait[id] = done
	go func() {
		var after func()
		guardWorkflowIO(func() {
			rf, err := read(root, run)
			b.mu.Lock()
			defer b.mu.Unlock()
			after = b.applyReadLocked(id, rs, rf, err, missNothing)
		})
		b.io.pool.release()
		b.io.pool.wakeWaiter()
		if after != nil {
			guardWorkflowIO(after)
		}
		b.mu.Lock()
		delete(b.resultWait, id)
		b.mu.Unlock()
		close(done)
	}()
	return done
}

// mergeRetainedLocked merges a result file into the board's own entry.
func (b *WorkflowBoard) mergeRetainedLocked(id string, rf *workflow.ResultFile) {
	r := b.retained[id]
	if r == nil {
		return
	}
	merged, ok := workflow.MergeResultFile(r.wf, rf)
	if !ok {
		return
	}
	b.cache[id] = workflow.NewResultCache(rf)
	r.wf = merged
	b.publishLocked(true)
}

// enqueueLocateLocked scans the session's run directories for the one
// holding agents' transcripts (R3a); a find becomes the entry's run id.
func (b *WorkflowBoard) enqueueLocateLocked(id, sessionID string, agents []string) {
	root, ws, locate := b.projectsRoot, b.workspace, b.disk.locate
	if locate == nil {
		return
	}
	b.enqueueLocked(ioJob{task: id, kind: ioLocate, work: func() ioApply {
		runID, err := locate(root, ws, sessionID, agents)
		if errors.Is(err, claudefs.ErrTooManyWorkflowRuns) {
			workflowRunScanCapped.Add(1)
		}
		return func() func() {
			if st := b.last[id]; runID != "" && st != nil && st.pub.RunID == "" {
				b.located[id] = runID
				b.publishLocked(true)
			}
			return nil
		}
	}})
}

// missLocked is the verdict a missing result file would bring for task id
// now: R4 when the board has had no live process for workflowOrphanAfter,
// R5 when its live process has not claimed a running entry it keeps itself.
func (b *WorkflowBoard) missLocked(id string, now int64) missVerdict {
	st, r := b.last[id], b.retained[id]
	if st == nil || r == nil || st.live || !workflow.IsUnsettled(st.pub.Status) {
		return missNothing
	}
	if b.proc == nil {
		if b.procGone > 0 && now-b.procGone >= workflowOrphanAfter.Milliseconds() {
			return missOrphan
		}
		return missNothing
	}
	window := workflowUnclaimedAfter
	if b.bindWrapped {
		window = workflowUnclaimedAfterWrapped
	}
	if workflow.IsRunning(st.pub.Status) && !b.claimed[id] && now-b.bindAt >= window.Milliseconds() {
		return missUnclaimed
	}
	return missNothing
}

// settleLocked applies verdict to the board's own entry for id.
func (b *WorkflowBoard) settleLocked(id string, verdict missVerdict) {
	r := b.retained[id]
	switch verdict {
	case missOrphan:
		r.wf = workflow.Interrupted(r.wf, b.now().UnixMilli())
	case missUnclaimed:
		r.wf = workflow.Unclaimed(r.wf)
	}
}

// sweep is the board's turn of the workflow sweeper: it looks for the
// result file of an unsettled entry gone quiet and of a terminal one still
// without it, and settles the entries no process will (R4, R5) once a read
// finds no file, or at once when there is no run dir to read. Entries whose
// run dir is still resolving wait. It also pumps the board's queued I/O.
func (b *WorkflowBoard) sweep(now time.Time) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ms := now.UnixMilli()
	settled := false
	for id, st := range b.last {
		w, rs := st.pub, b.resolve[id]
		if rs == nil || rs.pending() || w.ResultLoaded {
			continue
		}
		// The failed state still decides R4 / R5 below; a retry that finds
		// the dir is read from the next tick on.
		b.retryResolveLocked(id, rs, ms)
		since := ms - rs.readAt
		if workflow.IsUnsettled(w.Status) {
			miss := b.missLocked(id, ms)
			quiet := ms-w.LastObservedAt >= workflowQuietAfter.Milliseconds() && since >= workflowQuietAfter.Milliseconds()
			switch {
			case rs.ok && (miss != missNothing || quiet):
				b.enqueueReadLocked(w, rs, miss)
			case miss != missNothing:
				b.settleLocked(id, miss)
				settled = true
			}
			continue
		}
		recent := ms-w.EndedAt <= workflowResultRetryFor.Milliseconds()
		if rs.ok && recent && since >= workflowResultRetry.Milliseconds() {
			b.enqueueReadLocked(w, rs, missNothing)
		}
	}
	if settled {
		b.publishLocked(true)
	}
	b.pumpLocked()
}

// sweepWorkflowBoards runs every session's board sweep, off the table
// lock. A free function on the save tick: the Router's method budget is
// spent.
func sweepWorkflowBoards(ss *sessiontable.Table[*ManagedSession, routerState, routerStateView], now time.Time) {
	var boards []*WorkflowBoard
	ss.View(func(v sessView) {
		for _, s := range v.All() {
			if b := s.WorkflowBoard(); b != nil {
				boards = append(boards, b)
			}
		}
	})
	for _, b := range boards {
		b.sweep(now)
	}
}

// cachedResult is the result file the board read for task id, nil if none.
func (b *WorkflowBoard) cachedResult(id string) *workflow.ResultCache {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cache[id]
}
