package cli

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/cli/procmeter"
	"github.com/naozhi/naozhi/internal/cliinfo"
	"github.com/naozhi/naozhi/internal/eventlog/ring"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/shim"
	"github.com/naozhi/naozhi/internal/subagent"
)

// ProcessState is cliinfo.ProcessState (see there).
type ProcessState = cliinfo.ProcessState

const (
	StateSpawning = cliinfo.StateSpawning
	StateReady    = cliinfo.StateReady
	StateRunning  = cliinfo.StateRunning
	StateDead     = cliinfo.StateDead
)

const (
	DefaultNoOutputTimeout = cliinfo.DefaultNoOutputTimeout
	DefaultTotalTimeout    = cliinfo.DefaultTotalTimeout
	// maxScannerBufBytes caps a single NDJSON line read from the shim's stdout.
	// Kept 6 MiB below the shim's own 16 MiB per-line cap (internal/shim/server.go
	// maxServerLineBytes) so a shim-side allocator decision never makes this
	// reader silently truncate; the headroom covers coalesced buffered events and
	// base64-image-laden tool_result frames.
	maxScannerBufBytes = 10 * 1024 * 1024

	// maxStdinLineBytes is the largest single NDJSON line forwarded to the shim
	// (which enforces 16 MB per line); headroom is for the shimClientMsg envelope.
	// Exceeding it fails fast with clierr.ErrMessageTooLarge so the dashboard can surface it.
	maxStdinLineBytes = 12 * 1024 * 1024

	// lineBufShrinkThreshold caps the capacity readLoop's lineBuf may retain
	// across iterations before shrinking back to 4 KiB. tool_result payloads and
	// assistant text chunks commonly hit 50-200 KiB, so 256 KiB retains that
	// capacity (one realloc per session, not per event) while still bounding
	// runaway growth from a buggy shim: ~+10 MiB idle RSS at 50 sessions.
	lineBufShrinkThreshold = 256 * 1024
)

// maxPendingSlots caps the per-Process passthrough pending queue; a goroutine-
// leak / memory backstop, not a business limit. Tunable via SetMaxPendingSlots.
const maxPendingSlots = 16

// processCloseTimeout bounds Close() while the shim tears down its listener +
// socket (closeStdin + waitOrKill(5s) + listener.Close + os.Remove, so 8s is
// headroom); on expiry Close falls through to Kill. Var so tests can shorten it.
var processCloseTimeout = 8 * time.Second

// Process manages a CLI subprocess via a shim connection.
type Process struct {
	// link is the connection to the shim (socket, reader, writer, PIDs,
	// heartbeat, replay cursor).
	link     shimLink
	protocol Protocol
	caps     Caps // cached protocol capabilities (immutable after construction)

	// turn is the turn state machine, session ID, turn-done hook and
	// interrupt flags, under one lock.
	turn turnState

	eventCh  chan clievent.Event
	done     chan struct{}
	killCh   chan struct{} // closed by Kill() to unblock readLoop
	killOnce sync.Once

	// lifecycleCtx is canceled when the process exits (readLoop returns or Kill());
	// binds subagent-Resolve goroutines so SIGTERM doesn't leave them spinning
	// (#644). Lazily initialised so &Process{} test fixtures still work.
	lifecycleCtxOnce  sync.Once
	lifecycleCtxValue context.Context

	noOutputTimeout time.Duration
	totalTimeout    time.Duration
	// lastOutputNS is when readLoop last dispatched a CLI stdout frame, as
	// nanoseconds since monoBase (see lastOutputAt); the passthrough watchdog's
	// no-output clock.
	lastOutputNS atomic.Int64
	wdTuning     watchdogTuning

	// acks matches control_request acks (SetModel) to their waiters.
	acks controlAcks

	eventLog *ring.EventLog

	// meter is what the process has reported about itself (cost, context
	// usage, effort, model, binary version, metering, shadow usage).
	meter procmeter.Meter
	// startedAt is when naozhi attached to the process; endHook delivers
	// its ProcessEnd; detached is set by Detach before it lets go, closing
	// by Close before it asks the shim to shut down.
	startedAt time.Time
	endHook   endHook
	detached  atomic.Bool
	closing   atomic.Bool
	// spawnDiags is the gate decisions of this spawn (SpawnDiagsFor), set once
	// by Wrapper.Spawn before readLoop; runtime observation only, never
	// persisted. nil = none.
	spawnDiags atomic.Pointer[[]SpawnDiag]
	// onLiveVersion is invoked by setLiveVersion on each distinct binary version
	// so the owning Wrapper can refresh the global dashboard banner. Assigned once
	// before startReadLoop (no lock); nil for &Process{} test fixtures.
	onLiveVersion func(string)
	// onCodeChange receives each valid system/code_change_published frame so
	// the session can record the PR. Set via SetOnCodeChange at any time.
	onCodeChange atomic.Pointer[func(clievent.CodeChange)]

	// readEventBuf is a reusable backing array for ReadEventInto (#1676), owned
	// exclusively by handleShimStdout on the readLoop goroutine and consumed within
	// the same frame; cap 2 covers ACP's two-event turn-end split.
	readEventBuf [2]clievent.Event

	// adopted latches the outcome of the turn reconnectedMidTurn describes, so a
	// caller that did not issue the Send can still learn how it ended. The flag
	// above answers "may readLoop move this session to Ready?" and is consumed;
	// the latch answers "how did that turn end?" and is kept. See adopted_turn.go.
	adopted adoptedTurn

	// deathReason records why the process died; written once (first-writer-wins
	// CAS) by the path that transitions State→Dead. nil until stored.
	deathReason atomic.Pointer[string]
	// stderrTail is the CLI's last stderr lines (see process_stderr.go).
	stderrTail shim.StderrTail
	// sawOutput is set once the CLI is past startup: it wrote stdout (an
	// event, or an Init handshake line), or naozhi reattached to it
	// (applyReconnectVerdict); exited is the error for a non-zero
	// cli_exited and exitedAt its UnixNano time (exit_classify.go).
	sawOutput atomic.Bool
	exited    atomic.Pointer[clierr.ProcessExitedError]
	exitedAt  atomic.Int64

	// log is a pre-bound logger carrying the "session" attribute; set once by
	// SetSlogKey before the reader goroutines start, so reads are lock-free.
	log atomic.Pointer[slog.Logger]

	// slots is the passthrough slot machinery (SendPassthrough).
	slots sendSlots

	// tools tracks the backend's in-flight tool calls for the watchdog.
	tools inflightTools

	// linker maps parallel-agent task_ids to transcript jsonl paths for the
	// dashboard's agent_events endpoint. Set by InitLinker; nil in test fakes.
	linker *subagent.Linker
	// projectDir is subagent.ProjectDir of the working directory, computed
	// once so a system/init event costs no rune scan + os.UserHomeDir
	// syscall. Atomic because the reconnect path sets it (SetCwdForLinker)
	// while the read loop is already running and reading it.
	projectDir atomic.Pointer[string]
}

// sendSlot tracks one in-flight passthrough Send call: appended to pendingSlots
// atomically with the stdin write, matched to the CLI's replay event by uuid
// (single sender) or text (merged sender), then handed the turn's result.
// canceled is a tombstone (docs/rfc/passthrough-mode.md §5.2.2): a slot whose
// caller left via ctx.Done stays in pendingSlots to keep FIFO order, but fan-out
// books its result instead of delivering it. Atomic so the error paths read it
// lock-free after releasing slotsMu.
type sendSlot struct {
	id       uint64
	uuid     string
	text     string
	priority string // "" | "now" | "next" | "later"
	onEvent  clievent.EventCallback
	resultCh chan *clievent.SendResult
	errCh    chan error

	// Only mutated under Process.slotsMu (atomic.Bool to allow lock-free
	// reads from the error paths outside slotsMu).
	canceled  atomic.Bool
	replayed  bool
	enqueueAt time.Time
}

// isCanceled reads canceled atomically; the error paths use it lock-free
// outside slotsMu, while writes go through slotsMu to stay FIFO-ordered.
func (s *sendSlot) isCanceled() bool {
	return s.canceled.Load()
}

// SetSlogKey records the session key for readLoop / heartbeatLoop log entries.
// Called once before startReadLoop, so Store is race-free with the readers.
func (p *Process) SetSlogKey(key string) {
	if key == "" {
		return
	}
	p.log.Store(slog.Default().With("session", key))
}

// slogger returns the pre-bound logger for readLoop/heartbeatLoop log
// entries. Falls back to slog.Default() if not yet assigned.
func (p *Process) slogger() *slog.Logger {
	if l := p.log.Load(); l != nil {
		return l
	}
	return slog.Default()
}

// Death reason labels (cliinfo.DeathReason*).
const (
	DeathReasonCLIExited           = cliinfo.DeathReasonCLIExited
	DeathReasonShimEOF             = cliinfo.DeathReasonShimEOF
	DeathReasonShimReadErr         = cliinfo.DeathReasonShimReadErr
	DeathReasonShimOversizeThenEOF = cliinfo.DeathReasonShimOversizeThenEOF
	DeathReasonShimOversizeThenErr = cliinfo.DeathReasonShimOversizeThenErr
	DeathReasonReadLoopPanic       = cliinfo.DeathReasonReadLoopPanic
	DeathReasonKilled              = cliinfo.DeathReasonKilled
	DeathReasonNoOutputTimeout     = cliinfo.DeathReasonNoOutputTimeout
	DeathReasonTotalTimeout        = cliinfo.DeathReasonTotalTimeout
)

// setDeathReason records the death reason if not already set. First writer wins
// (CAS, so concurrent death paths such as panic defer vs. cli_exited cannot
// overwrite each other) and the root cause survives a second transition.
func (p *Process) setDeathReason(reason string) {
	if reason == "" {
		return
	}
	fresh := reason
	if p.deathReason.CompareAndSwap(nil, &fresh) {
		return
	}
	// Upgrade path: tolerate an explicit pointer to "" (no path stores it today);
	// a concurrent non-empty writer invalidates our CAS, preserving first-writer-wins.
	if cur := p.deathReason.Load(); cur != nil && *cur == "" {
		_ = p.deathReason.CompareAndSwap(cur, &fresh)
	}
}

// DeathReason returns the recorded death reason, or "" if alive or unset.
func (p *Process) DeathReason() string {
	if ptr := p.deathReason.Load(); ptr != nil {
		return *ptr
	}
	return ""
}

// lifecycleContext returns a context canceled when readLoop's `defer
// close(p.done)` fires or Kill() closes killCh (#644). Lazily initialised so
// &Process{} test fixtures that never call it pay nothing. Safe to share across
// goroutines; callers MUST NOT cancel it — that is wired internally.
func (p *Process) lifecycleContext() context.Context {
	p.lifecycleCtxOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		p.lifecycleCtxValue = ctx
		// Both channels nil (legacy test fixtures): no lifetime signal can ever
		// fire, so cancel synchronously rather than leak the context (#1289);
		// callers see an already-closed Done, i.e. "process is dead".
		if p.done == nil && p.killCh == nil {
			cancel()
			return
		}
		go func() {
			select {
			case <-p.done:
			case <-p.killCh:
			}
			cancel()
		}()
	})
	return p.lifecycleCtxValue
}

// newShimProcess creates a Process connected to a shim.
// The caller must call startReadLoop() after protocol Init.
func newShimProcess(conn net.Conn, reader *bufio.Reader, writer *bufio.Writer,
	proto Protocol, cliPID, shimPID int, noOutputTimeout, totalTimeout time.Duration) *Process {
	p := &Process{
		protocol: proto,
		caps:     ProtocolCaps(proto),
		// 1024 so a TeamCreate fan-out (8 subagents × ~5 events/s) cannot fill the
		// buffer before Send() drains it; drops force the findResultSince fallback (#1355).
		eventCh:         make(chan clievent.Event, 1024),
		done:            make(chan struct{}),
		killCh:          make(chan struct{}),
		noOutputTimeout: noOutputTimeout,
		totalTimeout:    totalTimeout,
		eventLog:        ring.NewEventLog(0),
		startedAt:       time.Now(),
	}
	p.link.init(conn, reader, writer, cliPID, shimPID)
	return p
}

// startReadLoop begins the shim message reader goroutine and heartbeat.
// Initial state is StateReady EXCEPT on the reconnect-mid-turn path (#1778),
// where SpawnReconnect pre-armed reconnectedMidTurn + StateRunning: forcing
// Ready here would let the stray-result CAS consume the flag with wasRunning
// false, stranding the session in Running once SpawnReconnect re-sets it.
func (p *Process) startReadLoop() {
	p.transition(evReadLoopStart)
	go p.readLoop()
	go p.heartbeatLoop()
}

// Alive returns true if the process has not exited.
func (p *Process) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// IsRunning returns true if the process is currently processing a message.
func (p *Process) IsRunning() bool {
	p.turn.mu.RLock()
	defer p.turn.mu.RUnlock()
	return p.turn.state == StateRunning
}

// Kill forcefully terminates the CLI process via shim.
//
// After sending "kill" and closing the conn, SIGUSR2 makes the shim shut down
// immediately (listener.Close + os.Remove(socket)); otherwise it holds the
// socket for its 30s disconnect grace and the next StartShim for the same key
// trips the "refusing to clobber" guard. Skipped when shimPID is 0 (no hello);
// PID reuse is negligible since we signal microseconds after it was seen alive.
func (p *Process) Kill() {
	p.killOnce.Do(func() {
		close(p.killCh)
		// Best-effort kill with a short deadline (the shim's disconnect watchdog
		// is the fallback).
		if err := p.link.sendFinal(shimClientMsg{Type: "kill"}, time.Second, true); err != nil {
			slog.Debug("kill: shim kill send failed", "err", err)
		}

		if p.link.shimPID > 0 {
			// A failing Signal (shim already gone) is fine — Discover's stat-check
			// reaps the socket within 30s. No-op on Windows (shim is POSIX-only).
			if err := osutil.SendShimReload(p.link.shimPID); err != nil {
				slog.Debug("kill: SendShimReload failed (likely already exited)",
					"shim_pid", p.link.shimPID, "err", err)
			}
		}
	})
}

// Close gracefully shuts down the CLI and tears down the shim process.
//
// Sends "shutdown" (not "close_stdin") so the shim walks its full exit path:
// closeStdin → waitOrKill(CLI) → listener.Close + os.Remove(socket); once
// p.done fires a fresh StartShim for the same key binds cleanly, whereas
// "close_stdin" leaves the shim listening for up to 30s and trips "refusing to
// clobber" on fast Reset+Recreate. To keep the shim alive, use Detach().
func (p *Process) Close() {
	p.closing.Store(true)
	// Short write deadline: a live shim with a full TCP buffer would otherwise
	// pin the write lock until OS keepalive (minutes), stalling
	// heartbeat/interrupt and Router shutdown past SIGTERM grace.
	if sendErr := p.link.sendFinal(shimClientMsg{Type: "shutdown"}, 2*time.Second, false); sendErr != nil {
		// The shim will not process the shutdown; waiting processCloseTimeout on
		// <-p.done would only double teardown latency. Fall through to Kill().
		p.Kill()
		return
	}
	timer := time.NewTimer(processCloseTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		slog.Warn("process close timeout, force killing", "pid", p.link.cliPID)
		p.Kill()
	}
}

// Detach disconnects from the shim without stopping the CLI (naozhi graceful
// shutdown). A short write deadline keeps Router.Shutdown's wg.Wait() from
// being pinned for minutes by a dead/slow socket during SIGTERM handling.
func (p *Process) Detach() {
	p.detached.Store(true)
	if err := p.link.sendFinal(shimClientMsg{Type: "detach"}, 2*time.Second, true); err != nil {
		slog.Debug("detach: shim detach send failed", "err", err)
	}
}

// State returns the current process state.
func (p *Process) State() ProcessState {
	p.turn.mu.RLock()
	defer p.turn.mu.RUnlock()
	return p.turn.state
}

// SetOnTurnDone sets the callback invoked by readLoop when a result event
// transitions the process from Running to Ready without an active Send().
// Thread-safe. The callback MUST be idempotent — readLoop can fire it several
// times in rapid succession (mid-turn reconnect CAS path immediately followed
// by a Kill; see the onTurnDone field godoc), so do only wake/broadcast work.
func (p *Process) SetOnTurnDone(fn func()) {
	p.turn.mu.Lock()
	p.turn.onTurnDone = fn
	p.turn.mu.Unlock()
}

// SetOnUnownedResult sets the callback that receives a result no live caller
// consumes (see turnState.onUnownedResult). mu-guarded, so safe at any time.
func (p *Process) SetOnUnownedResult(fn func(clievent.SendResult)) {
	p.turn.mu.Lock()
	p.turn.onUnownedResult = fn
	p.turn.mu.Unlock()
}

// SetOnCodeChange sets the callback that receives the PRs the CLI reports
// (see Process.onCodeChange). Lock-free, so safe at any time; nil clears it.
func (p *Process) SetOnCodeChange(fn func(clievent.CodeChange)) {
	if fn == nil {
		p.onCodeChange.Store(nil)
		return
	}
	p.onCodeChange.Store(&fn)
}

// SetOnLiveVersion sets the callback fired by setLiveVersion when a distinct
// CLI binary version is first observed. Not synchronised: Wrapper.Spawn calls
// it once before startReadLoop; never call it while the read loop is running.
func (p *Process) SetOnLiveVersion(fn func(string)) {
	p.onLiveVersion = fn
}

// SessionID returns the session ID in a thread-safe manner.
func (p *Process) SessionID() string {
	p.turn.mu.RLock()
	defer p.turn.mu.RUnlock()
	return p.turn.sessionID
}

// TotalCost is the cost the last result frame reported. Lock-free.
func (p *Process) TotalCost() float64 { return p.meter.TotalCost() }

// ContextUsagePercent is the last reported context-window utilisation
// (0-100); 0 for backends that don't report it (claude stream-json). Lock-free.
func (p *Process) ContextUsagePercent() float64 { return p.meter.ContextUsagePercent() }

// TurnDurationMs is the duration of the most recently completed turn, in ms;
// 0 before any turn completes. Lock-free.
func (p *Process) TurnDurationMs() int64 { return p.meter.TurnDurationMs() }

// Effort is the thinking-effort tier in force (kiro: low…max): the
// backend-reported tier, or the spawn pin until one is reported; "" when
// neither is known (codex). Lock-free.
func (p *Process) Effort() string { return p.meter.Effort() }

// MeteringUsage returns a copy of the backend-reported billing rows; nil for
// backends that report cost only via TotalCost (claude).
func (p *Process) MeteringUsage() []clievent.MeteringEntry { return p.meter.Metering() }

// MeteringGen returns the number of metering writes applied so far. Rows
// returned by MeteringUsage are unchanged while this value is unchanged, which
// lets pollers cache the copy (#2345). Wait-free.
func (p *Process) MeteringGen() uint64 { return p.meter.MeteringGen() }

// applyMetadata stores a Type:"metadata" frame (called from readLoop).
func (p *Process) applyMetadata(m *clievent.EventMetadata) { p.meter.ApplyMetadata(m) }

// ProtocolName returns the protocol name.
func (p *Process) ProtocolName() string {
	return p.protocol.Name()
}

// seedEffort pre-fills the effort tier from the spawn pin: `--effort <tier>` is
// launch-time state on every EffortTier protocol and claude never reports it in
// a metadata frame (RFC dashboard-model-effort-control §4.1). Fill-if-unset
// (CAS from nil) so a backend-reported tier — possibly already replayed on
// reconnect — is never clobbered by the static pin. No-op without
// Caps.EffortTier (codex): BuildArgs dropped the tier, so claiming it would lie.
func (p *Process) seedEffort(tier string) {
	if !p.caps.EffortTier {
		return
	}
	p.meter.SeedEffort(tier)
}

// SeedFromSpawnArgs restores the launch-time state a reattached process cannot
// get from SpawnOptions (SpawnReconnect has none) out of the shim-recorded
// spawn argv: the effort tier, and for claude the settings file its model list
// comes from. Fill-if-unset on both, so state already reported wins.
func (p *Process) SeedFromSpawnArgs(args []string) {
	p.seedEffort(effortFromArgs(args))
	if s, ok := p.protocol.(interface{ seedSettingsFromArgs([]string) }); ok {
		s.seedSettingsFromArgs(args)
	}
}

// effortFromArgs extracts the tier from `--effort <tier>` / `--effort=<tier>`,
// last occurrence winning; "" when absent or dangling (parity with BuildArgs).
func effortFromArgs(args []string) string {
	tier := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--effort":
			if i+1 < len(args) {
				tier = args[i+1]
				i++
			}
		case strings.HasPrefix(args[i], "--effort="):
			tier = strings.TrimPrefix(args[i], "--effort=")
		}
	}
	return tier
}

// setSpawnDiags records this spawn's gate decisions. Called once by
// Wrapper.Spawn before readLoop starts; never re-set afterwards.
func (p *Process) setSpawnDiags(diags []SpawnDiag) {
	if len(diags) == 0 {
		return
	}
	p.spawnDiags.Store(&diags)
}

// SpawnDiags returns the gate decisions of this spawn (nil when every
// configured input took effect). Lock-free; callers must not mutate.
func (p *Process) SpawnDiags() []SpawnDiag {
	if d := p.spawnDiags.Load(); d != nil {
		return *d
	}
	return nil
}

// setModel records the model the CLI runs: the spawn pin (Wrapper.Spawn),
// the one system/init resolves, or the one a set_model ack switched to.
func (p *Process) setModel(model string) { p.meter.SetModel(model) }

// Model returns the CLI model identifier, or "" when neither the operator
// configured one nor the CLI reported one yet. Lock-free.
func (p *Process) Model() string { return p.meter.Model() }

// AvailableModels returns the agent-reported model manifest captured during
// Init (ACP only; nil otherwise). An optional protocol facet like ModelSetter,
// hence the type assertion (docs/rfc/dashboard-model-effort-control.md §4.2).
func (p *Process) AvailableModels() []ModelInfo {
	if am, ok := p.protocol.(interface{ AvailableModels() []ModelInfo }); ok {
		return am.AvailableModels()
	}
	return nil
}

// setLiveVersion records the CLI binary version self-reported in system/init
// and, when it changes, fires onLiveVersion so the owning Wrapper can refresh
// the global dashboard banner. The change-gate keeps the duplicate init
// captures (readLoop + Send()) from firing the callback twice.
func (p *Process) setLiveVersion(v string) {
	if p.meter.SetLiveVersion(v) && p.onLiveVersion != nil {
		p.onLiveVersion(v)
	}
}

// LiveVersion returns the CLI binary version self-reported by the running
// process, or "" if the init frame has not arrived yet. Lock-free.
func (p *Process) LiveVersion() string { return p.meter.LiveVersion() }

// PID returns the CLI process ID (as reported by shim).
func (p *Process) PID() int {
	return p.link.cliPID
}

// TotalTimeout returns the configured total timeout for a single turn.
func (p *Process) TotalTimeout() time.Duration {
	if p.totalTimeout > 0 {
		return p.totalTimeout
	}
	return DefaultTotalTimeout
}

// LastSeq returns the last received shim sequence number (for reconnect).
func (p *Process) LastSeq() int64 { return p.link.lastSeq.Load() }
