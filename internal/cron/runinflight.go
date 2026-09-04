package cron

import (
	"sync/atomic"
	"time"
)

// runInflight 表示一个正在执行的 cron run。从 executeOpt CAS gate 进入到
// 终态分支退出之间，scheduler.runningJobs[jobID] 持有该结构。前端通过
// list API 的 current_run 字段读取它，所以字段写入需要无锁。
//
// One atomic.Pointer[runInflightView] holds the complete observable state, so
// snapshot() does a single Load and torn reads are structurally impossible
// (#742). Single-writer assumption: all setPhase / setSessionID / setFresh
// callers run in the executeOpt goroutine that won the running CAS gate, so a
// plain Load → mutate-copy → Store is race-free; a future multi-writer path
// must switch to a CAS loop. 该结构不持久化；进程崩溃时 inflight 信息丢失（RFC §4.2）。
type runInflight struct {
	// running 是 CAS 守卫：CompareAndSwap(false, true) 进入临界区，defer
	// Store(false) 退出。即使 view 是 nil，CAS 也能正常工作——这样后续
	// 加新字段不会破坏并发去抖语义。
	running atomic.Bool

	// view 在 CAS=true 阶段被写入；CAS=false 阶段允许是任意旧值（list
	// handler 必须先读 running 再决定要不要展示其它字段）。读者用一次
	// Load 拿到完整快照——所有可观察字段同源同步，杜绝 torn read。
	view atomic.Pointer[runInflightView]

	// phase is split out from view because setPhase fires 4× per run and a
	// mutate-copy-store on the view pointer paid a heap alloc per transition;
	// an atomic.Int32 store is alloc-free. Torn reads do not apply: phase is a
	// single-int enum meaningful in isolation, and populate/finalize order it
	// with the view under the running CAS gate (#703).
	phase atomic.Int32
}

// runPhase is the int32 enum used by runInflight.phase. The lookup at
// snapshot() time renders these to the on-wire strings expected by the
// dashboard (queued / jittering / spawning / sending) so callers don't
// need to know the int representation. phaseUnset is the sentinel
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

// String maps the runPhase enum back to the wire string the dashboard reads.
// unset / unknown values render as "" so a reader that snapshots before
// populate installed the phase observes a missing phase, not a stale one.
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

// phaseFromString inverts runPhase.String; an unknown string maps to
// phaseUnset so a typo in a future call site just renders as "".
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
// 直接解引用 Load 得到的指针返回。Exported so Scheduler.CurrentRun does not
// surface an unexported return type (#982).
type RunInflightView struct {
	RunID     string
	StartedAt time.Time
	Phase     string
	Trigger   TriggerKind
	SessionID string
	Fresh     bool
}

// runInflightView is the internal alias for RunInflightView, kept so the hot
// internal paths stay terse.
type runInflightView = RunInflightView

// runFinalizer is a per-run, stack-local cleanup gate. executeOpt creates one
// right after winning the inflight.running CAS and threads it through finishRun
// (broadcast-time cleanup) and its own defer (catch-all for jitter-window early
// returns). finishRun fires it BEFORE emitRunEnded so a dashboard list arriving
// with cron_run_ended observes CurrentRun(jobID) == ok:false, not a stale view.
//
// Per-run identity (not a gate on the shared *runInflight) guarantees run-A's
// late defer can never reset metadata a racing run-B installed: run-A's
// done=true short-circuits regardless of run-B's writes (#689). done needs no
// atomic — finishRun and the defer run in the same goroutine.
type runFinalizer struct {
	inflight *runInflight
	done     bool
}

// finalize is the single cleanup path: clear inflight metadata, then release
// the running CAS gate — clear before release, so a TriggerNow that wins the
// next CAS cannot observe this goroutine writing nil over its fresh fields.
// Idempotent within one finalizer (done flag) and nil-safe so finishRun
// callers that don't own the gate (emitOverlapSkipped) can pass nil.
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

// reset 把 inflight view 清回未运行态。CAS Store(false) 由 runFinalizer.finalize
// 调用；reset 单独抽出来是因为 DeleteJobByID 路径下我们不动 atomic.Bool，仅清掉
// 可观察元数据，避免 list API 把已删 job 的旧 inflight 残影显示给前端。
// phase 一并清掉，让下一轮 populate 窗口内的读者看到 "" 而不是上一轮残留的 phase。
func (r *runInflight) reset() {
	if r == nil {
		return
	}
	r.view.Store(nil)
	r.phase.Store(int32(phaseUnset))
}

// populate 写入 CAS-success 时的初始 view。phase 先清成 phaseUnset 再 Store
// 新 view：snapshot 的 "int32==unset → 用 view.Phase" 规则保证中间态读者看到
// 的 (RunID, Phase) 总来自同一个已 Store 的 view，不会 torn (#703)。
func (r *runInflight) populate(v runInflightView) {
	if r == nil {
		return
	}
	cp := v
	r.phase.Store(int32(phaseUnset))
	r.view.Store(&cp)
}

// snapshot 拷贝当前 inflight 状态。返回 ok=false 时调用方应该忽略 view
// 字段——running=false 时元数据可能是上一轮残留。Phase 由独立的 atomic.Int32
// 组合而来；int32==phaseUnset 时回退到 view 自带的 Phase，保证 populate
// 中间态不会出现跨 view 的 torn (RunID, Phase) (#703)。
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
// 边界调用。走 atomic.Int32 零分配快路径，值相同则跳过 Store；
// 单写者假设见结构体注释，所以不需要 CAS 循环。
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
