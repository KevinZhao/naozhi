package cli

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
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
	maxScannerBufBytes     = 10 * 1024 * 1024

	// maxStdinLineBytes is the largest single NDJSON line we will forward to
	// the shim. The shim enforces 16 MB per line; we leave headroom for the
	// shim-protocol JSON envelope added in shimClientMsg. Exceeding this
	// value used to produce a silent "connection reset by peer" from the
	// shim — now we fail fast with a clear error so the dashboard can surface it.
	maxStdinLineBytes = 12 * 1024 * 1024

	// lineBufShrinkThreshold caps how much capacity readLoop's lineBuf is
	// allowed to retain across iterations before being shrunk back to the
	// 4 KiB starting size.
	//
	// Sizing rationale: tool_result payloads and assistant text chunks in
	// the wild commonly hit 50-200 KiB; the prior 64 KiB threshold fired
	// on nearly every one of them, forcing the next large event in the
	// same session to re-grow through 4→8→16→32→64→128 KiB doublings
	// (~5 reallocs + copies). 256 KiB covers the realistic upper edge of
	// legitimate events while still rejecting runaway growth from a buggy
	// or adversarial shim. The tradeoff at 50 concurrent sessions is
	// 50 × +192 KiB = +~10 MiB idle RSS in exchange for eliminating the
	// reallocation churn from the common large-event path.
	lineBufShrinkThreshold = 256 * 1024
)

// ErrMessageTooLarge is returned when a user message (after JSON encoding)
// would exceed the shim's per-line limit. Callers should shrink the payload
// (e.g., downscale images) before retrying.
var ErrMessageTooLarge = errors.New("message too large for stream-json line")

// Sentinel errors for watchdog timeouts.
var (
	ErrNoOutputTimeout = errors.New("no output timeout")
	ErrTotalTimeout    = errors.New("total timeout")
)

// ErrProcessExited is returned by Send when the CLI subprocess exits before
// producing a result. Distinguishable from watchdog timeouts so callers
// (managed.go, dispatch) can react with "spawn a new process next turn"
// rather than counting it as a no-output stall.
var ErrProcessExited = errors.New("process exited during send")

// ErrProcessBusy is returned by Send when the (legacy, non-passthrough)
// state machine is already StateRunning. Typed so dispatch.mapSendError
// can reply "正在处理中" instead of falling into the generic /new reset
// hint. Passthrough-mode processes do not emit this.
var ErrProcessBusy = errors.New("process busy")

// Passthrough-mode sentinels. Kept in a separate block so callers can do
// targeted errors.Is switches without depending on the wider process error
// vocabulary.
var (
	// ErrSessionReset fires when a user slash-command (/new, /clear) or a
	// forced wrapper reset cancels all pending sends. Dispatcher should not
	// surface this to IM because the user triggered it knowingly.
	ErrSessionReset = errors.New("session reset")

	// ErrReconnectedUnknown fires when naozhi re-attaches to a shim+CLI that
	// survived a naozhi restart and had pending messages in flight. naozhi
	// cannot tell which messages were consumed, so every pending slot gets
	// this error. Dispatcher should surface "状态未知，请查看历史或重发"
	// so users can decide whether to resend.
	ErrReconnectedUnknown = errors.New("reconnected: processing state unknown")

	// ErrTooManyPending fires when Send is called while pendingSlots already
	// holds maxPendingSlots entries. The message is rejected up front — CLI
	// never sees it. Dispatcher maps this to sendAckBusy.
	ErrTooManyPending = errors.New("too many pending messages")

	// ErrOrphanedSlot is a pure defensive fallback: Send's select has a timer
	// tripwire at totalTimeout + 30s in case watchdog and readLoop both miss
	// delivering a result. Should only fire on genuine bugs.
	ErrOrphanedSlot = errors.New("slot orphaned: no result or error received")
)

// maxPendingSlots caps the per-Process passthrough pending queue depth. 16
// covers realistic IM bursts comfortably (CC TUI's equivalent is unbounded,
// but this is a goroutine-leak / memory backstop rather than a business
// limit). Can be tuned via Process.SetMaxPendingSlots.
const maxPendingSlots = 16

// ErrAbortedByUrgent fires when a priority:"now" stdin message causes the
// CLI to drop the in-flight turn. Any older pending slot that was enqueued
// before the urgent message and had not yet been replayed gets this error —
// its text never reached the model. Dispatcher should tell the user the
// message was superseded and let them decide whether to resend.
var ErrAbortedByUrgent = errors.New("aborted by priority:now preemption")

// ErrNoActiveTurn is returned by InterruptViaControl when the process is not
// currently running a turn (StateSpawning, StateReady, or StateDead). The
// caller didn't do anything wrong, but nothing was interrupted; logs should
// not claim "aborted active turn" in this case.
var ErrNoActiveTurn = errors.New("no active turn to interrupt")

// processCloseTimeout bounds Close() while it waits for the shim to tear
// down its listener + socket file. The shim-side path is closeStdin +
// waitOrKill(5s) + listener.Close + os.Remove, so 8s gives comfortable
// headroom; exceeding it falls through to Kill (which uses SIGUSR2 to
// force the shim's immediate-shutdown path, see Kill()). Var (not const)
// so tests can shorten it.
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
		// Was "ready" historically (session may be resumable), but that masqueraded
		// crashed processes as idle in the dashboard — operators could not tell a
		// passive CLI exit / shim EOF / readLoop panic from a genuine idle state.
		// The dashboard falls back to the "new-card" dot for any unrecognised state
		// and combines it with `dead_reason` to render a dead-card badge.
		return "dead"
	default:
		return "unknown"
	}
}

// Process manages a CLI subprocess via a shim connection.
type Process struct {
	shimConn net.Conn
	// shimCloseOnce serialises shimConn.Close across Kill / Detach / Close /
	// readLoop EOF paths. Without it, concurrent close races (e.g. Kill racing
	// readLoop's EOF cleanup) trigger a "use of closed network connection"
	// error from the second Close that callers must currently swallow with a
	// blank `_ =`. The Once eliminates the misleading-debug-log surface and
	// matches the closeOnce idiom already used by EventLog subscribers.
	// R219-GO-3.
	shimCloseOnce sync.Once
	shimR         *bufio.Reader
	shimW         *bufio.Writer
	shimWMu       sync.Mutex
	stdinWriter   *shimWriter // cached shimStdinWriter instance
	protocol      Protocol
	caps          Caps // cached protocol capabilities (immutable after construction)
	cliPID        int  // CLI PID reported by shim hello
	shimPID       int  // shim PID reported by shim hello; used by Kill() for SIGUSR2 fallback

	SessionID string
	State     ProcessState
	// mu protects State / SessionID / onTurnDone. Read-only accessors
	// (GetState / IsRunning / GetSessionID) use RLock so concurrent
	// ListSessions snapshots across N sessions and M tabs proceed in
	// parallel instead of serialising through a single Mutex. Write paths
	// (readLoop state transitions, Send State→Running, Interrupt
	// snapshot-and-flag) continue to use Lock to preserve the existing
	// "read State and set interrupted together under one lock" contract.
	// R70-PERF-L3.
	//
	// totalCost is stored separately as an atomic.Uint64 (math.Float64bits)
	// so Snapshot() paths that want lock-free reads (mirroring the
	// ManagedSession.totalCost pattern, R183-CONCUR-M2) never have to
	// nest p.mu.RLock under ownership of r.mu or sendMu.
	mu sync.RWMutex

	eventCh  chan Event
	done     chan struct{}
	killCh   chan struct{} // closed by Kill() to unblock readLoop
	killOnce sync.Once

	noOutputTimeout time.Duration
	totalTimeout    time.Duration
	interrupted     atomic.Bool // set by Interrupt(), cleared by next Send()
	interruptedRun  atomic.Bool // true when Interrupt() was called while State==Running

	// interruptSeq generates monotonic request_id suffixes for control_request
	// interrupt messages. Per-process so parallel-running tests don't share the
	// counter and dashboard traces stay readable. The CLI only uses request_id
	// to echo back in the matching control_response; uniqueness inside one
	// process connection is sufficient.
	interruptSeq atomic.Int64

	eventLog  *EventLog
	totalCost atomic.Uint64 // math.Float64bits(lastResultCostUSD); atomic so Snapshot is lock-free.

	// Normalized metadata, populated for ACP-class backends from
	// _kiro.dev/metadata events and (eventually) for stream-json from
	// result events / wall clock. Lock-free reads from Snapshot mirror
	// the totalCost pattern. See docs/rfc/multi-backend.md §8.8.
	contextUsagePercentBits atomic.Uint64 // math.Float64bits of last ContextUsagePercent
	turnDurationMs          atomic.Int64  // last TurnDurationMs (ms)
	// meteringMu guards meteringUsage. Replaced whole on each metadata
	// event so reads copy and the typical (no-metering) case stays cheap.
	meteringMu    sync.Mutex
	meteringUsage []MeteringEntry
	// model is the spawn-time CLI model identifier ("claude-opus-4.7",
	// "claude-sonnet-4.6", ""), set once by Wrapper.Spawn before
	// readLoop starts. Empty string means "operator did not configure
	// cli.backends[].model"; the dashboard renders that as
	// "(模型未配置)". Atomic.Pointer[string] keeps Snapshot lock-free,
	// matching how Wrapper.CLIVersion is exposed via WorkflowState. UI
	// Round 5 R5-3.
	model    atomic.Pointer[string]
	lastSeq  atomic.Int64  // last received shim seq, for reconnect
	pongRecv chan struct{} // signaled by readLoop on pong receipt

	// onTurnDone is called by readLoop when a result event transitions the
	// process from Running to Ready without an active Send(). This allows
	// the session layer to broadcast state changes (e.g., after shim reconnect
	// where isMidTurn set StateRunning but the CLI finished before Send was called).
	// Protected by mu — use SetOnTurnDone to assign.
	//
	// Idempotency contract (R183-CONCUR-M1): implementations MUST be idempotent
	// and safe to call multiple times in quick succession. readLoop fires the
	// callback from several arms that may execute back-to-back inside a single
	// iteration — notably the `result + reconnectedMidTurn` CAS path followed by
	// `<-killCh` when Kill() races the mid-turn reconnect finishing. It is also
	// fired by cli_exited, the fall-out StateDead path, and the panic-recover
	// defer. Current session-layer callbacks (`r.notifyChange()` and Send()'s
	// NotifyIdle → Broadcast) are already idempotent, but the contract is
	// documented here so future callbacks do not silently regress.
	onTurnDone func()

	// reconnectedMidTurn is set by SpawnReconnect when the last replayed event
	// indicated the CLI was mid-turn at reconnect time. readLoop consults this
	// flag to decide whether a stray `result` event (no active Send) should
	// transition State Running→Ready on its own. Outside the reconnect path,
	// the transition is owned by Send()'s defer and the readLoop must not race
	// it — see the guard in readLoop. Loaded/stored atomically since it is
	// cleared on the first such transition without taking p.mu.
	reconnectedMidTurn atomic.Bool

	// deathReason records why the process exited (passive death) or was killed.
	// Written exactly once by the code path that transitions State→Dead.
	// Read by session.ManagedSession.Send on ErrProcessExited (or via
	// LoadDeathReason from router/dashboard). atomic.Pointer[string] provides
	// type-safe lock-free reads; Load returns nil when never stored (distinct
	// from a pointer to ""), enabling the first-writer-wins CAS below.
	deathReason atomic.Pointer[string]

	// log is a pre-bound logger that readLoop/heartbeatLoop use so shim
	// disconnect and readloop panic entries carry a "session" attribute
	// without allocating a new slog handler chain on every log call.
	// Initialised to slog.Default() in newShimProcess (so tests constructing
	// Process directly keep their historical unattributed output) and
	// upgraded once by SetSlogKey during Wrapper.Spawn / SpawnReconnect.
	// Read lock-free via atomic.Pointer — writers are the one-shot
	// constructor path (no concurrency with readLoop). R70-ARCH-M3.
	log atomic.Pointer[slog.Logger]

	// Passthrough slot machinery. Lives alongside the legacy Send path —
	// when SendPassthrough is used, readLoop routes result events through
	// fanoutTurnResult instead of eventCh. Legacy Send (used by ACP, tests,
	// and pre-passthrough callers) is unaffected: pendingSlots stays nil
	// and readLoop keeps the existing eventCh delivery.
	//
	// Lock ordering (documented in docs/rfc/passthrough-mode.md §5.2.6):
	//   shimWMu → slotsMu  (Send path; append slot + write stdin atomically)
	//   slotsMu alone      (readLoop, cancel, reconnect)
	slotsMu          sync.Mutex
	pendingSlots     []*sendSlot // FIFO by stdin write order
	currentTurnSlots []*sendSlot // slots claimed by the in-flight turn
	turnStartedAt    time.Time   // set on system/init; zeroed on result
	inTurn           bool
	slotIDGen        atomic.Uint64

	// linker maps parallel-agent task_ids to on-disk transcript jsonl paths
	// so the dashboard's /api/sessions/agent_events endpoint can stream each
	// agent's internal events. Initialised by Wrapper.Spawn via InitLinker;
	// nil for processes that predate the agent-team UI feature (test fakes).
	linker *SubagentLinker
	// cwd is the working directory passed to Spawn. Captured so linker
	// projectDir can be (re)derived on shim reconnect without plumbing
	// SpawnOptions through every call site.
	cwd string
}

// sendSlot tracks one in-flight passthrough Send call. The slot is appended
// to Process.pendingSlots atomically with the stdin write (see Process.Send
// for the lock dance), then matched to the CLI's replay event by uuid (single
// sender) or by text (merged sender). When the turn's result arrives, the
// result is fanned out to every slot the turn claimed.
//
// canceled is the tombstone flag: when a Send caller leaves via ctx.Done, the
// slot stays in pendingSlots so FIFO positioning isn't broken, but the fan-out
// goroutine drops the result instead of trying to deliver to a channel with
// no listener. See docs/rfc/passthrough-mode.md §5.2.2.
type sendSlot struct {
	id       uint64
	uuid     string
	text     string
	priority string // "" | "now" | "next" | "later"
	onEvent  EventCallback
	resultCh chan *SendResult
	errCh    chan error

	// Only mutated under Process.slotsMu
	canceled  bool
	replayed  bool
	enqueueAt time.Time
	writtenAt time.Time
}

// isCanceled reads canceled under the lock-free atomic assumption that callers
// outside slotsMu only read — writes happen with slotsMu held. Used by fanout
// to avoid writing to a resultCh whose listener already returned.
//
// NB: this is not atomic in the strict sense, but the read-after-write ordering
// in the Send/cancel/fanout interleaving is always slotsMu-synchronised at the
// write side. The worst case of a racy read here is delivering one more
// resultCh to a canceled slot — buffered channel absorbs it; Send listener is
// already gone; memory is reclaimed at next GC. This is acceptable and lets
// fanout proceed outside slotsMu (see §5.2 lock ordering).
func (s *sendSlot) isCanceled() bool {
	// slotsMu is held by the caller in readLoop paths, but fanoutTurnResult
	// is explicitly documented to run *after* releasing slotsMu so heavy
	// channel writes don't serialize the readLoop. Re-reading here is a
	// belt-and-suspenders check; a narrow race is harmless per the note
	// above.
	return s.canceled
}

// SetSlogKey records the session key associated with this process so
// readLoop / heartbeatLoop log entries can be attributed. Called once per
// lifetime immediately after construction by Wrapper.Spawn/SpawnReconnect
// before startReadLoop runs, so Store is race-free with the reader goroutines.
// R70-ARCH-M3.
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
	DeathReasonCLIExited       = "cli_exited"
	DeathReasonShimEOF         = "shim_eof"
	DeathReasonShimReadErr     = "shim_read_error"
	DeathReasonReadLoopPanic   = "readloop_panic"
	DeathReasonKilled          = "killed"
	DeathReasonNoOutputTimeout = "no_output_timeout"
	DeathReasonTotalTimeout    = "total_timeout"
)

// setDeathReason records the death reason if not already set. First writer wins
// so the root cause is preserved even when readLoop's unwind triggers a second
// transition (e.g. cli_exited → defer hits the no-reason StateDead fallback).
//
// Uses atomic.Pointer[string].CompareAndSwap to close the TOCTOU gap between
// Load and Store that a naive "check-then-store" would leave open. Without
// CAS, two concurrent death paths (panic defer vs. cli_exited handler) can
// both observe a nil pointer on Load and then both Store, letting the slower
// goroutine overwrite the earlier classification.
//
// Each Store allocates a fresh *string (captured by address of a local); the
// CAS loop retries while an empty string is in place (defensive — no code path
// stores "" today, but tolerating the upgrade is cheap and forward-compatible).
func (p *Process) setDeathReason(reason string) {
	if reason == "" {
		return
	}
	// First attempt: store only if the pointer is still nil (never set).
	// CompareAndSwap returns true on success, so a subsequent caller observing
	// a non-nil pointer drops out here.
	fresh := reason
	if p.deathReason.CompareAndSwap(nil, &fresh) {
		return
	}
	// Upgrade path: if somebody stored a pointer to "" explicitly (not taken
	// today, but cheap to tolerate), swap it for the real reason. Snapshot
	// the current pointer and CAS against it; a concurrent non-empty writer
	// will invalidate our CAS and we bail to preserve first-writer-wins.
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

// newShimProcess creates a Process connected to a shim.
// The caller must call startReadLoop() after protocol Init.
func newShimProcess(conn net.Conn, reader *bufio.Reader, writer *bufio.Writer,
	proto Protocol, cliPID, shimPID int, noOutputTimeout, totalTimeout time.Duration) *Process {
	p := &Process{
		shimConn:        conn,
		shimR:           reader,
		shimW:           writer,
		protocol:        proto,
		caps:            ProtocolCaps(proto),
		cliPID:          cliPID,
		shimPID:         shimPID,
		State:           StateSpawning,
		eventCh:         make(chan Event, 256),
		done:            make(chan struct{}),
		killCh:          make(chan struct{}),
		noOutputTimeout: noOutputTimeout,
		totalTimeout:    totalTimeout,
		eventLog:        NewEventLog(0),
		pongRecv:        make(chan struct{}, 1),
	}
	p.stdinWriter = &shimWriter{p: p}
	return p
}

// shimStdinWriter returns an io.Writer that sends data to CLI stdin via the shim.
// Returns the same instance each call to preserve any buffered partial lines.
// Always non-nil: initialized in newShimProcess to avoid lazy-init data races
// when readLoop and Send call this concurrently on the SpawnReconnect path.
func (p *Process) shimStdinWriter() io.Writer {
	return p.stdinWriter
}

// startReadLoop begins the shim message reader goroutine and heartbeat.
func (p *Process) startReadLoop() {
	p.mu.Lock()
	p.State = StateReady
	p.mu.Unlock()
	go p.readLoop()
	go p.heartbeatLoop()
}

// findResultSince checks EventLog for a result entry logged after afterMS.
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
	return p.State == StateRunning
}

// isChanAlive reports whether done is still open (readLoop still running, so
// Kill forcefully terminates the CLI process via shim.
//
// After sending the "kill" message and closing the naozhi-side conn, send
// SIGUSR2 to the shim process itself so its immediate-shutdown path fires
// (server.go SIGUSR2 handler → initiateShutdown → Run's <-s.done case →
// listener.Close + os.Remove(socket)). Without this, a Kill leaves the
// shim in its 30s disconnect grace period holding the socket alive, and
// the next StartShim for the same key fails the dial-first guard — the
// same "refusing to clobber" class of bug Close was rewritten to avoid
// (UCCLEP-2026-04-26).
//
// shimPID may be 0 if the spawn path never observed a hello (tests,
// legacy); we skip SIGUSR2 in that case. PID reuse is a theoretical
// concern — we only signal while killOnce.Do is running, which bounds
// the window to microseconds after the shim is known to have been alive,
// so the risk is negligible.
func (p *Process) Kill() {
	p.killOnce.Do(func() {
		close(p.killCh)
		// Best-effort: send kill command with a short deadline to avoid blocking.
		// If the write fails (conn already broken), the shim's disconnect watchdog
		// will eventually kill the CLI.
		//
		// Hold shimWMu across SetWriteDeadline + shimSend + Close so we do not
		// race a concurrent shimSend (heartbeat/ping/write) whose bufio.Writer
		// is not safe against a concurrent Close()+Flush. shimSend already
		// takes shimWMu for the write, so we acquire it here and call
		// shimSendLocked which skips the re-lock.
		p.shimWMu.Lock()
		// If SetWriteDeadline fails (conn already closed / broken socket,
		// "use of closed network connection" after GC finalize), skip the
		// write entirely — without a deadline, shimSendLocked can block
		// until OS TCP keepalive expires (minutes), holding shimWMu and
		// starving every concurrent shimSend (heartbeat/ping/interrupt).
		// R218B-GO-4: log the SetWriteDeadline error at debug to aid
		// diagnosing transient socket failures during Kill.
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
			// SIGUSR2: shim's signal handler flips initiateShutdown immediately
			// and the main Run loop releases the listener and unlinks the
			// socket. A failing Signal (shim already exited, PID reused) is
			// fine — the socket is either already gone or will be reaped by
			// Discover's F4 stat-check within 30s. Windows: no-op (shim is
			// POSIX-only; see release.yml matrix note).
			if err := osutil.SendShimReload(p.shimPID); err != nil {
				slog.Debug("kill: SendShimReload failed (likely already exited)",
					"shim_pid", p.shimPID, "err", err)
			}
		}
	})
}

// Close gracefully shuts down the CLI and tears down the shim process.
//
// Sends "shutdown" instead of "close_stdin" so the shim's main loop walks
// its full exit path: closeStdin → waitOrKill(CLI) → listener.Close +
// os.Remove(socket). After p.done fires (shim EOF hits readLoop) the
// socket file is gone and any fresh StartShim for the same key will bind
// cleanly. "close_stdin" only terminated the CLI child and left the shim
// listening for up to 30s grace, which is the UCCLEP-2026-04-26 root
// cause for "refusing to clobber" on fast Reset+Recreate (fresh_context
// cron, explicit Router.Reset, config drift).
//
// Callers that want the naozhi-restart-friendly "keep shim alive" semantics
// must use Detach(), not Close().
func (p *Process) Close() {
	// R175-P1: Close() previously called p.shimSend(...) directly, which has no
	// write deadline. Kill() and Detach() both batch SetWriteDeadline+send
	// under shimWMu for good reason — without it, a shim that is alive but
	// whose TCP buffer is full (GC pause, stuck child, network wedge) pins
	// shimWMu until the OS keepalive timer fires (minutes). All other
	// shimSend callers (heartbeat, ping, interrupt) would block on the same
	// lock, and Router.Reset / shutdown would stall past the SIGTERM grace.
	// Mirror the Detach() pattern: grab shimWMu, set a short deadline, and
	// only send if the deadline was accepted. Deadline failure means the
	// conn is already broken, short-circuit to Kill().
	p.shimWMu.Lock()
	if err := p.shimConn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		p.shimWMu.Unlock()
		p.Kill()
		return
	}
	sendErr := p.shimSendLocked(shimClientMsg{Type: "shutdown"})
	// R175-P2: clear the 2s write deadline *before* releasing shimWMu so a
	// concurrent heartbeat ping inheriting the now-expired deadline cannot
	// see a stale `i/o timeout` and call Kill() — which would bypass the
	// graceful shim teardown this function is designed to drive (UCCLEP-
	// 2026-04-26). Kept under the lock to close the race window entirely.
	// SetWriteDeadline failure here is harmless: zero-time deadline is a
	// "no deadline" signal, so even on error the connection is no worse off
	// than before this defensive clear.
	_ = p.shimConn.SetWriteDeadline(time.Time{})
	p.shimWMu.Unlock()
	if sendErr != nil {
		// Write-path error (closed conn, deadline exceeded, EOF) means the
		// shim will not process the shutdown — waiting processCloseTimeout
		// on <-p.done just doubles teardown latency on the hot eviction
		// path. Fall straight through to Kill().
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

// Detach disconnects from the shim without stopping the CLI.
// Used during naozhi graceful shutdown to keep shim alive.
//
// Applies a short write deadline so Router.Shutdown's wg.Wait() cannot be
// pinned by a dead/slow socket (TCP write timeout would otherwise stretch to
// minutes, blocking SIGTERM handling).
func (p *Process) Detach() {
	p.shimWMu.Lock()
	// If SetWriteDeadline fails (conn already closed / broken), skip the
	// detach send — without a deadline, shimSendLocked can block on a
	// dead socket until TCP keepalive expires (minutes), which is
	// precisely what Detach is meant to avoid (Router.Shutdown's
	// wg.Wait() would stall past SIGTERM grace). Same pattern as Kill().
	// R218B-GO-4: log the SetWriteDeadline error at debug to aid
	// diagnosing transient socket failures during Detach.
	if err := p.shimConn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		slog.Debug("detach: SetWriteDeadline failed, skipping shim detach send", "err", err)
	} else {
		if err := p.shimSendLocked(shimClientMsg{Type: "detach"}); err != nil {
			slog.Debug("detach: shimSend failed", "err", err)
		}
	}
	p.closeShimConn()
	p.shimWMu.Unlock()
}

// closeShimConn closes p.shimConn at most once across all teardown paths
// (Kill / Detach / Close / readLoop EOF / connect-failure cleanup). Without
// the sync.Once gate, the second concurrent Close returns "use of closed
// network connection" which callers used to silently swallow with `_ =` —
// hiding any genuine close error and producing misleading entries when
// reviewers grep journalctl. R219-GO-3.
func (p *Process) closeShimConn() {
	p.shimCloseOnce.Do(func() {
		if err := p.shimConn.Close(); err != nil {
			slog.Debug("shimConn close failed", "err", err)
		}
	})
}

// GetState returns the current process state.
func (p *Process) GetState() ProcessState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.State
}

// SetOnTurnDone sets the callback invoked by readLoop when a result event
// transitions the process from Running to Ready without an active Send().
// Thread-safe: may be called while readLoop is running.
//
// The callback MUST be idempotent — readLoop can fire it multiple times in
// rapid succession when a mid-turn reconnect CAS path is immediately followed
// by a Kill (see the `onTurnDone` field godoc for the full list of fan-in
// sites). Implementations should assume "invocation count ≥ state change
// count" and perform only wake/broadcast/notify work that collapses naturally
// under repeated calls.
func (p *Process) SetOnTurnDone(fn func()) {
	p.mu.Lock()
	p.onTurnDone = fn
	p.mu.Unlock()
}

// GetSessionID returns the session ID in a thread-safe manner.
func (p *Process) GetSessionID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.SessionID
}

// TotalCost returns the cumulative cost (lock-free via atomic.Uint64).
func (p *Process) TotalCost() float64 {
	return math.Float64frombits(p.totalCost.Load())
}

// ContextUsagePercent returns the last reported context-window utilisation
// (0-100). Always 0 for backends that don't report it (claude stream-json
// today). Lock-free.
func (p *Process) ContextUsagePercent() float64 {
	return math.Float64frombits(p.contextUsagePercentBits.Load())
}

// TurnDurationMs returns the duration of the most recently completed turn,
// in ms. 0 when no turn has completed yet. Lock-free.
func (p *Process) TurnDurationMs() int64 {
	return p.turnDurationMs.Load()
}

// MeteringUsage returns a defensive copy of the most recent backend-reported
// billing rows. Empty for backends that report cost only via TotalCost
// (claude). Returns a fresh slice on every call so callers can safely retain
// the result.
func (p *Process) MeteringUsage() []MeteringEntry {
	p.meteringMu.Lock()
	defer p.meteringMu.Unlock()
	if len(p.meteringUsage) == 0 {
		return nil
	}
	out := make([]MeteringEntry, len(p.meteringUsage))
	copy(out, p.meteringUsage)
	return out
}

// applyMetadata stores normalized metadata observed on a Type:"metadata"
// event. Called from readLoop. Cheap; only ContextUsagePercent and
// TurnDurationMs are atomically stored, MeteringUsage replaces the whole
// slice under meteringMu. Each kiro turn emits at most one metadata frame
// so contention is negligible.
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
	if len(m.MeteringUsage) > 0 {
		p.meteringMu.Lock()
		// UI Round 5 R5-4: per-unit accumulator. kiro reports per-turn
		// increments; session-level totals must be summed naozhi-side
		// because kiro emits no running total. Each metadata frame's
		// entries get folded into the session's running tally by Unit.
		// Pre-existing units stay; new units are appended. Bounded
		// growth: kiro currently emits only "credit"; even worst-case
		// the unit set is ≤4 (credit, token, etc.).
		for _, in := range m.MeteringUsage {
			merged := false
			for i := range p.meteringUsage {
				if p.meteringUsage[i].Unit == in.Unit {
					p.meteringUsage[i].Value += in.Value
					if in.UnitPlural != "" {
						p.meteringUsage[i].UnitPlural = in.UnitPlural
					}
					merged = true
					break
				}
			}
			if !merged {
				p.meteringUsage = append(p.meteringUsage, in)
			}
		}
		p.meteringMu.Unlock()
	}
}

// ProtocolName returns the protocol name.
func (p *Process) ProtocolName() string {
	return p.protocol.Name()
}

// setModel records the spawn-time model. Called once by Wrapper.Spawn
// before readLoop starts; never re-set afterwards. UI Round 5 R5-3.
func (p *Process) setModel(model string) {
	if model == "" {
		p.model.Store(nil)
		return
	}
	m := model
	p.model.Store(&m)
}

// Model returns the spawn-time CLI model identifier, or "" if the
// operator did not configure one. Lock-free; safe to call from any
// goroutine including Snapshot's hot read path.
func (p *Process) Model() string {
	if m := p.model.Load(); m != nil {
		return *m
	}
	return ""
}

// PID returns the CLI process ID (as reported by shim).
func (p *Process) PID() int {
	return p.cliPID
}

// GetTotalTimeout returns the configured total timeout for a single turn.
func (p *Process) GetTotalTimeout() time.Duration {
	if p.totalTimeout > 0 {
		return p.totalTimeout
	}
	return DefaultTotalTimeout
}

// LastSeq returns the last received shim sequence number (for reconnect).
func (p *Process) LastSeq() int64 { return p.lastSeq.Load() }
