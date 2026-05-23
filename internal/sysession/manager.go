package sysession

import (
	"context"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// osExit is os.Exit indirected through a package var so tests can swap
// it for a panic recovery.  Stop calls this on the deadline-exceeded
// path; production code never overrides it.
var osExit = os.Exit

// Config is the top-level sysession configuration handed to NewManager.
// Mirrors the YAML shape under config.sysession (see RFC §7.5).
type Config struct {
	// Enabled toggles the entire Manager.  When false, NewManager
	// returns a no-op Manager (Start/Stop are safe but no daemons run).
	Enabled bool

	// TickTimeout is the per-Tick budget Manager passes via context.
	// Daemons that exceed it return DaemonRunTimedOut.  Zero falls back
	// to defaultTickTimeout.
	TickTimeout time.Duration

	// Runner is the LLM-call abstraction shared by all daemons.
	// Required when Enabled and at least one daemon is enabled — daemons
	// that don't actually call an LLM (future TransientSweeper) can
	// safely ignore the Runner.
	Runner Runner

	// Router is the session router subset the daemons need.  Required
	// when Enabled.
	Router SystemSessionRouter

	// Daemons is the per-daemon config map.  Key is daemon name (must
	// match an entry in builtinDaemons).  Value carries enable flag +
	// tick interval + daemon-specific knobs.
	Daemons map[string]DaemonRuntimeConfig

	// OnRunStarted / OnRunEnded receive run lifecycle events for
	// dashboard WS broadcast.  Both nil-safe; either may be nil
	// independently.  Manager invokes them outside any Manager-internal
	// lock so handlers may take their own locks freely.
	OnRunStarted func(DaemonRunStartedEvent)
	OnRunEnded   func(DaemonRunEndedEvent)

	// NewTicker is an optional injection point for tests.  nil means
	// time.NewTicker.  Test usage:
	//
	//   ch := make(chan time.Time, 1)
	//   cfg.NewTicker = func(d time.Duration) (<-chan time.Time, func()) {
	//       return ch, func() {}
	//   }
	//   // poke ch to drive runOnce
	NewTicker tickerFactory
}

// DaemonRuntimeConfig is the common-shape per-daemon runtime knobs
// every built-in daemon understands.  Daemon-specific fields are
// passed via Daemons[name].Specific (DaemonConfig).
type DaemonRuntimeConfig struct {
	Enabled  bool
	Tick     time.Duration
	Specific DaemonConfig
}

const (
	defaultTickTimeout = 30 * time.Second

	// defaultDaemonTickInterval is the fallback tick cadence when a daemon
	// runtime config leaves Tick zero/negative. Distinct from
	// defaultTickTimeout (per-Tick context budget) — this one drives how
	// often runOnce gets scheduled.
	defaultDaemonTickInterval = 30 * time.Second

	// consecutiveCLIFailureLimit is the breaker threshold (RFC §7.4).
	// Hit this many CLI/panic failures in a row and Manager stops the
	// daemon until process restart.  Validation/timeout failures DO NOT
	// count toward this limit.
	consecutiveCLIFailureLimit = 5
)

// tickerFactory abstracts time.NewTicker so tests can drive ticks on
// demand.  Returns the channel + a stop closure (must be invoked).
type tickerFactory func(d time.Duration) (<-chan time.Time, func())

func stdTickerFactory(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// daemonRecord is the per-daemon runtime state.  We hold pointers in
// Manager.daemons (not values) so atomic fields don't relocate when
// the slice grows during NewManager and so the runDaemonLoop goroutine
// shares one canonical record with the rest of Manager.
type daemonRecord struct {
	daemon Daemon
	tick   time.Duration

	// inflight is the per-daemon overlap gate.  CompareAndSwap(false,
	// true) before Tick; defer Store(false).  Manager uses atomic.Bool
	// (not a sync.Mutex) so a stuck Tick doesn't queue new ticks behind
	// it — overlapping ticks are explicitly skipped, not deferred.
	inflight atomic.Bool

	// disabled is set when consecutiveCLIFailures crosses the breaker
	// threshold.  A disabled daemon's ticks short-circuit before
	// invoking the daemon's Tick.
	disabled atomic.Bool

	// Failure counters.  Atomic so the dashboard endpoint can read them
	// without taking Manager.mu.
	consecutiveCLIFailures        atomic.Int32
	consecutiveValidationFailures atomic.Int32

	// processStartedAt is captured once at NewManager time so the
	// dashboard can show "no run since process start" vs "never ran"
	// in the UI.  Same value for every record on the same Manager
	// instance.
	processStartedAt time.Time

	// runs holds the per-daemon ring buffer of completed DaemonRuns.
	runs *runRing
}

// Manager runs all daemons.  Lifecycle:
//
//	NewManager → Start → ... → Stop
//
// Manager is single-shot — Stop is terminal.  A future restart should
// build a fresh Manager.
type Manager struct {
	enabled bool
	cfg     Config
	tickFn  tickerFactory
	// daemons is populated once by NewManager and never mutated
	// afterwards; safe to read concurrently from Inspector / Tick paths
	// without taking a lock.
	daemons []*daemonRecord
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc

	// Lifecycle hooks.  Held under hookMu so SetCallbacks (called late
	// during startup wiring, after Hub is built) doesn't race
	// recordRun callers.  Reads happen on every Tick (twice — once at
	// run start, once at run end); writes are once or twice during
	// init.  RWMutex lets concurrent ticks read in parallel without
	// serialising on a single mutex.
	hookMu       sync.RWMutex
	onRunStarted func(DaemonRunStartedEvent)
	onRunEnded   func(DaemonRunEndedEvent)

	// started is set to true inside Start's startOnce.Do.  Stop checks
	// it before doing any cancellation/wait work so a Stop-before-Start
	// caller (e.g. a wiring bug that fails between NewManager and Start)
	// short-circuits cleanly instead of waiting on a never-built ctx.
	// R232-GO-3: previous guard `m.cancel != nil` was correct only after
	// Start ran but didn't disambiguate "Start never called" from "Start
	// called but raced past the m.cancel assignment" — this atomic flag
	// closes that ambiguity by happening exactly once under startOnce.
	started atomic.Bool

	startOnce sync.Once
	stopOnce  sync.Once
}

// NewManager builds a Manager from cfg.  Validates the built-in daemon
// list, builds enabled daemons, and pre-allocates their runRings.
//
// Disabled (cfg.Enabled=false) returns a Manager whose Start is a no-op.
//
// Errors only when the configuration is internally inconsistent (a
// daemon's Build fails, an invalid name slipped into builtinDaemons).
// Per-daemon "enabled but no Tick interval" defaults silently, matching
// the rest of naozhi's config approach.
func NewManager(cfg Config) (*Manager, error) {
	validateBuiltinDaemonNames()

	if cfg.NewTicker == nil {
		cfg.NewTicker = stdTickerFactory
	}
	if cfg.TickTimeout <= 0 {
		cfg.TickTimeout = defaultTickTimeout
	}

	m := &Manager{
		enabled:      cfg.Enabled,
		cfg:          cfg,
		tickFn:       cfg.NewTicker,
		onRunStarted: cfg.OnRunStarted,
		onRunEnded:   cfg.OnRunEnded,
	}
	if !cfg.Enabled {
		// Build nothing; Start is a no-op.
		return m, nil
	}
	if cfg.Router == nil {
		return nil, fmt.Errorf("sysession: NewManager requires Router when enabled")
	}

	// Single timestamp shared by every daemonRecord on this Manager
	// instance — Manager represents one process start, so all daemons
	// agree on the "since process start" baseline shown in the
	// dashboard.
	now := time.Now()
	for _, factory := range builtinDaemons {
		runtime, ok := cfg.Daemons[factory.Name]
		if !ok || !runtime.Enabled {
			continue
		}
		deps := DaemonDeps{
			Router: cfg.Router,
			Runner: cfg.Runner,
			Cfg:    runtime.Specific,
		}
		d, err := factory.Build(deps)
		if err != nil {
			return nil, fmt.Errorf("sysession: build daemon %q: %w", factory.Name, err)
		}
		// Allow Configurable daemons to surface configuration errors
		// (Build itself can already fail; Configure is the lazy hook
		// for daemons that prefer to validate after construction).
		if c, ok := d.(Configurable); ok {
			if err := c.Configure(runtime.Specific); err != nil {
				return nil, fmt.Errorf("sysession: configure daemon %q: %w", factory.Name, err)
			}
		}
		tick := runtime.Tick
		if tick <= 0 {
			tick = defaultDaemonTickInterval
		}
		m.daemons = append(m.daemons, &daemonRecord{
			daemon:           d,
			tick:             tick,
			processStartedAt: now,
			runs:             newRunRing(),
		})
	}
	return m, nil
}

// Start launches one goroutine per enabled daemon.  Start is idempotent
// (calling it twice is a logic error in calling code, but we panic
// rather than silently double-spawn).
//
// Returns immediately; daemons run asynchronously.  Callers should
// invoke Stop during shutdown.
func (m *Manager) Start(parent context.Context) {
	if !m.enabled {
		return
	}
	startedThisCall := false
	m.startOnce.Do(func() {
		m.ctx, m.cancel = context.WithCancel(parent)
		for _, rec := range m.daemons {
			m.wg.Add(1)
			go m.runDaemonLoop(rec)
		}
		// Publish the started flag AFTER ctx/cancel/goroutines are
		// installed so a concurrent Stop that observes started=true is
		// guaranteed to see m.cancel populated.
		m.started.Store(true)
		slog.Info("sysession: manager started", "daemons", len(m.daemons))
		startedThisCall = true
	})
	if !startedThisCall {
		panic("sysession: Manager.Start called twice")
	}
}

// Stop cancels the daemon ctx and waits for all goroutines to finish.
// stopCtx bounds the wait — when it expires before goroutines drain we
// log loudly and force-exit with code 2 rather than leaking goroutines
// that may still call into Router after Router.Stop.  RFC v2.1 §5.2:
// Tick must honour ctx; if it doesn't, the daemon is broken and the
// operator should hear about it loudly at shutdown rather than
// silently corrupting state.
//
// We prefer os.Exit(2) over panic for the deadline path:
//   - panic would dump goroutine stacks to stderr; those stacks may
//     contain in-flight buildExcerpt strings (= user conversation
//     fragments).  Container logs would then leak conversation
//     content that the deliberately-omitted WS ErrorMsg field works
//     hard to keep server-side (RFC §9.4 / Sec-LOW-2).
//   - exit code 2 is a discriminable signal to systemd that this was
//     a hard shutdown failure, not a clean exit (0) and not a panic
//     (typically 2 already, but the explicit slog message makes the
//     cause attributable).
//
// R232-ARCH-13: this divergence from cron Stop's "budget + leak" stance
// is intentional — sysession daemons run user-prompt-derived strings
// through a CLI subprocess and a stuck goroutine touching a torn-down
// Router would risk leaking those into another session's reply (the
// server side of "Sec-LOW-2"). Cron's budget+leak path is safe because
// cron deliveries pass through dispatch's outbound retry which
// re-resolves the active session. Aligning the two policies would
// require either (a) sysession giving up its strict no-leak invariant
// or (b) cron adopting force-exit (regression for cron jobs that legit
// take longer than budget). Tracked as long-running design tension; do
// not "harmonise" without revisiting Sec-LOW-2 and cron-shutdown-budget.
//
// Stop is idempotent.  Subsequent calls are no-ops.
func (m *Manager) Stop(stopCtx context.Context) {
	if !m.enabled {
		return
	}
	// R232-GO-3: short-circuit when Start was never called. The atomic
	// `started` flag is set inside startOnce.Do so the only way to observe
	// false here is "Start has not entered its critical section yet" — in
	// that case there are no daemons to cancel and no wg slots to drain,
	// and a later Start would still no-op because stopOnce already fired.
	if !m.started.Load() {
		m.stopOnce.Do(func() {})
		return
	}
	m.stopOnce.Do(func() {
		// m.cancel is set by Start; the started.Load() check above
		// guarantees m.cancel is populated by the time we reach here.
		m.cancel()
		done := make(chan struct{})
		// R234-GO-5: this drainer goroutine is intentionally NOT tracked by
		// any WaitGroup. It serves only to bridge wg.Wait() into the select
		// below so the deadline path can fire without blocking on the wait.
		// On the deadline branch osExit(2) terminates the whole process, so
		// the abandoned goroutine has no observable lifetime.
		// On the clean branch wg.Wait() has already returned, close(done)
		// fires, and the goroutine exits before this function returns —
		// `done` is the synchronisation point the closure observes.
		// Tests that swap osExit for a panic-recovery on the deadline path
		// will see this goroutine block forever in wg.Wait(); that is
		// expected (the test is exercising the "daemon ignored ctx" fault
		// mode) and is the reason we surface osExit as a package var rather
		// than calling os.Exit directly.
		go func() {
			m.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
			slog.Info("sysession: manager stopped cleanly")
		case <-stopCtx.Done():
			slog.Error("sysession: Stop deadline exceeded; daemons did not honour ctx — this is a daemon bug, not a transient error",
				"hint", "force-exit so leaking goroutines don't write to a torn-down router")
			osExit(2)
		}
	})
}

// Inspector returns a read-only snapshot of all daemons' state for
// the /api/system/daemons endpoint.  Cheap to call.
func (m *Manager) Inspector() []DaemonStatus {
	if !m.enabled {
		return nil
	}
	out := make([]DaemonStatus, 0, len(m.daemons))
	for _, rec := range m.daemons {
		st := DaemonStatus{
			Name:                          rec.daemon.Name(),
			Description:                   rec.daemon.Description(),
			Enabled:                       !rec.disabled.Load(),
			Tick:                          rec.tick,
			ProcessStartedAt:              rec.processStartedAt,
			ConsecutiveCLIFailures:        int(rec.consecutiveCLIFailures.Load()),
			ConsecutiveValidationFailures: int(rec.consecutiveValidationFailures.Load()),
		}
		if last, ok := rec.runs.Latest(); ok {
			st.LastRun = &last
		}
		st.RunsTotal = rec.runs.Len()
		out = append(out, st)
	}
	return out
}

// SetCallbacks installs (or replaces) the OnRunStarted / OnRunEnded
// hooks.  Safe to call after Start — main.go uses this so the WS hub
// (which is built after the Manager) can wire daemon_run_* broadcasts.
//
// Pass nil to clear a hook.  Either argument may be nil independently.
func (m *Manager) SetCallbacks(onRunStarted func(DaemonRunStartedEvent), onRunEnded func(DaemonRunEndedEvent)) {
	m.hookMu.Lock()
	defer m.hookMu.Unlock()
	m.onRunStarted = onRunStarted
	m.onRunEnded = onRunEnded
}

func (m *Manager) loadOnRunStarted() func(DaemonRunStartedEvent) {
	m.hookMu.RLock()
	defer m.hookMu.RUnlock()
	return m.onRunStarted
}

func (m *Manager) loadOnRunEnded() func(DaemonRunEndedEvent) {
	m.hookMu.RLock()
	defer m.hookMu.RUnlock()
	return m.onRunEnded
}

// DaemonStatus is the public read-only view of a daemon's state.
// Mirrors the JSON shape the dashboard endpoint emits (see RFC §9.2).
type DaemonStatus struct {
	Name                          string        `json:"name"`
	Description                   string        `json:"description"`
	Enabled                       bool          `json:"enabled"`
	Tick                          time.Duration `json:"tick"`
	ProcessStartedAt              time.Time     `json:"process_started_at"`
	LastRun                       *DaemonRun    `json:"last_run,omitempty"`
	RunsTotal                     int           `json:"runs_total"`
	ConsecutiveCLIFailures        int           `json:"consecutive_cli_failures"`
	ConsecutiveValidationFailures int           `json:"consecutive_validation_failures"`
}

// runDaemonLoop is the per-daemon goroutine body.  Picks up an initial
// jitter delay so all daemons don't fire simultaneously at t=0, then
// drives the ticker until ctx cancellation.
//
// time.NewTimer + Stop on the jitter (NOT time.After) so a fast-shutdown
// case doesn't leak the timer past goroutine exit.  RFC v2.1 §5.1.
func (m *Manager) runDaemonLoop(rec *daemonRecord) {
	defer m.wg.Done()

	// Jitter range = [0, tick).  Done before the first tick so daemons
	// with similar tick periods (e.g. two daemons both at 30s) don't
	// pile up in lockstep.
	if rec.tick > 0 {
		// rec.tick is at least 1ns by construction; mrand.Int64N panics
		// on n<=0, so the guard above is required.
		delay := time.Duration(mrand.Int64N(int64(rec.tick)))
		jitter := time.NewTimer(delay)
		select {
		case <-m.ctx.Done():
			jitter.Stop()
			return
		case <-jitter.C:
		}
	}

	ch, stop := m.tickFn(rec.tick)
	defer stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ch:
			if rec.disabled.Load() {
				continue // silently skip disabled (post-breaker) ticks
			}
			m.runOnce(rec, DaemonTriggerScheduled)
		}
	}
}

// runOnce executes one Tick on rec.  Combines the CAS gate, panic
// recovery, ctx-with-timeout, and run recording into a single
// well-ordered defer.
//
// The single combined defer (panic recovery + inflight reset +
// recordRun) is intentional:  splitting them across multiple defers
// creates an ordering bug where panic-recover may run before
// inflight.Store(false), potentially leaving the daemon's CAS gate
// stuck open.  Keep this in one place.  RFC v2.1 §5.1.
func (m *Manager) runOnce(rec *daemonRecord, trigger DaemonTriggerKind) {
	if !rec.inflight.CompareAndSwap(false, true) {
		slog.Debug("sysession: skipping overlapping tick",
			"daemon", rec.daemon.Name())
		return
	}
	runID := newRunID()
	startedAt := time.Now()

	if cb := m.loadOnRunStarted(); cb != nil {
		cb(DaemonRunStartedEvent{
			Name:      rec.daemon.Name(),
			RunID:     runID,
			Trigger:   trigger,
			StartedAt: startedAt,
		})
	}

	var (
		report  TickReport
		tickErr error
		isPanic bool
	)

	// R232-GO-5/GO-6: declare tickCtx + defer cancel BEFORE the combined
	// recover/inflight/recordRun defer. defer is LIFO, so this layout
	// makes the combined defer run first (handles panic + clears CAS +
	// records the run), then cancel() releases the timeout context.
	// Reversing the order leaves any goroutine reading tickCtx during
	// recordRun observing a context that was already cancelled by the
	// inner defer — easy to misclassify as DaemonErrorClassCanceled.
	tickCtx, cancel := context.WithTimeout(m.ctx, m.cfg.TickTimeout)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			isPanic = true
			tickErr = fmt.Errorf("sysession: daemon %q panicked: %v",
				rec.daemon.Name(), r)
			slog.Error("sysession: daemon panic",
				"daemon", rec.daemon.Name(), "recover", r)
		}
		// inflight reset MUST happen after recover so a panicking Tick
		// can't permanently jam the CAS gate.  The single combined
		// defer (recover + Store + recordRun) makes this ordering
		// observable in one place.
		rec.inflight.Store(false)
		m.recordRun(rec, runID, trigger, startedAt, report, tickErr, isPanic)
	}()

	report, tickErr = rec.daemon.Tick(tickCtx)
}

// recordRun writes the DaemonRun to the per-daemon ring, updates the
// failure counters, trips the breaker if needed, and fires OnRunEnded.
//
// Failure-class counters (RFC §7.4):
//
//   - Upstream / Panic:  consecutiveCLIFailures++.  At ≥ limit, set
//     rec.disabled = true.
//   - Validation:  consecutiveValidationFailures++.  Does NOT trip the
//     breaker (one bad candidate shouldn't kill the daemon).
//   - Timeout:  surfaced for observability.  Does NOT trip the breaker
//     and does NOT clear other counters (a stretch of timeouts followed
//     by an upstream error would still trigger the breaker correctly).
//   - Success:  resets BOTH counters.
func (m *Manager) recordRun(rec *daemonRecord, runID string, trigger DaemonTriggerKind,
	startedAt time.Time, report TickReport, err error, isPanic bool) {
	endedAt := time.Now()
	state, class := classifyError(err, isPanic)
	dr := DaemonRun{
		RunID:      runID,
		Name:       rec.daemon.Name(),
		State:      state,
		Trigger:    trigger,
		StartedAt:  startedAt,
		EndedAt:    endedAt,
		DurationMS: endedAt.Sub(startedAt).Milliseconds(),
		ErrorClass: class,
		Stats:      flattenTickReport(report),
	}
	if err != nil {
		dr.ErrorMsg = err.Error()
	}
	rec.runs.Append(dr)

	switch class {
	case DaemonErrorClassNone:
		rec.consecutiveCLIFailures.Store(0)
		rec.consecutiveValidationFailures.Store(0)
	case DaemonErrorClassValidation:
		rec.consecutiveValidationFailures.Add(1)
	case DaemonErrorClassUpstream, DaemonErrorClassPanic:
		failures := rec.consecutiveCLIFailures.Add(1)
		if failures >= consecutiveCLIFailureLimit && rec.disabled.CompareAndSwap(false, true) {
			slog.Error("sysession: circuit breaker tripped",
				"daemon", rec.daemon.Name(),
				"consecutive_failures", failures,
				"last_error", err)
		}
	case DaemonErrorClassTimeout:
		// Intentionally do nothing — see comment above.  Timeouts
		// surface via the run record (state=DaemonRunTimedOut) without
		// touching counters.
	}

	if cb := m.loadOnRunEnded(); cb != nil {
		cb(DaemonRunEndedEvent{
			Name:       rec.daemon.Name(),
			RunID:      runID,
			State:      dr.State,
			DurationMS: dr.DurationMS,
			ErrorClass: dr.ErrorClass,
			Trigger:    dr.Trigger,
		})
	}
}

// flattenTickReport converts a TickReport into the Stats map shape
// stored on DaemonRun.  Returns nil for a fully-zero report so the
// JSON serialisation omits the field.
func flattenTickReport(r TickReport) map[string]int64 {
	if r.Examined == 0 && r.Acted == 0 && len(r.Skipped) == 0 {
		return nil
	}
	out := make(map[string]int64, 2+len(r.Skipped))
	if r.Examined != 0 {
		out["examined"] = int64(r.Examined)
	}
	if r.Acted != 0 {
		out["acted"] = int64(r.Acted)
	}
	for k, v := range r.Skipped {
		out["skipped_"+k] = int64(v)
	}
	return out
}
