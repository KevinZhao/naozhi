// scheduler_callbacks.go: server-side callback registration + run-event
// types + emit helpers + per-state metrics bump.
//
// Split out of scheduler.go to keep the broadcast surface in one place
// (event shape, register, emit, metric) for new event additions. No
// behaviour change. Methods stay on *Scheduler so the s.onExecute /
// s.onRunStarted / s.onRunEnded atomic.Pointer fields remain accessible
// without exporting.

package cron

import (
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// OnExecuteFunc is called after a cron job finishes execution.
// It receives the job ID, result text (or empty), and error message (or empty).
type OnExecuteFunc func(jobID, result, errMsg string)

// RunStartedEvent is broadcast when a cron run enters the running state
// (after CAS gate, before IM notify resolution). Consumers (Hub) marshal
// to a WS message; the cron package itself never serialises — this keeps
// the package free of server / wshub coupling.
type RunStartedEvent struct {
	JobID     string
	RunID     string
	StartedAt time.Time
	Trigger   TriggerKind
	SessionID string // 可能为空：CAS 之后立刻广播时 GetOrCreate 还没跑
	Fresh     bool
}

// RunEndedEvent is broadcast when a cron run reaches a terminal state
// (succeeded / failed / skipped / timed_out / canceled). EndedAt and
// DurationMS reflect the wall-clock that record path observes.
type RunEndedEvent struct {
	JobID      string
	RunID      string
	State      RunState
	StartedAt  time.Time
	EndedAt    time.Time
	DurationMS int64
	SessionID  string
	ErrorClass ErrorClass
	ErrorMsg   string
	Trigger    TriggerKind
}

// SetOnExecute registers a callback invoked after each cron job execution.
//
// R230-GO-2: the `s.onExecute.Store(&fn)` pattern takes the address of the
// parameter, which forces fn to escape to heap (1 alloc per call). This is
// deliberately accepted: SetOn* are only invoked at startup wiring (1 call
// per scheduler instance per process lifetime), so the per-call allocation
// is invisible. The alternative — atomic.Value with a wrapper struct, or a
// dedicated holder struct — would either lose the typed Load() ergonomics
// callers rely on (Load returns *OnExecuteFunc directly) or balloon the
// API surface. Document-and-accept rather than pessimize the read path.
func (s *Scheduler) SetOnExecute(fn OnExecuteFunc) {
	if fn == nil {
		s.onExecute.Store(nil)
		return
	}
	s.onExecute.Store(&fn)
}

// SetOnRunStarted registers a callback for the run-started broadcast event.
// nil disables the broadcast (testing path / no-WS mode).
func (s *Scheduler) SetOnRunStarted(fn OnRunStartedFunc) {
	if fn == nil {
		s.onRunStarted.Store(nil)
		return
	}
	s.onRunStarted.Store(&fn)
}

// SetOnRunEnded registers a callback for the run-ended broadcast event.
// Invoked for every terminal state including skipped/canceled — the
// callback should distinguish via RunEndedEvent.State.
func (s *Scheduler) SetOnRunEnded(fn OnRunEndedFunc) {
	if fn == nil {
		s.onRunEnded.Store(nil)
		return
	}
	s.onRunEnded.Store(&fn)
}

// emitRunStarted invokes the registered server-side hook outside s.mu so
// hub locks may be acquired by the handler without inversion risk. nil
// hook = no broadcast (used by tests / no-WS deployments).
//
// R230C-GO-15: CronRunStartedTotal bumps here, not at the call sites, so
// the counter cannot drift from the broadcast event count when a new emit
// path lands. Metric advancement is independent of subscriber wiring (the
// nil-hook fast path still bumps), matching the prior contract where both
// executeOpt's normal path and emitOverlapSkipped each manually bumped.
func (s *Scheduler) emitRunStarted(ev RunStartedEvent) {
	metrics.CronRunStartedTotal.Add(1)
	if fn := s.onRunStarted.Load(); fn != nil {
		(*fn)(ev)
	}
}

func (s *Scheduler) emitRunEnded(ev RunEndedEvent) {
	if fn := s.onRunEnded.Load(); fn != nil {
		(*fn)(ev)
	}
}

// bumpRunStateMetrics increments the per-state counter for the terminal
// transition. Mirrored in metrics.go and pinned by counter_wiring_contract_test.
func (s *Scheduler) bumpRunStateMetrics(state RunState) {
	switch state {
	case RunStateSucceeded:
		metrics.CronRunSucceededTotal.Add(1)
	case RunStateFailed:
		metrics.CronRunFailedTotal.Add(1)
	case RunStateSkipped:
		metrics.CronRunSkippedTotal.Add(1)
	case RunStateTimedOut:
		metrics.CronRunTimedOutTotal.Add(1)
	case RunStateCanceled:
		metrics.CronRunCanceledTotal.Add(1)
	}
}
