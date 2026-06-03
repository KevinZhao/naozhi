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
)

// osExit was previously the default Stop deadline-exceeded exit hook,
// indirected through a package var so tests could swap it for panic
// recovery. Per #1287 (R20260527-GO-5) the default OnHardFail now binds
// os.Exit directly so a swap of this var does NOT bleed into Manager
// instances that left cfg.OnHardFail unset. Tests that want to observe
// the hard-fail path MUST set cfg.OnHardFail explicitly (e.g. wrap this
// var, or supply their own no-op / panic-recovery func). Kept exported
// at package scope only because legacy in-pkg tests may still wire
// through it via cfg.OnHardFail = func(c int) { osExit(c) }.
var osExit = os.Exit

// StopPolicyForceExit is the documented Stop-overflow strategy this
// Manager honours: when the per-call ctx expires with daemons still in
// flight, Stop fires OnHardFail (default os.Exit(2)). The process exits
// rather than leaking goroutines that could touch a torn-down router.
//
// Why this is a string constant rather than a typed enum: cron uses
// StopPolicyBudgetThenLeak (see internal/cron/scheduler.go) and the
// divergence is a deliberate security decision (Sec-LOW-2 / RFC
// system-session.md §5.2). sysession daemons run user-prompt-derived
// strings through a CLI subprocess; a stuck goroutine touching a
// torn-down router would risk echoing conversation excerpts into a
// different session's reply path. cron deliveries pass through dispatch's
// outbound retry which re-resolves the active session, so cron's
// budget+leak strategy is safe. Aligning the two policies would
// require reopening Sec-LOW-2; do not "harmonise" without revisiting it.
//
// Closes #1060 (R244-ARCH-7) — promotes the implicit decision (lived
// only in Stop's godoc) to a typed constant operators can reference in
// alerts / runbooks. NOT used in sysession's control flow today;
// intentionally doc-only so future "let's check policy at runtime"
// callers must add the comparison and its tests deliberately.
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

	// Runner is the LLM-call abstraction shared by all daemons.
	// Required when Enabled and at least one daemon is enabled — daemons
	// that don't actually call an LLM (future TransientSweeper) can
	// safely ignore the Runner.
	Runner Runner

	// Router is the session router subset the daemons need.  Required
	// when Enabled.  Accepts the producer-side RawSystemSessionRouter
	// (satisfied directly by *session.Router); NewManager wraps it into
	// the cli-free SystemSessionRouter the daemons consume
	// (R260528-ARCH-9 / #1370).
	Router RawSystemSessionRouter

	// Daemons is the per-daemon config map.  Key is daemon name (must
	// match an entry in builtinDaemons).  Value carries enable flag +
	// tick interval + daemon-specific knobs.
	Daemons map[string]DaemonRuntimeConfig

	// WorkspaceRoots enumerates the distinct workspace roots the
	// attachment-gc daemon sweeps. Optional — only the attachment-gc
	// daemon consumes it; nil disables that daemon's work (it logs and
	// no-ops). Wired in cmd/naozhi from router default workspace +
	// per-chat overrides + project paths (docs/rfc/attachment-gc-daemon.md
	// §4.4). Kept out of Router because it spans router + project manager.
	WorkspaceRoots WorkspaceRootLister

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

	// OnHardFail is invoked from Stop when stopCtx expires before
	// daemons drain. Defaults to os.Exit (bound directly, not through
	// the osExit package var — see #1287 / R20260527-GO-5).
	// Embedders that wrap sysession in a larger process (tests,
	// future supervisor that hosts cron + sysession + server in one
	// binary) can override this to shut down cleanly without taking
	// the whole process down. Signature mirrors os.Exit so the
	// default is a one-line wrapper. R240-ARCH-22.
	OnHardFail func(code int)
}

// DaemonRuntimeConfig is the common-shape per-daemon runtime knobs
// every built-in daemon understands.  Daemon-specific fields are
// passed via Daemons[name].Specific (DaemonConfig).
type DaemonRuntimeConfig struct {
	Enabled bool
	Tick    time.Duration
	// RunOnStart makes runDaemonLoop fire one Tick immediately at
	// startup, BEFORE the initial jitter + ticker loop. Without it a
	// daemon's first Tick lands one full tick interval later (plus
	// jitter), and a process that restarts more often than the tick
	// interval may never tick at all. Low-frequency sweeper daemons
	// (e.g. attachment-gc at 6h) need this; high-frequency daemons
	// (auto-titler at 30s) generally don't. See
	// docs/rfc/attachment-gc-daemon.md §4.6-3.
	RunOnStart bool
	Specific   DaemonConfig
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

	// runOnStart mirrors DaemonRuntimeConfig.RunOnStart: fire one Tick
	// at startup before the jitter+ticker loop. See DaemonRuntimeConfig.
	runOnStart bool
}

// ctxCancel bundles the daemon-loop context with its cancel function so
// both are published through a single atomic.Pointer store. Folding them
// together (R20260603-GO-4, #1653) eliminates the window that existed when
// ctx was a plain struct field written before the cancel pointer was
// atomically stored: a concurrent Stop could observe the cancel pointer as
// nil while the freshly-written ctx was already driving spawned goroutines.
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
	// lifeP holds the daemon-loop ctx + its cancel function once Start
	// has installed them and spawned goroutines. nil pointer ⇒ Start has
	// not run; non-nil ⇒ ctx is live and cancellable. R242-GO-6 replaced
	// the previous "started atomic.Bool + plain cancel field" pair with a
	// single atomic.Pointer; R20260603-GO-4 (#1653) further folds the
	// plain `ctx` field into the same atomic so there is no window between
	// the ctx field write and the cancel publish — a concurrent Stop that
	// observes a non-nil pointer is guaranteed to see a fully-populated
	// ctx+cancel, and runDaemonLoop reads the same single source of truth.
	// The atomic store/load is the single happens-before edge between
	// Start, Stop, and the daemon goroutines.
	lifeP atomic.Pointer[ctxCancel]

	// Lifecycle hooks.  Held under atomic.Pointer so SetCallbacks
	// (called late during startup wiring, after Hub is built) doesn't
	// race recordRun callers. Reads happen on every Tick (twice — once
	// at run start, once at run end); writes are once or twice during
	// init.
	//
	// R242-GO-16 + R246-ARCH-6 alignment: previously held under a
	// sync.RWMutex with plain function fields. Switched to
	// atomic.Pointer[holder] to (a) drop the lock-acquire pair on every
	// Tick read, (b) match the upstream/connector.go pattern where
	// SetDiscoverFunc / SetPreviewFunc already use atomic.Pointer for
	// the identical "wired late, read often" lifecycle, and (c) let
	// the race detector enforce the invariant rather than relying on a
	// "main is single-threaded" doc comment. See holder type docs in
	// hook_holders.go for why we wrap the function values in a struct
	// (atomic.Pointer needs a concrete pointee type).
	onRunStarted atomic.Pointer[onRunStartedHolder]
	onRunEnded   atomic.Pointer[onRunEndedHolder]

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
	// R240-ARCH-22 / #1287 (R20260527-GO-5): caller-overridable hard-fail
	// hook. Default binds os.Exit directly (NOT through the osExit pkg
	// var) so a test that swaps osExit AND constructs another Manager
	// without supplying cfg.OnHardFail won't see this Manager's "default"
	// closure read the swapped osExit at call time. Tests that need to
	// observe the hard-fail path MUST supply cfg.OnHardFail explicitly;
	// the osExit pkg-var is reserved for the legacy in-pkg test patterns
	// that wire NewManager directly without setting cfg.OnHardFail and
	// rely on the caller-side var swap (see osExit godoc).
	if cfg.OnHardFail == nil {
		cfg.OnHardFail = os.Exit
	}

	m := &Manager{
		enabled: cfg.Enabled,
		cfg:     cfg,
		tickFn:  cfg.NewTicker,
	}
	// Route initial callbacks through SetCallbacks so onRunStarted /
	// onRunEnded have a single write path (R242-GO-16). The constructor
	// is the only caller before publication, so the lock acquired by
	// SetCallbacks is uncontended; the symmetry vs the late re-bind from
	// main.go (when Hub is finally available) avoids the previous
	// "constructor writes plain field, runtime writes via mutex" race
	// shape that left the lock contract one-sided.
	m.SetCallbacks(cfg.OnRunStarted, cfg.OnRunEnded)
	if !cfg.Enabled {
		// Build nothing; Start is a no-op.
		return m, nil
	}
	if cfg.Router == nil {
		return nil, fmt.Errorf("sysession: NewManager requires Router when enabled")
	}
	// Adapt the producer-side router into the cli-free daemon-facing
	// interface once; every daemon below gets the wrapped form so none
	// of the daemon code path references internal/cli (R260528-ARCH-9 /
	// #1370).
	daemonRouter := wrapRouter(cfg.Router)

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
			Router:         daemonRouter,
			Runner:         cfg.Runner,
			Cfg:            runtime.Specific,
			WorkspaceRoots: cfg.WorkspaceRoots,
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
			runOnStart:       runtime.RunOnStart,
		})
	}
	return m, nil
}

// Start launches one goroutine per enabled daemon. Start is idempotent:
// the second and subsequent calls are no-ops — sync.Once guards the
// goroutine spawn and a Warn line records the redundant call so an
// embedder mis-wiring is still visible without process-killing
// semantics.
//
// R260528-ARCH-16 (#1377): pre-fix this panic'd on the second call.
// That diverged from cron.Scheduler.Start (CAS-idempotent, returns nil
// on the second call) and made embedders that wrap naozhi as a library
// fragile: a parent restarting only the network layer could trigger
// SIGABRT here despite the wrapper having no clean way to coordinate
// the start lifecycle. Aligning with the cron CAS pattern keeps the
// "logic error in calling code" loud (slog.Warn) without taking the
// host process down.
//
// Returns immediately; daemons run asynchronously. Callers should
// invoke Stop during shutdown.
//
// R260528-ARCH-13 (#1374): a nil parent ctx used to panic deep inside
// context.WithCancel (which requires non-nil parent per its contract).
// The wireup path (internal/wireup/schedulers.go) already passes a real
// ctx, but library embedders that forward a zero-value ctx through
// helper layers would crash with an unhelpful stack. Fall back to
// context.Background() with a Warn so the misuse is loud but the daemon
// goroutines still come up — the eventual Stop(stopCtx) cancels via
// m.lifeP regardless of which parent was used at Start.
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
		// Publish ctx+cancel as one atomic store BEFORE spawning the
		// daemon goroutines (R20260603-GO-4, #1653). The store is the
		// single happens-before edge for Start↔Stop and Start↔daemon:
		// a concurrent Stop now either observes nil (no goroutines exist
		// yet, nothing to cancel) or a fully-populated life (ctx is live
		// and cancellable). The goroutines receive `life` by value at
		// spawn, so each daemon's ctx read is ordered after this store
		// via goroutine-creation happens-before. There is no longer a
		// window where ctx is live but cancel is unpublished.
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
	// R232-GO-3 / R242-GO-6 / R20260603-GO-4: short-circuit when Start was
	// never called. lifeP (ctx+cancel) is published inside startOnce.Do as a
	// single atomic store BEFORE goroutines spawn, so the only way to observe
	// nil here is "Start has not entered its critical section yet" — in that
	// case there are no goroutines in flight and no daemons to cancel and
	// no wg slots to drain.
	//
	// R20260527122801-GO-003: do NOT consume stopOnce on this early path.
	// Burning stopOnce here would silently disarm a later legitimate Stop:
	// in a Stop→Start→Stop sequence the second Stop would observe the
	// already-fired stopOnce and skip cancelling daemon ctx, leaking the
	// daemon goroutines until process exit. Returning without touching
	// stopOnce keeps the early path a true no-op so a real Stop after a
	// successful Start still cancels the daemon ctx exactly once.
	life := m.lifeP.Load()
	if life == nil {
		return
	}
	m.stopOnce.Do(func() {
		// life was loaded above and is published as a single atomic
		// store inside Start (ctx+cancel together) — the load is the
		// happens-before pair for that store. R20260603-GO-4 (#1653):
		// observing non-nil life guarantees cancel is valid, so there
		// is no window where we could see a live ctx but a nil cancel.
		life.cancel()
		done := make(chan struct{})
		// R234-GO-5: this watcher goroutine is intentionally not tracked
		// in any WaitGroup. The only termination path on the stopCtx
		// timeout branch is osExit(2), which terminates the entire
		// process — abandoning the goroutine is therefore safe in
		// production.
		//
		// CAVEAT for test harnesses that swap osExit (see osExit pkg
		// var): if osExit is replaced with panic-recovery or no-op, this
		// goroutine will block on m.wg.Wait() until some daemon finally
		// returns, leaking a goroutine for the lifetime of the test
		// binary. Tests that swap osExit MUST also drive every spawned
		// daemon to return so wg.Wait can complete. Do not "fix" this
		// by adding a context to wg.Wait — the production semantic is
		// "block forever or kill the process", and weakening it lets
		// stuck-daemon shutdowns silently torn-down the router.
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
			// R240-ARCH-22: dispatch through the configurable hook so
			// embedders aren't forced to swap a package-level var to
			// avoid taking the host process down. Default hook still
			// calls osExit(2) — semantics unchanged for naozhi binary.
			//
			// #1286 (R20260527-COR-6): isolate the call in a recover
			// frame. The default os.Exit never returns and never panics,
			// but a test-supplied OnHardFail might panic. Without recover
			// the panic propagates out of stopOnce.Do, leaves stopOnce
			// already-fired, and (depending on the call site) leaks the
			// watcher goroutine spawned above which is still parked on
			// m.wg.Wait(). Logging the panic and returning normally lets
			// Stop callers observe a clean return — they were going to be
			// terminated anyway in the production default path.
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

// SetCallbacks installs (or replaces) the OnRunStarted / OnRunEnded
// hooks.  Safe to call after Start — main.go uses this so the WS hub
// (which is built after the Manager) can wire daemon_run_* broadcasts.
//
// Pass nil to clear a hook.  Either argument may be nil independently.
//
// R246-ARCH-6 alignment: implemented via atomic.Pointer.Store so
// concurrent Set / load sequences are well-defined without a mutex.
// The two hook fields are stored independently — passing one nil and
// one non-nil clears one without affecting the other.
func (m *Manager) SetCallbacks(onRunStarted func(DaemonRunStartedEvent), onRunEnded func(DaemonRunEndedEvent)) {
	if onRunStarted == nil {
		m.onRunStarted.Store(nil)
	} else {
		m.onRunStarted.Store(&onRunStartedHolder{fn: onRunStarted})
	}
	if onRunEnded == nil {
		m.onRunEnded.Store(nil)
	} else {
		m.onRunEnded.Store(&onRunEndedHolder{fn: onRunEnded})
	}
}

// loadOnRunStarted returns the currently installed start hook, or nil
// if none is set. Lock-free; safe from any goroutine.
func (m *Manager) loadOnRunStarted() func(DaemonRunStartedEvent) {
	if h := m.onRunStarted.Load(); h != nil {
		return h.fn
	}
	return nil
}

// loadOnRunEnded returns the currently installed end hook, or nil if
// none is set. Lock-free; safe from any goroutine.
func (m *Manager) loadOnRunEnded() func(DaemonRunEndedEvent) {
	if h := m.onRunEnded.Load(); h != nil {
		return h.fn
	}
	return nil
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
func (m *Manager) runDaemonLoop(rec *daemonRecord, life *ctxCancel) {
	defer m.wg.Done()
	// life is passed by Start at spawn time (R20260603-GO-4, #1653); reading
	// life.ctx here is ordered after Start's atomic store via goroutine-
	// creation happens-before, so no atomic load is needed on this hot path.
	ctx := life.ctx

	// RunOnStart: fire one Tick immediately, before the jitter+ticker
	// loop. Low-frequency sweepers (attachment-gc) need a deterministic
	// startup run so a process that restarts more often than its tick
	// interval still makes progress. Honour ctx + breaker first so a
	// cancelled/disabled daemon doesn't tick. See DaemonRuntimeConfig.
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

	// Jitter range = [0, tick).  Done before the first tick so daemons
	// with similar tick periods (e.g. two daemons both at 30s) don't
	// pile up in lockstep.
	if rec.tick > 0 {
		// rec.tick is at least 1ns by construction; mrand.Int64N panics
		// on n<=0, so the guard above is required.
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

// runOnce executes one Tick on rec.  Combines the CAS gate, panic
// recovery, ctx-with-timeout, and run recording into a single
// well-ordered defer.
//
// The single combined defer (panic recovery + inflight reset +
// recordRun) is intentional:  splitting them across multiple defers
// creates an ordering bug where panic-recover may run before
// inflight.Store(false), potentially leaving the daemon's CAS gate
// stuck open.  Keep this in one place.  RFC v2.1 §5.1.
func (m *Manager) runOnce(ctx context.Context, rec *daemonRecord, trigger DaemonTriggerKind) {
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
	tickCtx, cancel := context.WithTimeout(ctx, m.cfg.TickTimeout)
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
		// R260528-BUG-21: belt-and-braces sanitisation on the persisted
		// error message. runner.go already SanitizeForLog's stderr before
		// fmt.Errorf wraps it (see "sysession: runner stderr" path), but
		// other err sources reaching recordRun (timeouts, panics-as-error,
		// validation errors carrying user-supplied strings) bypass that
		// hop, so a control rune or oversized payload can land in the
		// run-history JSONL and propagate to the dashboard / log via the
		// DaemonRunEnded callback. Sanitise here as the centralised gate.
		// 1024 cap matches the dashboard run-history line budget — long
		// enough to preserve the meaningful tail of a wrapped error chain
		// without letting a misbehaving daemon write multi-KB strings to
		// every run record.
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
		// Intentionally do nothing — see comment above.  Timeouts
		// surface via the run record (state=DaemonRunTimedOut) without
		// touching counters.
	case DaemonErrorClassCanceled:
		// Same as Timeout — Canceled means the operator shut us down
		// or naozhi is restarting, NOT that the daemon is broken.
		// State=DaemonRunCanceled records the event without touching
		// the breaker counters or the success counters (so a long
		// success streak is not nuked by an orderly shutdown). R236-QA-05.
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
