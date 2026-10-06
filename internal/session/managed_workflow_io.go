package session

// managed_workflow_io.go — what the workflow board does off its lock: the
// sessions_update notifier (a timer goroutine that never takes b.mu) and the
// bounded background I/O dispatch, whose first job is resolving each run's
// directory (RFC §5.8 "通知", "RunDir 解析", §5.6(6b) in-flight bounds).

import (
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/metrics"
)

// workflowSummaryMinInterval spaces count-only sessions_update per board.
const workflowSummaryMinInterval = 30 * time.Second

const (
	notifyStructural uint32 = 1 << iota
	notifyCount
)

// workflowTimer is the part of *time.Timer the notifier uses.
type workflowTimer interface{ Reset(d time.Duration) bool }

// boardNotifier emits a board's sessions_update: a structural change at
// once, count-only changes at most every interval with a trailing emission.
// arm runs under b.mu; fire runs on the timer's goroutine and only swaps
// atomics before calling the closures, which take the table lock.
type boardNotifier struct {
	pending  atomic.Uint32
	countAt  atomic.Int64 // unix nano of the last emission
	fns      atomic.Pointer[workflowNotifyFns]
	timer    workflowTimer
	newTimer func(f func()) workflowTimer
	now      func() time.Time
	interval time.Duration
}

// workflowNotifyFns are the router's closures: structural marks the store
// changed and notifies, count only bumps the version the dashboard polls.
type workflowNotifyFns struct{ structural, count func() }

func (n *boardNotifier) init(now func() time.Time) {
	n.now, n.interval = now, workflowSummaryMinInterval
	n.newTimer = func(f func()) workflowTimer {
		t := time.AfterFunc(time.Hour, f)
		t.Stop()
		return t
	}
}

// setNotify installs the closures the notifier calls.
func (b *WorkflowBoard) setNotify(structural, count func()) {
	if b != nil {
		b.notify.fns.Store(&workflowNotifyFns{structural: structural, count: count})
	}
}

// arm schedules an emission for bits. Under b.mu.
func (n *boardNotifier) arm(bits uint32) {
	if n.timer == nil {
		n.timer = n.newTimer(n.fire)
	}
	old := n.pending.Or(bits)
	switch {
	case bits&notifyStructural != 0:
		n.timer.Reset(0)
	case old != 0:
		// Already due: a structural emission covers counts too.
	default:
		n.timer.Reset(n.untilDue())
	}
}

// untilDue is how long a count-only emission still has to wait.
func (n *boardNotifier) untilDue() time.Duration {
	return max(0, time.Duration(n.countAt.Load())+n.interval-time.Duration(n.now().UnixNano()))
}

// fire emits what is pending. A structural emission also refreshes the
// counts, so it restarts the count interval.
func (n *boardNotifier) fire() {
	bits := n.pending.Swap(0)
	if bits == 0 {
		return
	}
	if bits&notifyStructural == 0 {
		if d := n.untilDue(); d > 0 {
			n.pending.Or(bits)
			n.timer.Reset(d)
			return
		}
	}
	n.countAt.Store(n.now().UnixNano())
	fns := n.fns.Load()
	switch {
	case fns == nil:
	case bits&notifyStructural != 0:
		if fns.structural != nil {
			fns.structural()
		}
	case fns.count != nil:
		fns.count()
	}
}

// Background I/O bounds: per board, and across all boards.
const (
	workflowIOSlots         = 8
	workflowBoardIOInFlight = 2
	// workflowIOStuckAfter: a job in flight longer is presumed stuck; it
	// keeps its global slot and the board may start one more.
	workflowIOStuckAfter = 30 * time.Second
)

// workflowIO is the global I/O slot pool every board shares.
var workflowIO = newIOPool(workflowIOSlots)

// ioPool is a fixed number of I/O slots and the boards waiting for one.
// Lock order: b.mu → pool.mu; the pool never takes a board's lock while
// holding its own.
type ioPool struct {
	slots   chan struct{}
	mu      sync.Mutex
	waiting []*WorkflowBoard
}

func newIOPool(n int) *ioPool { return &ioPool{slots: make(chan struct{}, n)} }

// acquire takes a slot, or registers b to be pumped when one frees. It
// tries again once registered: a slot freed between the first try and the
// registration found no one waiting to pump.
func (p *ioPool) acquire(b *WorkflowBoard) bool {
	select {
	case p.slots <- struct{}{}:
		return true
	default:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !slices.Contains(p.waiting, b) {
		p.waiting = append(p.waiting, b)
	}
	select {
	case p.slots <- struct{}{}:
		p.waiting = slices.DeleteFunc(p.waiting, func(w *WorkflowBoard) bool { return w == b })
		return true
	default:
		return false
	}
}

func (p *ioPool) release() { <-p.slots }

// wakeWaiter pumps waiting boards, longest-waiting first, while slots are
// free. Call with no lock held.
func (p *ioPool) wakeWaiter() {
	for {
		p.mu.Lock()
		if len(p.waiting) == 0 || len(p.slots) == cap(p.slots) {
			p.mu.Unlock()
			return
		}
		b := p.waiting[0]
		p.waiting = p.waiting[1:]
		p.mu.Unlock()
		b.mu.Lock()
		b.pumpLocked()
		b.mu.Unlock()
	}
}

// ioJob is one background task of a board, of a kind (ioResolve, ioRead,
// ioLocate): work runs off every lock and returns what to apply under b.mu
// (nil for nothing).
type ioJob struct {
	task, kind string
	work       func() ioApply
}

// ioApply applies a job's result under b.mu; the func it returns, if any,
// runs once b.mu is released (a Tracker merge, whose wake takes b.mu).
type ioApply func() (after func())

// I/O job kinds.
const (
	ioResolve = "resolve"
	ioRead    = "read"
	ioLocate  = "locate"
)

// ioDispatch is a board's background I/O. All bookkeeping is under b.mu and
// makes no system call, so enqueueing is safe on the read loop and inside a
// table transaction. A task has at most one job in flight.
type ioDispatch struct {
	pool     *ioPool
	queue    []ioJob
	inflight map[string]time.Time
}

// enqueueLocked queues job, replacing a queued one of its task and kind,
// and starts what the bounds allow.
func (b *WorkflowBoard) enqueueLocked(job ioJob) {
	d := &b.io
	if i := slices.IndexFunc(d.queue, func(q ioJob) bool { return q.task == job.task && q.kind == job.kind }); i >= 0 {
		d.queue[i] = job
	} else {
		d.queue = append(d.queue, job)
	}
	b.pumpLocked()
}

// pumpLocked starts queued jobs while a board slot and a global slot are free.
func (b *WorkflowBoard) pumpLocked() {
	d := &b.io
	now := b.now()
	for len(d.queue) > 0 {
		stuck := 0
		for _, at := range d.inflight {
			if now.Sub(at) > workflowIOStuckAfter {
				stuck++
			}
		}
		if len(d.inflight) >= workflowBoardIOInFlight+min(stuck, 1) {
			return
		}
		i := slices.IndexFunc(d.queue, func(q ioJob) bool { return d.inflight[q.task].IsZero() })
		if i < 0 || !d.pool.acquire(b) {
			return
		}
		job := d.queue[i]
		d.queue = slices.Delete(d.queue, i, i+1)
		if d.inflight == nil {
			d.inflight = map[string]time.Time{}
		}
		d.inflight[job.task] = now
		go b.runJob(job)
	}
}

// runJob runs job's work off-lock, then applies its result under b.mu and
// hands its slot on.
func (b *WorkflowBoard) runJob(job ioJob) {
	var apply ioApply
	var after func()
	guard := func(f func()) {
		defer func() {
			if r := recover(); r != nil {
				metrics.PanicRecoveredTotal.Add(1)
				slog.Error("workflow board I/O panic recovered", "panic", r, "stack", string(debug.Stack()))
			}
		}()
		f()
	}
	guard(func() { apply = job.work() })
	b.mu.Lock()
	delete(b.io.inflight, job.task)
	if apply != nil {
		guard(func() { after = apply() })
	}
	b.io.pool.release()
	b.pumpLocked()
	b.mu.Unlock()
	b.io.pool.wakeWaiter()
	if after != nil {
		guard(after)
	}
}

// workflowRunSource is what a run's directory is resolved from: the launch
// receipt's transcript dir, the session it hangs under, its run id and the
// session's workspace. A change of any of them resolves it again.
type workflowRunSource struct {
	TranscriptDir, SessionID, RunID, Workspace string
}

// workflowRun is a resolved run directory: RunDir spelled under the
// projects root, Rel / ResultRel below it, the session id its path names.
type workflowRun = claudefs.WorkflowRun

// workflowRunResolver resolves a run directory under projectsRoot; it may
// do I/O and block, so the board calls it only from the I/O dispatch.
type workflowRunResolver func(projectsRoot string, src workflowRunSource) (workflowRun, bool)

// resolveState is one task's resolution: the source it was started for
// and, once done, whether and where it resolved. A changed source replaces
// it, and with it the bookkeeping of the run's result file reads.
type resolveState struct {
	src      workflowRunSource
	done, ok bool
	run      workflowRun
	// readAt is when the result file was last read (unix ms); readEnded
	// whether a read happened with the entry terminal; owed asks for one
	// read once resolved (a new bind's reconciliation, R3b).
	readAt    int64
	readEnded bool
	owed      bool
}

// pending reports whether the run directory is still being resolved: work
// that needs it waits, and the sweeper skips the entry.
func (rs *resolveState) pending() bool { return rs.src.RunID != "" && !rs.done }

// registerResolveLocked starts resolving w's run directory when its source
// is new or changed. Only bookkeeping happens here; the resolver runs on the
// I/O dispatch, and its result lands in the state it was started for, which
// a changed source has already replaced.
func (b *WorkflowBoard) registerResolveLocked(w *workflow.Workflow) {
	src := workflowRunSource{TranscriptDir: w.LaunchTranscriptDir, SessionID: w.SessionID, RunID: w.RunID, Workspace: b.workspace}
	if rs := b.resolve[w.TaskID]; rs != nil && rs.src == src {
		return
	}
	rs := &resolveState{src: src}
	b.resolve[w.TaskID] = rs
	if b.disk.resolve == nil || src.RunID == "" {
		rs.done = true
		return
	}
	task, root, resolver := w.TaskID, b.projectsRoot, b.disk.resolve
	b.enqueueLocked(ioJob{task: task, kind: ioResolve, work: func() ioApply {
		run, ok := resolver(root, src)
		return func() func() {
			rs.done, rs.ok, rs.run = true, ok, run
			if ok && b.resolve[task] == rs {
				b.publishLocked(true)
			}
			return nil
		}
	}})
}
