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
}

// runInflightView 既是 snapshot() 的返回值类型，也是 atomic.Pointer 内部
// 的存储类型。两职合一保证写入快照与读取快照字节布局一致；snapshot
// 直接解引用 Load 得到的指针返回。
type runInflightView struct {
	RunID     string
	StartedAt time.Time
	Phase     string
	Trigger   TriggerKind
	SessionID string
	Fresh     bool
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
func (r *runInflight) reset() {
	if r == nil {
		return
	}
	r.view.Store(nil)
}

// populate 写入 CAS-success 时的初始 view。一次 Store 取代历史的 5 个
// boxString / 1 个 boxTime + 1 个 freshSnap.Store 序列。
func (r *runInflight) populate(v runInflightView) {
	if r == nil {
		return
	}
	cp := v
	r.view.Store(&cp)
}

// inflightInflightCounter is the contract that releaseRun expects from a
// metrics counter — implemented by metrics.LabeledCounter (Add(int64)).
// Defined inline so runinflight.go does not pull internal/metrics into
// its import graph (keeps this file dependency-light and the
// finalizing-side contract explicit). R246-CR-017.
type inflightInflightCounter interface {
	Add(int64)
}

// releaseRun encapsulates the 3-step release contract that pairs with the
// CAS-true entry in executeOpt: clear observable metadata, drop the CAS
// gate, then decrement the inflight gauge. Order is load-bearing — see
// R238-GO-2 for the reset-before-Store(false) rationale (a TriggerNow that
// wins the next CAS must not have its freshly-populated metadata
// clobbered by this reset). Extracting the trio into a single helper
// pins the order behind one call site so a future inliner / refactor
// cannot drift the steps without a compile-visible signature change.
// R246-CR-017.
//
// Safe to call on a nil receiver (no-op) so test fixtures that build a
// partial Scheduler without an inflight pool do not NPE on the deferred
// release.
func (r *runInflight) releaseRun(gauge inflightInflightCounter) {
	if r == nil {
		return
	}
	r.reset()
	r.running.Store(false)
	if gauge != nil {
		gauge.Add(-1)
	}
}

// snapshot 拷贝当前 inflight 状态。返回 ok=false 时调用方应该忽略 view
// 字段——running=false 时元数据可能是上一轮残留。
//
// R238-ARCH-3 (#742): single atomic.Pointer.Load replaces the prior 6
// independent Loads. The view a reader observes is always the exact
// view some writer Stored — never a mix of two writers' partial updates.
func (r *runInflight) snapshot() (runInflightView, bool) {
	if r == nil {
		return runInflightView{}, false
	}
	if !r.running.Load() {
		return runInflightView{}, false
	}
	p := r.view.Load()
	if p == nil {
		// CAS 已 true 但 populate 尚未跑（极窄窗口；executeOpt 在 CAS 后
		// 立刻 populate），返回 ok=true + 空字段以保持原 nil-pointer 路径
		// 的零值语义。
		return runInflightView{}, true
	}
	return *p, true
}

// setPhase 写入当前阶段。executeOpt 在 jitter / snapshot / spawn / send
// 边界调用。fast path: 同 Phase 不重复 Store（atomic.Pointer Store 会刷
// cache line，热路径里 phase 写 4 次成本不大但能省就省）。
//
// 单写者假设见结构体注释：populate / setPhase / setSessionID / setFresh
// 都在同一个 runJob goroutine 中调用，所以 Load → mutate-copy → Store
// 不需要 CAS 循环。
func (r *runInflight) setPhase(phase string) {
	if r == nil {
		return
	}
	cur := r.view.Load()
	if cur != nil && cur.Phase == phase {
		return
	}
	var v runInflightView
	if cur != nil {
		v = *cur
	}
	v.Phase = phase
	r.view.Store(&v)
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

// JobRunCounters lives in job.go (R239-CR-7) — it is a Job field, not a
// runInflight field, so keeping its type definition next to the rest of
// Job's wire schema prevents a "where does this field type live?"
// scavenger hunt. Anchor preserved here for git-blame continuity.
