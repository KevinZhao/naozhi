package sysession

import (
	"context"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

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
	enabled   bool
	cfg       Config
	tickFn    tickerFactory
	daemons   []*daemonRecord
	wg        sync.WaitGroup
	ctx       context.Context
	cancel    context.CancelFunc
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
		enabled: cfg.Enabled,
		cfg:     cfg,
		tickFn:  cfg.NewTicker,
	}
	if !cfg.Enabled {
		// Build nothing; Start is a no-op.
		return m, nil
	}
	if cfg.Router == nil {
		return nil, fmt.Errorf("sysession: NewManager requires Router when enabled")
	}

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
			tick = 30 * time.Second // ultimate default
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
	started := false
	m.startOnce.Do(func() {
		m.ctx, m.cancel = context.WithCancel(parent)
		for _, rec := range m.daemons {
			m.wg.Add(1)
			go m.runDaemonLoop(rec)
		}
		slog.Info("sysession: manager started", "daemons", len(m.daemons))
		started = true
	})
	if !started {
		panic("sysession: Manager.Start called twice")
	}
}

// Stop cancels the daemon ctx and waits for all goroutines to finish.
// stopCtx bounds the wait — when it expires before goroutines drain, we
// PANIC rather than leaking goroutines that may still call into Router
// after Router.Stop.  RFC v2.1 §5.2:  Tick must honour ctx; if it
// doesn't, the daemon is broken and the operator should hear about it
// loudly at shutdown rather than silently corrupting state.
//
// Stop is idempotent.  Subsequent calls are no-ops.
func (m *Manager) Stop(stopCtx context.Context) {
	if !m.enabled {
		return
	}
	m.stopOnce.Do(func() {
		m.cancel()
		done := make(chan struct{})
		go func() {
			m.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
			slog.Info("sysession: manager stopped cleanly")
		case <-stopCtx.Done():
			panic("sysession: Stop deadline exceeded; daemons did not honour ctx — this is a daemon bug, not a transient error")
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

	if m.cfg.OnRunStarted != nil {
		m.cfg.OnRunStarted(DaemonRunStartedEvent{
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

	tickCtx, cancel := context.WithTimeout(m.ctx, m.cfg.TickTimeout)
	defer cancel()

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

	if m.cfg.OnRunEnded != nil {
		m.cfg.OnRunEnded(DaemonRunEndedEvent{
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
