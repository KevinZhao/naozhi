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
// 为什么所有可观察字段都用 atomic.Value（即便 string）：
//   - executeOpt 各阶段在 lock-free 路径写 Phase；list handler 在自己的锁里
//     读，跨 goroutine 必须 race-clean。
//   - SessionID 由 GetOrCreate 成功后写，可能与 list handler 并发读。
//   - StartedAt 在 CAS 成功后立刻 Store，之后只读不写（没必要 atomic，但
//     用 atomic.Pointer 避免 time.Time struct copy 的 race detector 误报）。
//
// 该结构不持久化；进程崩溃时 inflight 信息丢失（设计：见 RFC §4.2）。
type runInflight struct {
	// running 是 CAS 守卫：CompareAndSwap(false, true) 进入临界区，defer
	// Store(false) 退出。即使其他字段未填，CAS 也能正常工作——这样后续
	// 加新字段不会破坏并发去抖语义。
	running atomic.Bool

	// 以下字段在 CAS=true 阶段被赋值，CAS=false 阶段允许是任意旧值（list
	// handler 必须先读 running 再决定要不要展示其它字段）。

	runID     atomic.Pointer[string]
	startedAt atomic.Pointer[time.Time]
	phase     atomic.Pointer[string]
	trigger   atomic.Pointer[string]
	sessionID atomic.Pointer[string]
	freshSnap atomic.Bool // snapshot 时 fresh=true 则 store true
}

// 各 phase 名字常量。固定字符串便于前端切图标。
const (
	PhaseQueued    = "queued"
	PhaseJittering = "jittering"
	PhaseSpawning  = "spawning"
	PhaseSending   = "sending"
)

// strHeap returns a *string referencing a fresh heap-allocated copy of s.
// R234-GO-2 / R234-GO-6: setPhase / setSessionID and the 5-field initializer
// in executeOpt previously called `r.phase.Store(&phase)` directly on a
// parameter or local variable. That pattern depends on Go's escape analysis
// promoting the operand to the heap when the address is captured by an
// atomic.Pointer.Store; it is a soundness contract enforced by the
// compiler today, but it is silent — readers cannot tell from the call site
// that the local "must" escape, and a future inliner change could surprise.
//
// Routing every Store through strHeap makes the heap allocation explicit and
// removes the escape-analysis dependency. The single named local (`p := s`)
// captured by `&p` is unambiguously addressable storage that survives the
// caller's stack frame regardless of inlining heuristics.
//
// Cost is identical to the prior code (one heap alloc per Store) — the
// helper is purely a readability + correctness anchor.
func strHeap(s string) *string {
	p := s
	return &p
}

// timeHeap is the time.Time analogue of strHeap. Same rationale: explicit
// heap copy for atomic.Pointer[time.Time].Store.
func timeHeap(t time.Time) *time.Time {
	p := t
	return &p
}

// reset 把 inflight 字段清回未运行态。CAS Store(false) 由 executeOpt defer
// 调用；reset 单独抽出来是因为 DeleteJobByID 路径下我们不动 atomic.Bool
// （见 scheduler.go runningJobs 注释——历史 entry 不清，避免 ID 复用 split
// CAS gate）。reset 仅清掉可观察元数据，避免 list API 把已删 job 的旧
// inflight 残影显示给前端。
//
// R240-PERF-1: Store(nil) on each *string / *time.Time avoids the 5
// strHeap("") + 1 timeHeap(zero) heap allocations per finishRun (1Hz × N
// jobs accumulates). snapshot already treats Load() == nil as the zero
// value (see the `if p := r.X.Load(); p != nil` guards), so callers
// observe identical zero semantics.
func (r *runInflight) reset() {
	if r == nil {
		return
	}
	r.runID.Store(nil)
	r.phase.Store(nil)
	r.trigger.Store(nil)
	r.sessionID.Store(nil)
	r.startedAt.Store(nil)
	r.freshSnap.Store(false)
}

// snapshot 拷贝当前 inflight 状态。返回 ok=false 时调用方应该忽略 view
// 字段——running=false 时元数据可能是上一轮残留。
type runInflightView struct {
	RunID     string
	StartedAt time.Time
	Phase     string
	Trigger   TriggerKind
	SessionID string
	Fresh     bool
}

func (r *runInflight) snapshot() (runInflightView, bool) {
	if r == nil {
		return runInflightView{}, false
	}
	if !r.running.Load() {
		return runInflightView{}, false
	}
	v := runInflightView{Fresh: r.freshSnap.Load()}
	if p := r.runID.Load(); p != nil {
		v.RunID = *p
	}
	if p := r.startedAt.Load(); p != nil {
		v.StartedAt = *p
	}
	if p := r.phase.Load(); p != nil {
		v.Phase = *p
	}
	if p := r.trigger.Load(); p != nil {
		v.Trigger = TriggerKind(*p)
	}
	if p := r.sessionID.Load(); p != nil {
		v.SessionID = *p
	}
	return v, true
}

// setPhase 写入当前阶段。executeOpt 在 jitter / snapshot / spawn / send
// 边界调用。fast path: 同 Phase 不重复 Store（atomic.Pointer Store 会刷
// cache line，热路径里 phase 写 4 次成本不大但能省就省）。
func (r *runInflight) setPhase(phase string) {
	if r == nil {
		return
	}
	if cur := r.phase.Load(); cur != nil && *cur == phase {
		return
	}
	r.phase.Store(strHeap(phase))
}

// setSessionID 写入 GetOrCreate 拿到的 session_id。同样 fast-path 去重。
func (r *runInflight) setSessionID(id string) {
	if r == nil || id == "" {
		return
	}
	if cur := r.sessionID.Load(); cur != nil && *cur == id {
		return
	}
	r.sessionID.Store(strHeap(id))
}

// jobRunCounters 是 Job 的累计计数。维护策略详见 RFC §3.2：list API 直接
// 读，避免扫 runs/<jobID>/。EWMA / P² 由 P1 引入；P0 阶段先填 total/
// succeeded/failed/skipped/canceled/timed_out 计数，AvgMS 在 P1 实现。
type JobRunCounters struct {
	Total     int64 `json:"total,omitempty"`
	Succeeded int64 `json:"succeeded,omitempty"`
	Failed    int64 `json:"failed,omitempty"`
	Skipped   int64 `json:"skipped,omitempty"`
	TimedOut  int64 `json:"timed_out,omitempty"`
	Canceled  int64 `json:"canceled,omitempty"`
}

// addRun 把一次终态 run 累加到 counters。调用方持 s.mu.Lock。
func (c *JobRunCounters) addRun(state RunState) {
	c.Total++
	switch state {
	case RunStateSucceeded:
		c.Succeeded++
	case RunStateFailed:
		c.Failed++
	case RunStateSkipped:
		c.Skipped++
	case RunStateTimedOut:
		c.TimedOut++
	case RunStateCanceled:
		c.Canceled++
	}
}
