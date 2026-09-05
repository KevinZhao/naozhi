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

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/runtelemetry"
)

// osExit is the legacy test hook for the Stop hard-fail path. The default
// OnHardFail binds os.Exit directly, so swapping this var only affects
// Managers whose cfg.OnHardFail explicitly routes through it (#1287).
var osExit = os.Exit

// StopPolicyForceExit names the Stop-overflow strategy this Manager
// honours: when stopCtx expires with daemons still in flight, Stop fires
// OnHardFail (default os.Exit(2)) rather than leaking goroutines that
// could touch a torn-down router. Doc-only; not consulted at runtime (#1060).
//
// Deliberately diverges from cron's StopPolicyBudgetThenLeak (see
// RFC system-session.md §5.2): sysession daemons run user-conversation
// excerpts through a CLI subprocess and a stuck goroutine could echo them
// into another session's reply path; cron deliveries re-resolve the active
// session via dispatch's outbound retry, so leaking is safe there.
const StopPolicyForceExit = "force_exit"

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

	// Runner is the LLM-call abstraction shared by all daemons. Required
	// when Enabled and at least one daemon calls an LLM.
	Runner Runner

	// Router is the session router subset the daemons need. Required when
	// Enabled. NewManager wraps the producer-side RawSystemSessionRouter
	// (*session.Router) into the cli-free SystemSessionRouter (#1370).
	Router RawSystemSessionRouter

	// Daemons is the per-daemon config map.  Key is daemon name (must
	// match an entry in builtinDaemons).  Value carries enable flag +
	// tick interval + daemon-specific knobs.
	Daemons map[string]DaemonRuntimeConfig

	// WorkspaceRoots enumerates the workspace roots the attachment-gc
	// daemon sweeps; nil makes that daemon log and no-op. Kept out of
	// Router because it spans router + project manager.
	WorkspaceRoots WorkspaceRootLister

	// NewTicker is an optional test injection point; nil means
	// time.NewTicker. Tests return a channel they poke to drive runOnce.
	NewTicker tickerFactory

	// OnHardFail is invoked from Stop when stopCtx expires before daemons
	// drain. Defaults to os.Exit; embedders hosting sysession in a larger
	// process can override it to shut down without killing the process.
	OnHardFail func(code int)
}

// DaemonRuntimeConfig is the common-shape per-daemon runtime knobs
// every built-in daemon understands.  Daemon-specific fields are
// passed via Daemons[name].Specific (DaemonConfig).
type DaemonRuntimeConfig struct {
	Enabled bool
	Tick    time.Duration
	// RunOnStart fires one Tick immediately at startup, before the jitter +
	// ticker loop, so a low-frequency sweeper (attachment-gc at 6h) makes
	// progress even when the process restarts more often than its tick
	// interval (docs/rfc/attachment-gc-daemon.md §4.6-3).
	RunOnStart bool
	Specific   DaemonConfig
}

const (
	defaultTickTimeout = 30 * time.Second

	// defaultDaemonTickInterval is the tick cadence when config leaves Tick
	// zero/negative (distinct from defaultTickTimeout, the per-Tick budget).
	defaultDaemonTickInterval = 30 * time.Second

	// consecutiveCLIFailureLimit is the breaker threshold (RFC §7.4): this
	// many CLI/panic failures in a row disable the daemon until restart.
	// Validation/timeout failures do not count.
	consecutiveCLIFailureLimit = 5
)

// tickerFactory abstracts time.NewTicker so tests can drive ticks on
// demand.  Returns the channel + a stop closure (must be invoked).
type tickerFactory func(d time.Duration) (<-chan time.Time, func())

func stdTickerFactory(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// daemonRecord is the per-daemon runtime state. Held by pointer so the
// atomic fields never relocate and runDaemonLoop shares one record with
// the rest of Manager.
type daemonRecord struct {
	daemon Daemon
	tick   time.Duration

	// inflight is the per-daemon overlap gate: atomic.Bool rather than a
	// mutex so overlapping ticks are skipped, not queued behind a stuck Tick.
	inflight atomic.Bool

	// disabled is set when the breaker trips; ticks then short-circuit.
	disabled atomic.Bool

	// Failure counters.  Atomic so the dashboard endpoint can read them
	// without taking Manager.mu.
	consecutiveCLIFailures        atomic.Int32
	consecutiveValidationFailures atomic.Int32

	// processStartedAt lets the dashboard distinguish "no run since process
	// start" from "never ran"; identical for every record on a Manager.
	processStartedAt time.Time

	// runs holds the per-daemon ring buffer of completed DaemonRuns.
	runs *runRing

	// runOnStart mirrors DaemonRuntimeConfig.RunOnStart.
	runOnStart bool
}

// ctxCancel bundles the daemon-loop ctx with its cancel so both publish
// through one atomic store; a concurrent Stop can never observe a live
// ctx with a nil cancel (#1653).
type ctxCancel struct {
	ctx    context.Context
	cancel context.CancelFunc
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
	// lifeP holds ctx+cancel once Start has spawned goroutines; nil ⇒ Start
	// has not run. The atomic store/load is the single happens-before edge
	// between Start, Stop and the daemon goroutines (#1653).
	lifeP atomic.Pointer[ctxCancel]

	// telemetry is the host's runtelemetry.Broadcaster (#1723), the same
	// seam cron uses. atomic.Pointer because SetTelemetry is wired late
	// (after the Hub is built) and races emitRun* reads on every Tick;
	// nil ⇒ emit* is a silent no-op. Same shape as cron.Scheduler.telemetry.
	telemetry atomic.Pointer[runtelemetry.Broadcaster]

	startOnce sync.Once
	stopOnce  sync.Once
}

// NewManager builds a Manager from cfg: validates the built-in daemon
// list, builds enabled daemons and pre-allocates their runRings.
// cfg.Enabled=false yields a Manager whose Start is a no-op. Errors only
// when the configuration is internally inconsistent; a missing Tick
// interval defaults silently.
func NewManager(cfg Config) (*Manager, error) {
	validateBuiltinDaemonNames()

	if cfg.NewTicker == nil {
		cfg.NewTicker = stdTickerFactory
	}
	if cfg.TickTimeout <= 0 {
		cfg.TickTimeout = defaultTickTimeout
	}
	// Default hard-fail hook binds os.Exit directly (not the osExit var) so
	// a test swapping osExit can't leak into Managers that left OnHardFail
	// unset (#1287).
	if cfg.OnHardFail == nil {
		cfg.OnHardFail = os.Exit
	}

	m := &Manager{
		enabled: cfg.Enabled,
		cfg:     cfg,
		tickFn:  cfg.NewTicker,
	}
	// telemetry stays nil until the host wires it via SetTelemetry.
	if !cfg.Enabled {
		// Build nothing; Start is a no-op.
		return m, nil
	}
	if cfg.Router == nil {
		return nil, fmt.Errorf("sysession: NewManager requires Router when enabled")
	}
	// Wrap once so no daemon code path references internal/cli (#1370).
	daemonRouter := wrapRouter(cfg.Router)

	// One timestamp shared by every daemonRecord: a Manager represents
	// one process start.
	now := time.Now()
	for _, factory := range builtinDaemons {
		runtime, ok := cfg.Daemons[factory.Name]
		if !ok || !runtime.Enabled {
			continue
		}
		deps := DaemonDeps{
			Router:         daemonRouter,
			Runner:         cfg.Runner,
			Cfg:            runtime.Specific,
			WorkspaceRoots: cfg.WorkspaceRoots,
		}
		d, err := factory.Build(deps)
		if err != nil {
			return nil, fmt.Errorf("sysession: build daemon %q: %w", factory.Name, err)
		}
		// Configure is the post-construction validation hook.
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
			runOnStart:       runtime.RunOnStart,
		})
	}
	return m, nil
}

// Start launches one goroutine per enabled daemon and returns
// immediately; callers invoke Stop during shutdown. Idempotent: repeat
// calls are a Warn'd no-op, matching cron.Scheduler.Start (#1377). A nil
// parent ctx falls back to context.Background with a Warn instead of
// panicking inside context.WithCancel (#1374); Stop(stopCtx) cancels via
// m.lifeP regardless.
func (m *Manager) Start(parent context.Context) {
	if !m.enabled {
		return
	}
	if parent == nil {
		slog.Warn("sysession: Manager.Start called with nil parent ctx; falling back to context.Background — caller wiring bug",
			"hint", "shutdown still works via Stop(stopCtx); see #1374")
		parent = context.Background()
	}
	startedThisCall := false
	m.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(parent)
		life := &ctxCancel{ctx: ctx, cancel: cancel}
		// Publish ctx+cancel as one atomic store BEFORE spawning goroutines:
		// it is the single happens-before edge for Start↔Stop and
		// Start↔daemon (goroutines receive `life` by value at spawn), so Stop
		// sees either nil or a fully-populated life (#1653).
		m.lifeP.Store(life)
		for _, rec := range m.daemons {
			m.wg.Add(1)
			go m.runDaemonLoop(rec, life)
		}
		slog.Info("sysession: manager started", "daemons", len(m.daemons))
		startedThisCall = true
	})
	if !startedThisCall {
		slog.Warn("sysession: Manager.Start called more than once (idempotent no-op)")
	}
}

// Stop cancels the daemon ctx and waits for all goroutines. When stopCtx
// expires first it logs loudly and fires OnHardFail (default exit 2)
// rather than leaking goroutines that may call into Router after
// Router.Stop (RFC v2.1 §5.2). Exit, not panic: a panic would dump
// goroutine stacks carrying in-flight buildExcerpt strings (user
// conversation fragments) into container logs (RFC §9.4),
// and exit code 2 is a discriminable signal to systemd. The divergence
// from cron's budget+leak Stop is deliberate; see StopPolicyForceExit.
//
// Stop is idempotent.
func (m *Manager) Stop(stopCtx context.Context) {
	if !m.enabled {
		return
	}
	// lifeP is published inside startOnce.Do BEFORE goroutines spawn, so
	// nil here means Start never ran: nothing to cancel or drain. Do NOT
	// consume stopOnce on this path, or a Stop→Start→Stop sequence would
	// skip the real cancel and leak the daemon goroutines.
	life := m.lifeP.Load()
	if life == nil {
		return
	}
	m.stopOnce.Do(func() {
		// Non-nil life guarantees cancel is valid (#1653).
		life.cancel()
		done := make(chan struct{})
		// The watcher goroutine is deliberately untracked: on the timeout
		// branch the process exits. Tests that override OnHardFail with a
		// no-op MUST drive every daemon to return so wg.Wait completes. Do
		// not add a ctx to wg.Wait — "block or kill the process" is the
		// production semantic.
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
			// Call the configurable hook inside a recover frame: os.Exit never
			// returns, but a test-supplied OnHardFail might panic, and an
			// unrecovered panic would leak the watcher parked on m.wg.Wait()
			// (#1286).
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("sysession: OnHardFail panicked; ignoring to avoid leaking Stop watcher",
							"panic", r)
					}
				}()
				m.cfg.OnHardFail(2)
			}()
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

// runDaemonLoop is the per-daemon goroutine body: optional RunOnStart
// tick, initial jitter so daemons don't fire in lockstep at t=0, then
// the ticker until ctx cancellation. time.NewTimer + Stop (not
// time.After) so a fast shutdown doesn't leak the timer (RFC v2.1 §5.1).
func (m *Manager) runDaemonLoop(rec *daemonRecord, life *ctxCancel) {
	defer m.wg.Done()
	// life arrives by value at spawn; reading life.ctx needs no atomic load.
	ctx := life.ctx

	// RunOnStart: honour ctx + breaker so a cancelled/disabled daemon
	// doesn't tick.
	if rec.runOnStart {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if !rec.disabled.Load() {
			m.runOnce(ctx, rec, DaemonTriggerScheduled)
		}
	}

	// Jitter in [0, tick) so daemons with equal periods don't pile up.
	if rec.tick > 0 {
		// mrand.Int64N panics on n<=0, so the guard above is required.
		delay := time.Duration(mrand.Int64N(int64(rec.tick)))
		jitter := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			jitter.Stop()
			return
		case <-jitter.C:
		}
	}

	ch, stop := m.tickFn(rec.tick)
	defer stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			if rec.disabled.Load() {
				continue // silently skip disabled (post-breaker) ticks
			}
			m.runOnce(ctx, rec, DaemonTriggerScheduled)
		}
	}
}

// runOnce executes one Tick on rec. Panic recovery, inflight reset and
// recordRun live in ONE defer on purpose: split across defers, recover
// could run before inflight.Store(false) and leave the CAS gate stuck
// (RFC v2.1 §5.1).
func (m *Manager) runOnce(ctx context.Context, rec *daemonRecord, trigger DaemonTriggerKind) {
	if !rec.inflight.CompareAndSwap(false, true) {
		slog.Debug("sysession: skipping overlapping tick",
			"daemon", rec.daemon.Name())
		return
	}
	runID := newRunID()
	startedAt := time.Now()

	m.emitRunStarted(rec.daemon.Name(), runID, trigger, startedAt)

	var (
		report  TickReport
		tickErr error
		isPanic bool
	)

	// cancel is deferred BEFORE the combined defer (LIFO) so the run is
	// recorded before the timeout ctx is cancelled; reversed, a goroutine
	// reading tickCtx during recordRun would see it already cancelled.
	tickCtx, cancel := context.WithTimeout(ctx, m.cfg.TickTimeout)
	defer cancel()
	// Runner calls inside Tick book their cost to this run; daemons must
	// derive child contexts from the ctx they receive, never Background().
	tickCtx = withRunInfo(tickCtx, rec.daemon.Name(), runID)

	defer func() {
		if r := recover(); r != nil {
			isPanic = true
			tickErr = fmt.Errorf("sysession: daemon %q panicked: %v",
				rec.daemon.Name(), r)
			// The recover value can carry conversation text; scrub before logging.
			slog.Error("sysession: daemon panic",
				"daemon", rec.daemon.Name(),
				"recover", osutil.SanitizeForLog(fmt.Sprintf("%v", r), 256))
		}
		// inflight reset MUST follow recover so a panicking Tick can't jam
		// the CAS gate.
		rec.inflight.Store(false)
		m.recordRun(rec, runID, trigger, startedAt, report, tickErr, isPanic)
	}()

	report, tickErr = rec.daemon.Tick(tickCtx)
}

// recordRun appends the DaemonRun to the ring, updates failure counters,
// trips the breaker and fires emitRunEnded. Counters (RFC §7.4):
//   - Upstream / Panic: consecutiveCLIFailures++; at ≥ limit set disabled.
//   - Validation: consecutiveValidationFailures++; never trips the breaker.
//   - Timeout / Canceled: recorded only; counters untouched, so a timeout
//     streak followed by an upstream error still trips, and an orderly
//     shutdown doesn't reset a success streak.
//   - Success: resets both counters.
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
		// Centralised sanitisation: panics-as-error and validation errors
		// carrying user strings bypass runner.go's stderr scrub and would
		// otherwise reach run-history JSONL and the dashboard. 1024 matches
		// the dashboard run-history line budget.
		dr.ErrorMsg = osutil.SanitizeForLog(err.Error(), 1024)
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
		// Timeouts surface via the run record only.
	case DaemonErrorClassCanceled:
		// Canceled means orderly shutdown, not a broken daemon; leave all
		// counters alone.
	}

	m.emitRunEnded(rec.daemon.Name(), runID, dr.State, dr.DurationMS, dr.ErrorClass, dr.Trigger)
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
