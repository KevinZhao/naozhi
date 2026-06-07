package cron

import (
	"sync/atomic"
	"time"
)

// runInflight 表示一个正在执行的 cron run。从 executeOpt CAS gate 进入到
// 终态分支退出之间，scheduler.runningJobs[jobID] 持有该结构。前端通过
// list API 的 current_run 字段读取它，所以字段写入需要无锁。
//
// 替代历史的 *atomic.Bool 守卫：CAS 语义不变（running 字段），同时把"正在
// 跑"的元数据（RunID/StartedAt/Phase/SessionID/Trigger）暴露给观察者。
//
// R238-ARCH-3 (#742): the previous layout had 6 separate atomic.Pointer /
// atomic.Bool fields (runID/startedAt/phase/trigger/sessionID/freshSnap).
// snapshot() loaded each one independently, so a writer that updated the
// view between two of those Loads produced a torn read (e.g. RunID from
// run N alongside Phase from run N+1 if a fast finishRun → reset → next
// run.populate sequence interleaved). Per-field atomics only faked
// lock-free safety; the reader had no all-or-nothing guarantee.
//
// New layout: one atomic.Pointer[runInflightView] holding the complete
// observable state. Writers (the executeOpt run goroutine — single
// writer per Job per "running" window thanks to the running CAS gate)
// build a fresh runInflightView and atomically swap the pointer.
// snapshot() does one Load and reads every field off the same struct —
// torn reads are now structurally impossible. 6 Stores per run-start →
// 1 (initial populate) plus a small number of mutate-copy-store updates
// for setPhase / setSessionID / setFresh transitions.
//
// Single-writer assumption: all of executeOpt's setPhase / setSessionID
// / setFresh callers run inside the same goroutine (the runJob
// goroutine). The CAS-based running gate guarantees only one such
// goroutine is active per (Scheduler, Job) at a time, so per-field
// updates do not need their own CAS — a plain Load → mutate-copy →
// Store sequence is race-free for writers and the atomic.Pointer.Store
// is sequentially-consistent for any concurrent reader. If a future
// caller introduces multi-writer paths it must switch to a CAS loop.
//
// 该结构不持久化；进程崩溃时 inflight 信息丢失（设计：见 RFC §4.2）。
type runInflight struct {
	// running 是 CAS 守卫：CompareAndSwap(false, true) 进入临界区，defer
	// Store(false) 退出。即使 view 是 nil，CAS 也能正常工作——这样后续
	// 加新字段不会破坏并发去抖语义。
	running atomic.Bool

	// view 在 CAS=true 阶段被写入；CAS=false 阶段允许是任意旧值（list
	// handler 必须先读 running 再决定要不要展示其它字段）。读者用一次
	// Load 拿到完整快照——所有可观察字段同源同步，杜绝 torn read。
	view atomic.Pointer[runInflightView]

	// phase is split out from view because setPhase fires 4× per run
	// (Queued → Jittering → Spawning → Sending), and the prior mutate-
	// copy-store path on the view pointer paid a runInflightView heap
	// alloc on every transition. Storing phase as an atomic.Int32 enum
	// is a single atomic store with zero alloc; snapshot() composes the
	// observable view by Loading both view (for RunID/StartedAt/Trigger
	// /SessionID/Fresh) and phase (for the current phase string). The
	// "torn read" concern that motivated the unified pointer for the
	// other fields does NOT apply to phase: phase is a single-int enum
	// whose value is meaningful in isolation, and the running-CAS gate
	// already orders its lifecycle with the rest of the view (populate
	// installs the new view AND resets phase before any reader can see
	// the new run; finalize clears both before releasing the gate).
	// R246-GO-15 (#703).
	phase atomic.Int32
}

// runPhase is the int32 enum used by runInflight.phase. The lookup at
// snapshot() time renders these to the on-wire strings expected by the
// dashboard (queued / jittering / spawning / sending) so callers don't
// need to know the int representation. PhasePopulating is the sentinel
// the populate path Stores so a snapshot between running=true CAS and
// the populate Store renders phase="" (treated as "no phase yet" by the
// dashboard) instead of leaking a stale value from the prior run.
type runPhase int32

const (
	phaseUnset     runPhase = 0
	phaseQueued    runPhase = 1
	phaseJittering runPhase = 2
	phaseSpawning  runPhase = 3
	phaseSending   runPhase = 4
)

// phaseToString maps the runPhase enum back to the wire string. The
// dashboard / list API reads the string, so this is the single source
// of truth for the int↔string conversion. unset / unknown values
// render as "" so a reader that snapshots the inflight before populate
// has installed the initial phase observes a missing-phase view rather
// than a stale-phase view.
func (p runPhase) String() string {
	switch p {
	case phaseQueued:
		return PhaseQueued
	case phaseJittering:
		return PhaseJittering
	case phaseSpawning:
		return PhaseSpawning
	case phaseSending:
		return PhaseSending
	default:
		return ""
	}
}

// phaseFromString inverts phaseToString. Returned to setPhase callers
// that pass a string constant; an unknown string maps to phaseUnset so
// a typo in a future call site doesn't crash, just renders as "".
func phaseFromString(s string) runPhase {
	switch s {
	case PhaseQueued:
		return phaseQueued
	case PhaseJittering:
		return phaseJittering
	case PhaseSpawning:
		return phaseSpawning
	case PhaseSending:
		return phaseSending
	default:
		return phaseUnset
	}
}

// RunInflightView 既是 snapshot() 的返回值类型，也是 atomic.Pointer 内部
// 的存储类型。两职合一保证写入快照与读取快照字节布局一致；snapshot
// 直接解引用 Load 得到的指针返回。
//
// R249-ARCH-16 (#982): the canonical type is now the EXPORTED RunInflightView
// so the public Scheduler.CurrentRun method does not surface an unexported
// return type (golint unexported-return). The lowercase runInflightView below
// is a type alias kept so the dense internal call sites (atomic.Pointer,
// snapshot, populate) read unchanged — alias resolution means both spell the
// exact same type, so there is zero behaviour or layout change.
type RunInflightView struct {
	RunID     string
	StartedAt time.Time
	Phase     string
	Trigger   TriggerKind
	SessionID string
	Fresh     bool
}

// runInflightView is the internal alias for RunInflightView. Aliased rather
// than renamed package-wide so the hot internal paths stay terse. R249-ARCH-16.
type runInflightView = RunInflightView

// runFinalizer is a per-run, stack-local cleanup gate. executeOpt creates
// one immediately after it wins the inflight.running CAS and threads the
// pointer through finishRun (broadcast-time cleanup) and its own defer
// (catch-all for jitter-window early returns). finishRun fires it BEFORE
// emitRunEnded so a dashboard list arriving with cron_run_ended observes
// CurrentRun(jobID) == ok:false rather than the stale Spawning view.
//
// Why a per-run struct instead of an atomic gate on *runInflight (the
// pre-#689 first-cut design): both finishRun and the executeOpt defer
// live in the same goroutine, and run-A's finalizer is a distinct object
// from run-B's. The done bool needs no atomic — it's only read+written
// inside one goroutine. More importantly the per-run identity guarantees
// run-A's late defer can NEVER reset metadata that a racing run-B has
// installed: run-A's done=true short-circuits run-A's defer regardless
// of what run-B did to the shared *runInflight in the meantime. R238-GO-2
// + R246-GO-3 (#689). Tests in run_inflight_finalize_test.go pin both
// the broadcast-ordering contract and the run-A→run-B isolation.
type runFinalizer struct {
	inflight *runInflight
	done     bool
}

// finalize is the single cleanup path: clear inflight metadata, then
// release the running CAS gate (R238-GO-2 ordering — clear before
// release, so a TriggerNow that wins the next CAS cannot be observed
// by this goroutine writing nil over its freshly-installed fields).
//
// Idempotent within one finalizer: the done-flag short-circuit ensures
// the executeOpt defer, when finishRun already ran, leaves the shared
// *runInflight alone. Nil-safe so finishRun callers that don't own the
// gate (emitOverlapSkipped) can pass nil. R246-GO-3 (#689).
func (f *runFinalizer) finalize() {
	if f == nil || f.done {
		return
	}
	f.done = true
	if f.inflight == nil {
		return
	}
	f.inflight.reset()
	f.inflight.running.Store(false)
}

// 各 phase 名字常量。固定字符串便于前端切图标。
const (
	PhaseQueued    = "queued"
	PhaseJittering = "jittering"
	PhaseSpawning  = "spawning"
	PhaseSending   = "sending"
)

// reset 把 inflight view 清回未运行态。CAS Store(false) 由 executeOpt defer
// 调用；reset 单独抽出来是因为 DeleteJobByID 路径下我们不动 atomic.Bool
// （见 scheduler.go runningJobs 注释——历史 entry 不清，避免 ID 复用 split
// CAS gate）。reset 仅清掉可观察元数据，避免 list API 把已删 job 的旧
// inflight 残影显示给前端。
//
// R238-ARCH-3 (#742): single Store(nil) replaces the prior 6 independent
// Store(nil) calls — both shorter and atomic across all fields (a
// concurrent reader can no longer observe a partially-reset view).
//
// R246-GO-15 (#703): also reset phase so the next run's populate-window
// reader (between running=true CAS and the phase Store of the new run)
// observes the no-phase sentinel ("") rather than the trailing phase
// from the prior run.
func (r *runInflight) reset() {
	if r == nil {
		return
	}
	r.view.Store(nil)
	r.phase.Store(int32(phaseUnset))
}

// populate 写入 CAS-success 时的初始 view。一次 Store 取代历史的 5 个
// boxString / 1 个 boxTime + 1 个 freshSnap.Store 序列。
//
// R246-GO-15 (#703): phase is split out (atomic.Int32). populate clears
// the int32 to phaseUnset BEFORE storing the new view so a snapshot
// reader between the two Stores observes either (old view, old int32)
// or (old view, unset → falls back to old view.Phase) or (new view,
// unset → falls back to new view.Phase = view's bundled Phase string).
// All three are consistent with some Stored view; no torn (RunID,Phase)
// pair can be observed because snapshot's "int32==unset → use
// view.Phase" rule guarantees populate atomicity. setPhase callers
// after populate switch to the int32 path and pay zero alloc.
func (r *runInflight) populate(v runInflightView) {
	if r == nil {
		return
	}
	cp := v
	// Clear the int32 phase first so a reader catching the partially-
	// populated state observes the new view's bundled Phase string
	// instead of a stale setPhase-from-prior-run int32 value.
	r.phase.Store(int32(phaseUnset))
	r.view.Store(&cp)
}

// snapshot 拷贝当前 inflight 状态。返回 ok=false 时调用方应该忽略 view
// 字段——running=false 时元数据可能是上一轮残留。
//
// R238-ARCH-3 (#742): single atomic.Pointer.Load replaces the prior 6
// independent Loads. The view a reader observes is always the exact
// view some writer Stored — never a mix of two writers' partial updates.
//
// R246-GO-15 (#703): phase lives in a separate atomic.Int32 so its
// hot-path setter (4 transitions per run) can skip the runInflightView
// alloc. snapshot composes the on-wire Phase string by Loading the
// int32 and looking up phaseToString. To keep populate atomicity intact
// (TestRunInflight_SnapshotAtomic exercises alternating-populate races),
// the int32==phaseUnset case falls back to the view's bundled Phase
// string — populate Stores phaseUnset before swapping the view pointer,
// so a snapshot caught mid-populate sees the prior view + its bundled
// Phase or the new view + its bundled Phase, never a cross-view tear.
// Once setPhase has fired (post-populate), the int32 takes over and
// snapshot returns the live phase value.
func (r *runInflight) snapshot() (runInflightView, bool) {
	if r == nil {
		return runInflightView{}, false
	}
	if !r.running.Load() {
		return runInflightView{}, false
	}
	p := r.view.Load()
	ph := runPhase(r.phase.Load())
	if p == nil {
		// CAS 已 true 但 populate 尚未跑（极窄窗口；executeOpt 在 CAS 后
		// 立刻 populate），返回 ok=true + 空字段以保持原 nil-pointer 路径
		// 的零值语义。
		return runInflightView{Phase: ph.String()}, true
	}
	v := *p
	// phaseUnset (the populate-window value) yields v.Phase from the
	// bundled view; once setPhase has run for this run the int32 wins.
	if ph != phaseUnset {
		v.Phase = ph.String()
	}
	return v, true
}

// setPhase 写入当前阶段。executeOpt 在 jitter / snapshot / spawn / send
// 边界调用。
//
// R246-GO-15 (#703): zero-alloc fast path. phase lives in atomic.Int32
// (split out from runInflightView per the field's docstring), so the
// hot-path 4-transitions-per-run cost is now a single atomic.Int32
// Store instead of a Load → struct-copy → Store on the unified view
// pointer. snapshot() recombines the int32 phase with the view at read
// time so the dashboard observes a consistent (RunID, Phase) pair.
//
// Same-phase fast-path: skip the Store when int32 already matches so a
// future caller that asserts setPhase(x) is idempotent (cache-line
// write economy + ordered-store debugger trace cleanliness) still sees
// no observable Store on a no-op call.
//
// 单写者假设见结构体注释：populate / setPhase / setSessionID / setFresh
// 都在同一个 runJob goroutine 中调用，所以 phase Store 不需要 CAS 循环。
func (r *runInflight) setPhase(phase string) {
	if r == nil {
		return
	}
	next := phaseFromString(phase)
	if runPhase(r.phase.Load()) == next {
		return
	}
	r.phase.Store(int32(next))
}

// setSessionID 写入 GetOrCreate 拿到的 session_id。同样 fast-path 去重。
func (r *runInflight) setSessionID(id string) {
	if r == nil || id == "" {
		return
	}
	cur := r.view.Load()
	if cur != nil && cur.SessionID == id {
		return
	}
	var v runInflightView
	if cur != nil {
		v = *cur
	}
	v.SessionID = id
	r.view.Store(&v)
}

// setFresh 写入 snapshotJob 后的 fresh 标志。Mirror setPhase / setSessionID
// 的 single-writer mutate-copy-store。
func (r *runInflight) setFresh(fresh bool) {
	if r == nil {
		return
	}
	cur := r.view.Load()
	if cur != nil && cur.Fresh == fresh {
		return
	}
	var v runInflightView
	if cur != nil {
		v = *cur
	}
	v.Fresh = fresh
	r.view.Store(&v)
}

// The terminal release path is NOT a method on *runInflight. R246-CR-017
// (#759) once extracted a releaseRun(inflightGauge) helper that did
// reset() → running.Store(false) → gauge.Add(-1), but R246-GO-3 (#689)
// superseded it: a method on the shared *runInflight cannot stop run-A's
// late executeOpt defer from clobbering fields a racing run-B already
// installed. The live release path is the per-run, stack-local
// runFinalizer.finalize() (reset + CAS-release, R238-GO-2 ordering
// preserved) plus the gauge Add(-1) at the defer site in scheduler_run.go
// — the finalizer's done flag gives the run-A/run-B isolation a shared
// method could not. See scheduler_run.go's defer block and
// run_inflight_finalize_test.go for the contract this anchor replaced.

// JobRunCounters lives in job.go (R239-CR-7) — it is a Job field, not a
// runInflight field, so keeping its type definition next to the rest of
// Job's wire schema prevents a "where does this field type live?"
// scavenger hunt. Anchor preserved here for git-blame continuity.
