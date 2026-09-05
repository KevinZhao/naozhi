package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
)

// ProcessState represents the lifecycle state of a CLI process.
type ProcessState int

const (
	StateSpawning ProcessState = iota
	StateReady
	StateRunning
	StateDead
)

const (
	DefaultNoOutputTimeout = 2 * time.Minute
	DefaultTotalTimeout    = 5 * time.Minute
	// maxScannerBufBytes caps a single NDJSON line read from the shim's stdout.
	// Kept 6 MiB below the shim's own 16 MiB per-line cap (internal/shim/server.go
	// maxServerLineBytes) so a shim-side allocator decision never makes this
	// reader silently truncate; the headroom covers coalesced buffered events and
	// base64-image-laden tool_result frames.
	maxScannerBufBytes = 10 * 1024 * 1024

	// maxStdinLineBytes is the largest single NDJSON line forwarded to the shim
	// (which enforces 16 MB per line); headroom is for the shimClientMsg envelope.
	// Exceeding it fails fast with ErrMessageTooLarge so the dashboard can surface it.
	maxStdinLineBytes = 12 * 1024 * 1024

	// lineBufShrinkThreshold caps the capacity readLoop's lineBuf may retain
	// across iterations before shrinking back to 4 KiB. tool_result payloads and
	// assistant text chunks commonly hit 50-200 KiB, so 256 KiB retains that
	// capacity (one realloc per session, not per event) while still bounding
	// runaway growth from a buggy shim: ~+10 MiB idle RSS at 50 sessions.
	lineBufShrinkThreshold = 256 * 1024
)

// ErrMessageTooLarge is returned when a user message (after JSON encoding) would
// exceed the shim's per-line limit; callers should shrink the payload first.
var ErrMessageTooLarge = errors.New("message too large for stream-json line")

// Sentinel errors for watchdog timeouts.
var (
	ErrNoOutputTimeout = errors.New("no output timeout")
	ErrTotalTimeout    = errors.New("total timeout")
)

// ErrProcessExited is returned by Send when the CLI subprocess exits before
// producing a result; callers react by spawning a new process next turn.
var ErrProcessExited = errors.New("process exited during send")

// ErrProcessBusy is returned by Send when the legacy (non-passthrough) state
// machine is already StateRunning; dispatch maps it to "正在处理中".
var ErrProcessBusy = errors.New("process busy")

// Passthrough-mode sentinels (separate block for targeted errors.Is switches).
var (
	// ErrSessionReset fires when a user slash-command (/new, /clear) or a forced
	// wrapper reset cancels all pending sends; not surfaced to IM (user-triggered).
	ErrSessionReset = errors.New("session reset")

	// ErrReconnectedUnknown fires when naozhi re-attaches to a shim+CLI that
	// survived a restart with messages in flight: naozhi cannot tell which were
	// consumed, so every pending slot gets it (dispatcher: 状态未知，请查看历史或重发).
	ErrReconnectedUnknown = errors.New("reconnected: processing state unknown")

	// ErrTooManyPending fires when Send is called with maxPendingSlots already
	// pending; the message is rejected up front (dispatcher: sendAckBusy).
	ErrTooManyPending = errors.New("too many pending messages")

	// ErrOrphanedSlot is a defensive fallback: Send's totalTimeout+30s tripwire in
	// case watchdog and readLoop both miss delivering a result. Fires only on bugs.
	ErrOrphanedSlot = errors.New("slot orphaned: no result or error received")
)

// maxPendingSlots caps the per-Process passthrough pending queue; a goroutine-
// leak / memory backstop, not a business limit. Tunable via SetMaxPendingSlots.
const maxPendingSlots = 16

// ErrAbortedByUrgent fires when a priority:"now" message makes the CLI drop the
// in-flight turn: older pending slots not yet replayed get this error — their
// text never reached the model, so the user must decide whether to resend.
var ErrAbortedByUrgent = errors.New("aborted by priority:now preemption")

// ErrNoActiveTurn is returned by InterruptViaControl when no turn is running;
// nothing was interrupted, so logs must not claim "aborted active turn".
var ErrNoActiveTurn = errors.New("no active turn to interrupt")

// processCloseTimeout bounds Close() while the shim tears down its listener +
// socket (closeStdin + waitOrKill(5s) + listener.Close + os.Remove, so 8s is
// headroom); on expiry Close falls through to Kill. Var so tests can shorten it.
var processCloseTimeout = 8 * time.Second

func (s ProcessState) String() string {
	switch s {
	case StateSpawning:
		return "running" // spawning is transient; visible as running
	case StateReady:
		return "ready"
	case StateRunning:
		return "running"
	case StateDead:
		// Not "ready": the dashboard must not show crashed processes as idle.
		return "dead"
	default:
		return "unknown"
	}
}

// Process manages a CLI subprocess via a shim connection.
type Process struct {
	shimConn net.Conn
	// shimCloseOnce serialises shimConn.Close across Kill / Detach / Close /
	// readLoop EOF so a racing second Close never logs a misleading error.
	shimCloseOnce sync.Once
	shimR         *bufio.Reader
	shimW         *bufio.Writer
	shimWMu       sync.Mutex
	stdinWriter   *shimWriter // cached shimStdinWriter instance
	protocol      Protocol
	caps          Caps // cached protocol capabilities (immutable after construction)
	cliPID        int  // CLI PID reported by shim hello
	shimPID       int  // shim PID reported by shim hello; used by Kill() for SIGUSR2 fallback

	// sessionID and state are protected by mu. Readers MUST use SessionID() /
	// State() rather than the fields directly to avoid racing readLoop's
	// transition writes (#623).
	sessionID string
	state     ProcessState
	// mu protects state / sessionID / onTurnDone. Accessors use RLock so Snapshot
	// polls run in parallel; write paths (readLoop transitions, Send state→Running,
	// Interrupt snapshot-and-flag) use Lock so "read state + set interrupted" stays
	// atomic. totalCost is a separate atomic so readers never nest p.mu under r.mu.
	mu sync.RWMutex

	eventCh  chan Event
	done     chan struct{}
	killCh   chan struct{} // closed by Kill() to unblock readLoop
	killOnce sync.Once

	// lifecycleCtx is canceled when the process exits (readLoop returns or Kill());
	// binds subagent-Resolve goroutines so SIGTERM doesn't leave them spinning
	// (#644). Lazily initialised so &Process{} test fixtures still work.
	lifecycleCtxOnce   sync.Once
	lifecycleCtxValue  context.Context
	lifecycleCtxCancel context.CancelFunc

	noOutputTimeout time.Duration
	totalTimeout    time.Duration
	interrupted     atomic.Bool // set by Interrupt(), cleared by next Send()
	interruptedRun  atomic.Bool // true when Interrupt() was called while State==Running

	// interruptSeq generates request_id suffixes for control_request interrupts;
	// the CLI only echoes it back, so per-connection uniqueness suffices.
	interruptSeq atomic.Int64

	// controlAckMu guards controlAcks: pending SetModel ack waiters keyed by
	// request_id, registered before the wire write and removed by the waiter, so
	// a late ack is dropped harmlessly (docs/rfc/dashboard-model-effort-control.md §4.4).
	controlAckMu sync.Mutex
	controlAcks  map[string]chan error

	eventLog  *EventLog
	totalCost atomic.Uint64 // math.Float64bits(lastResultCostUSD); atomic so Snapshot is lock-free.

	// Normalized metadata from backend metadata events (ACP _kiro.dev/metadata).
	// Lock-free reads from Snapshot. See docs/rfc/multi-backend.md §8.8.
	contextUsagePercentBits atomic.Uint64 // math.Float64bits of last ContextUsagePercent
	turnDurationMs          atomic.Int64  // last TurnDurationMs (ms)
	// effort is the backend-reported thinking-effort tier (kiro: low…max); empty
	// when never reported. A string, not an enum, so an unrecognised future tier
	// still reaches the dashboard (docs/rfc/kiro-effort-visibility.md §2).
	effort atomic.Pointer[string]
	// spawnDiags is the gate decisions of this spawn (SpawnDiagsFor), set once
	// by Wrapper.Spawn before readLoop; runtime observation only, never
	// persisted (same lifecycle as effort). nil = none.
	spawnDiags atomic.Pointer[[]SpawnDiag]
	// shadowMu guards shadow, the token usage of assistant frames since the
	// last result frame (see ShadowUsage).
	shadowMu sync.Mutex
	shadow   ShadowUsage
	// meteringMu guards meteringUsage (read-mostly: 1 Hz × N-tab polls, ≤1
	// write/turn). meteringLen mirrors len(meteringUsage) under meteringMu so
	// MeteringUsage() can skip the RLock when empty (claude-class backends).
	meteringMu    sync.RWMutex
	meteringUsage []MeteringEntry
	// meteringIdx maps Unit → index into meteringUsage (O(1) merge); lazily built
	// on first applyMetadata so zero-metering sessions stay allocation-free.
	meteringIdx map[string]int
	meteringLen atomic.Int32
	// meteringGen counts metering writes (#2345), bumped under meteringMu, so a
	// reader sampling MeteringGen BEFORE MeteringUsage never pairs a gen with
	// older rows; ManagedSession.Snapshot keys its copy cache on it.
	meteringGen atomic.Uint64
	// model is the spawn-time CLI model identifier, set once by Wrapper.Spawn
	// before readLoop starts. Empty means cli.backends[].model is unconfigured
	// (dashboard renders "(模型未配置)"). atomic.Pointer keeps Snapshot lock-free.
	model atomic.Pointer[string]
	// liveVersion is the CLI binary version self-reported in system/init —
	// authoritative for THIS process even after a host claude upgrade made the
	// spawn-time Wrapper.CLIVersion stale. Empty until the init frame arrives.
	liveVersion atomic.Pointer[string]
	// onLiveVersion is invoked by setLiveVersion on each distinct binary version
	// so the owning Wrapper can refresh the global dashboard banner. Assigned once
	// before startReadLoop (no lock); nil for &Process{} test fixtures.
	onLiveVersion func(string)
	lastSeq       atomic.Int64  // last received shim seq, for reconnect
	pongRecv      chan struct{} // signaled by readLoop on pong receipt

	// readEventBuf is a reusable backing array for ReadEventInto (#1676), owned
	// exclusively by handleShimStdout on the readLoop goroutine and consumed within
	// the same frame; cap 2 covers ACP's two-event turn-end split.
	readEventBuf [2]Event

	// onTurnDone is called by readLoop when a result event transitions the
	// process from Running to Ready without an active Send() (e.g. after a shim
	// reconnect set StateRunning but the CLI finished before Send was called), so
	// the session layer can broadcast state changes. Protected by mu — assign via
	// SetOnTurnDone.
	//
	// Implementations MUST be idempotent: readLoop may fire the callback more
	// than once per turn from arms that run back-to-back — the result +
	// reconnectedMidTurn CAS path followed by <-killCh, plus cli_exited, the
	// fall-out StateDead path and the panic defer.
	onTurnDone func()

	// reconnectedMidTurn is set by SpawnReconnect when the CLI was mid-turn at
	// reconnect. It lets readLoop transition a stray result (no active Send)
	// Running→Ready; otherwise that transition is owned by Send()'s defer and
	// readLoop must not race it. Atomic: cleared without taking p.mu.
	reconnectedMidTurn atomic.Bool

	// deathReason records why the process died; written once (first-writer-wins
	// CAS) by the path that transitions State→Dead. nil until stored.
	deathReason atomic.Pointer[string]

	// log is a pre-bound logger carrying the "session" attribute; set once by
	// SetSlogKey before the reader goroutines start, so reads are lock-free.
	log atomic.Pointer[slog.Logger]

	// Passthrough slot machinery: with SendPassthrough, readLoop routes results
	// through fanoutTurnResult instead of eventCh (legacy Send leaves pendingSlots
	// nil). Lock ordering (docs/rfc/passthrough-mode.md §5.2.6):
	//   shimWMu → slotsMu  (Send path; append slot + write stdin atomically)
	//   slotsMu alone      (readLoop, cancel, reconnect)
	slotsMu          sync.Mutex
	pendingSlots     []*sendSlot // FIFO by stdin write order
	currentTurnSlots []*sendSlot // slots claimed by the in-flight turn
	turnStartedAt    time.Time   // set on system/init; zeroed on result
	inTurn           bool
	slotIDGen        atomic.Uint64

	// linker maps parallel-agent task_ids to transcript jsonl paths for the
	// dashboard's agent_events endpoint. Set by InitLinker; nil in test fakes.
	linker *SubagentLinker
	// cwd is the Spawn working directory, kept so the linker projectDir can be
	// re-derived on shim reconnect.
	cwd string
	// cachedProjectDir is resolveProjectDir(cwd), computed once (cwd is immutable)
	// to avoid a rune scan + os.UserHomeDir syscall per system/init event.
	cachedProjectDir string
}

// sendSlot tracks one in-flight passthrough Send call: appended to pendingSlots
// atomically with the stdin write, matched to the CLI's replay event by uuid
// (single sender) or text (merged sender), then handed the turn's result.
// canceled is a tombstone (docs/rfc/passthrough-mode.md §5.2.2): a slot whose
// caller left via ctx.Done stays in pendingSlots to keep FIFO order, but fan-out
// drops its result. Atomic so fanout reads it lock-free after releasing slotsMu.
type sendSlot struct {
	id       uint64
	uuid     string
	text     string
	priority string // "" | "now" | "next" | "later"
	onEvent  EventCallback
	resultCh chan *SendResult
	errCh    chan error

	// Only mutated under Process.slotsMu (atomic.Bool to allow lock-free
	// reads from fanoutTurnResult outside slotsMu).
	canceled  atomic.Bool
	replayed  bool
	enqueueAt time.Time
	writtenAt time.Time
}

// isCanceled reads canceled atomically; fanout uses it lock-free outside
// slotsMu, while writes go through slotsMu to stay FIFO-ordered.
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

// Death reason labels. Kept as exported constants so session/router callers
// can match without relying on stringly-typed literals that drift.
const (
	DeathReasonCLIExited           = "cli_exited"
	DeathReasonShimEOF             = "shim_eof"
	DeathReasonShimReadErr         = "shim_read_error"
	DeathReasonShimOversizeThenEOF = "shim_oversize_then_eof"
	DeathReasonShimOversizeThenErr = "shim_oversize_then_read_error"
	DeathReasonReadLoopPanic       = "readloop_panic"
	DeathReasonKilled              = "killed"
	DeathReasonNoOutputTimeout     = "no_output_timeout"
	DeathReasonTotalTimeout        = "total_timeout"
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
		p.lifecycleCtxCancel = cancel
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
		shimConn: conn,
		shimR:    reader,
		shimW:    writer,
		protocol: proto,
		caps:     ProtocolCaps(proto),
		cliPID:   cliPID,
		shimPID:  shimPID,
		state:    StateSpawning,
		// 1024 so a TeamCreate fan-out (8 subagents × ~5 events/s) cannot fill the
		// buffer before Send() drains it; drops force the findResultSince fallback (#1355).
		eventCh:         make(chan Event, 1024),
		done:            make(chan struct{}),
		killCh:          make(chan struct{}),
		noOutputTimeout: noOutputTimeout,
		totalTimeout:    totalTimeout,
		eventLog:        NewEventLog(0),
		// maxMisses+1 so a heartbeatLoop scheduler stall cannot drop pongs
		// (readLoop's pong arm is non-blocking) and miscount a healthy shim.
		pongRecv: make(chan struct{}, 4),
	}
	p.stdinWriter = &shimWriter{p: p}
	return p
}

// shimStdinWriter returns the io.Writer feeding CLI stdin via the shim. Same
// instance each call (preserves buffered partial lines); initialised eagerly in
// newShimProcess so readLoop and Send cannot race a lazy init on reconnect.
func (p *Process) shimStdinWriter() io.Writer {
	return p.stdinWriter
}

// startReadLoop begins the shim message reader goroutine and heartbeat.
// Initial state is StateReady EXCEPT on the reconnect-mid-turn path (#1778),
// where SpawnReconnect pre-armed reconnectedMidTurn + StateRunning: forcing
// Ready here would let the stray-result CAS consume the flag with wasRunning
// false, stranding the session in Running once SpawnReconnect re-sets it.
func (p *Process) startReadLoop() {
	p.mu.Lock()
	if !(p.reconnectedMidTurn.Load() && p.state == StateRunning) {
		p.state = StateReady
	}
	p.mu.Unlock()
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
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state == StateRunning
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
		// is the fallback). Hold shimWMu across deadline + send + Close: bufio.Writer
		// is not safe against a concurrent Close()+Flush from heartbeat/interrupt.
		p.shimWMu.Lock()
		// Skip the write if the deadline can't be set: without one shimSendLocked
		// can block until TCP keepalive expires (minutes), starving shimWMu.
		if err := p.shimConn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			slog.Debug("kill: SetWriteDeadline failed, skipping shim kill send", "err", err)
		} else {
			if err := p.shimSendLocked(shimClientMsg{Type: "kill"}); err != nil {
				slog.Debug("kill: shimSend failed", "err", err)
			}
		}
		p.closeShimConn()
		p.shimWMu.Unlock()

		if p.shimPID > 0 {
			// A failing Signal (shim already gone) is fine — Discover's stat-check
			// reaps the socket within 30s. No-op on Windows (shim is POSIX-only).
			if err := osutil.SendShimReload(p.shimPID); err != nil {
				slog.Debug("kill: SendShimReload failed (likely already exited)",
					"shim_pid", p.shimPID, "err", err)
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
	// Short write deadline under shimWMu: a live shim with a full TCP buffer
	// would otherwise pin shimWMu until OS keepalive (minutes), stalling
	// heartbeat/interrupt and Router shutdown past SIGTERM grace.
	p.shimWMu.Lock()
	if err := p.shimConn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		p.shimWMu.Unlock()
		p.Kill()
		return
	}
	sendErr := p.shimSendLocked(shimClientMsg{Type: "shutdown"})
	// Clear the deadline *before* releasing shimWMu so a concurrent heartbeat
	// ping cannot inherit it, see a stale i/o timeout and Kill() — bypassing the
	// graceful teardown. Failure is harmless (zero-time means "no deadline").
	_ = p.shimConn.SetWriteDeadline(time.Time{})
	p.shimWMu.Unlock()
	if sendErr != nil {
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
		slog.Warn("process close timeout, force killing", "pid", p.cliPID)
		p.Kill()
	}
}

// Detach disconnects from the shim without stopping the CLI (naozhi graceful
// shutdown). A short write deadline keeps Router.Shutdown's wg.Wait() from
// being pinned for minutes by a dead/slow socket during SIGTERM handling.
func (p *Process) Detach() {
	p.shimWMu.Lock()
	// Skip the send if the deadline can't be set: without one shimSendLocked
	// can block until TCP keepalive expires. Same pattern as Kill().
	if err := p.shimConn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		slog.Debug("detach: SetWriteDeadline failed, skipping shim detach send", "err", err)
	} else {
		if err := p.shimSendLocked(shimClientMsg{Type: "detach"}); err != nil {
			slog.Debug("detach: shimSend failed", "err", err)
		}
	}
	// Zero the deadline before closeShimConn so a parallel teardown path (Kill,
	// heartbeat) cannot inherit it and hit a spurious i/o timeout.
	_ = p.shimConn.SetWriteDeadline(time.Time{})
	p.closeShimConn()
	p.shimWMu.Unlock()
}

// closeShimConn closes p.shimConn at most once across all teardown paths so a
// second concurrent Close never logs a misleading error.
func (p *Process) closeShimConn() {
	p.shimCloseOnce.Do(func() {
		if err := p.shimConn.Close(); err != nil {
			slog.Debug("shimConn close failed", "err", err)
		}
	})
}

// State returns the current process state.
func (p *Process) State() ProcessState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state
}

// SetOnTurnDone sets the callback invoked by readLoop when a result event
// transitions the process from Running to Ready without an active Send().
// Thread-safe. The callback MUST be idempotent — readLoop can fire it several
// times in rapid succession (mid-turn reconnect CAS path immediately followed
// by a Kill; see the onTurnDone field godoc), so do only wake/broadcast work.
func (p *Process) SetOnTurnDone(fn func()) {
	p.mu.Lock()
	p.onTurnDone = fn
	p.mu.Unlock()
}

// SetOnLiveVersion sets the callback fired by setLiveVersion when a distinct
// CLI binary version is first observed. Not synchronised: Wrapper.Spawn calls
// it once before startReadLoop; never call it while the read loop is running.
func (p *Process) SetOnLiveVersion(fn func(string)) {
	p.onLiveVersion = fn
}

// SessionID returns the session ID in a thread-safe manner.
func (p *Process) SessionID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.sessionID
}

// TotalCost returns the cumulative cost (lock-free via atomic.Uint64).
func (p *Process) TotalCost() float64 {
	return math.Float64frombits(p.totalCost.Load())
}

// ContextUsagePercent returns the last reported context-window utilisation
// (0-100); 0 for backends that don't report it (claude stream-json). Lock-free.
func (p *Process) ContextUsagePercent() float64 {
	return math.Float64frombits(p.contextUsagePercentBits.Load())
}

// TurnDurationMs returns the duration of the most recently completed turn,
// in ms. 0 when no turn has completed yet. Lock-free.
func (p *Process) TurnDurationMs() int64 {
	return p.turnDurationMs.Load()
}

// Effort returns the backend-reported thinking-effort tier for the most recent
// turn (kiro: low…max); "" when the backend never reports one (claude, codex)
// or before the first metadata frame. Lock-free.
func (p *Process) Effort() string {
	if e := p.effort.Load(); e != nil {
		return *e
	}
	return ""
}

// MeteringUsage returns a defensive copy of the most recent backend-reported
// billing rows; nil for backends that report cost only via TotalCost (claude).
// An atomic length probe lets that dominant polled case skip the RLock.
func (p *Process) MeteringUsage() []MeteringEntry {
	if p.meteringLen.Load() == 0 {
		return nil
	}
	p.meteringMu.RLock()
	defer p.meteringMu.RUnlock()
	if len(p.meteringUsage) == 0 {
		return nil
	}
	out := make([]MeteringEntry, len(p.meteringUsage))
	copy(out, p.meteringUsage)
	return out
}

// MeteringGen returns the number of metering writes applied so far. Rows
// returned by MeteringUsage are unchanged while this value is unchanged, which
// lets pollers cache the copy (#2345). Wait-free.
func (p *Process) MeteringGen() uint64 {
	return p.meteringGen.Load()
}

// applyMetadata stores normalized metadata from a Type:"metadata" event (called
// from readLoop): scalars atomically, MeteringUsage merged under meteringMu.
// Every field is guarded on being non-zero so a frame that omits a field never
// regresses an earlier value (pinned by TestProcess_ApplyMetadata_AndAccessors).
func (p *Process) applyMetadata(m *EventMetadata) {
	if m == nil {
		return
	}
	if m.ContextUsagePercent > 0 {
		p.contextUsagePercentBits.Store(math.Float64bits(m.ContextUsagePercent))
	}
	if m.TurnDurationMs > 0 {
		p.turnDurationMs.Store(m.TurnDurationMs)
	}
	// Overwrite semantics (unlike the per-unit accumulation below): effort is a
	// current-state tier, so xhigh→max mid-session must replace. The non-empty
	// guard stops a frame that omits effort from blanking a known tier.
	if m.Effort != "" {
		// Change-gate: re-storing an identical value every frame would allocate
		// and dirty a cache line the 1 Hz × N-tab Snapshot poll reads.
		if prev := p.effort.Load(); prev == nil || *prev != m.Effort {
			e := m.Effort
			p.effort.Store(&e)
		}
	}
	if len(m.MeteringUsage) > 0 {
		p.meteringMu.Lock()
		// kiro reports per-turn increments and no running total, so session totals
		// are summed by Unit; maxMeteringUnits bounds growth from a buggy upstream.
		const maxMeteringUnits = 16
		// Lazy init so zero-metering sessions never allocate the map.
		if p.meteringIdx == nil {
			p.meteringIdx = make(map[string]int, maxMeteringUnits)
			for i := range p.meteringUsage {
				p.meteringIdx[p.meteringUsage[i].Unit] = i
			}
		}
		for _, in := range m.MeteringUsage {
			if i, ok := p.meteringIdx[in.Unit]; ok {
				p.meteringUsage[i].Value += in.Value
				if in.UnitPlural != "" {
					p.meteringUsage[i].UnitPlural = in.UnitPlural
				}
				continue
			}
			if len(p.meteringUsage) >= maxMeteringUnits {
				continue
			}
			p.meteringIdx[in.Unit] = len(p.meteringUsage)
			p.meteringUsage = append(p.meteringUsage, in)
		}
		// Publish the post-merge length under meteringMu so MeteringUsage's
		// lock-free fast path sees the same length the slice has after Unlock.
		p.meteringLen.Store(int32(len(p.meteringUsage)))
		p.meteringGen.Add(1)
		p.meteringMu.Unlock()
	}
}

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
	if tier == "" || !p.caps.EffortTier {
		return
	}
	e := tier
	p.effort.CompareAndSwap(nil, &e)
}

// SeedEffortFromArgs is the reconnect-path seedEffort: SpawnReconnect has no
// SpawnOptions, so the tier is recovered from the shim-recorded spawn argv.
func (p *Process) SeedEffortFromArgs(args []string) {
	p.seedEffort(effortFromArgs(args))
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

// setModel records the spawn-time model. Called once by Wrapper.Spawn
// before readLoop starts; never re-set afterwards.
func (p *Process) setModel(model string) {
	if model == "" {
		p.model.Store(nil)
		return
	}
	m := model
	p.model.Store(&m)
}

// Model returns the spawn-time CLI model identifier, or "" if the operator did
// not configure one. Lock-free.
func (p *Process) Model() string {
	if m := p.model.Load(); m != nil {
		return *m
	}
	return ""
}

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
// (lock-free) and, when it changes, fires onLiveVersion so the owning Wrapper
// can refresh the global dashboard banner. The change-gate keeps the duplicate
// init captures (readLoop + Send()) from firing the callback twice.
func (p *Process) setLiveVersion(v string) {
	if v == "" {
		return
	}
	if prev := p.liveVersion.Load(); prev != nil && *prev == v {
		return
	}
	s := v
	p.liveVersion.Store(&s)
	if p.onLiveVersion != nil {
		p.onLiveVersion(v)
	}
}

// LiveVersion returns the CLI binary version self-reported by the running
// process, or "" if the init frame has not arrived yet. Lock-free.
func (p *Process) LiveVersion() string {
	if v := p.liveVersion.Load(); v != nil {
		return *v
	}
	return ""
}

// PID returns the CLI process ID (as reported by shim).
func (p *Process) PID() int {
	return p.cliPID
}

// TotalTimeout returns the configured total timeout for a single turn.
func (p *Process) TotalTimeout() time.Duration {
	if p.totalTimeout > 0 {
		return p.totalTimeout
	}
	return DefaultTotalTimeout
}

// LastSeq returns the last received shim sequence number (for reconnect).
func (p *Process) LastSeq() int64 { return p.lastSeq.Load() }
