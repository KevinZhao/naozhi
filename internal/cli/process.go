package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
	shimConn    net.Conn
	shimR       *bufio.Reader
	shimW       *bufio.Writer
	shimWMu     sync.Mutex
	stdinWriter *shimWriter // cached shimStdinWriter instance
	protocol    Protocol
	cliPID      int // CLI PID reported by shim hello
	shimPID     int // shim PID reported by shim hello; used by Kill() for SIGUSR2 fallback

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
	lastSeq   atomic.Int64  // last received shim seq, for reconnect
	pongRecv  chan struct{} // signaled by readLoop on pong receipt

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
		if err := p.shimConn.SetWriteDeadline(time.Now().Add(time.Second)); err == nil {
			_ = p.shimSendLocked(shimClientMsg{Type: "kill"})
		}
		_ = p.shimConn.Close()
		p.shimWMu.Unlock()

		if p.shimPID > 0 {
			// SIGUSR2: shim's signal handler flips initiateShutdown immediately
			// and the main Run loop releases the listener and unlinks the
			// socket. A failing Signal (shim already exited, PID reused) is
			// fine — the socket is either already gone or will be reaped by
			// Discover's F4 stat-check within 30s. Windows: no-op (shim is
			// POSIX-only; see release.yml matrix note).
			if err := signalShimShutdown(p.shimPID); err != nil {
				slog.Debug("kill: signalShimShutdown failed (likely already exited)",
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
	if err := p.shimConn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err == nil {
		_ = p.shimSendLocked(shimClientMsg{Type: "detach"})
	}
	_ = p.shimConn.Close()
	p.shimWMu.Unlock()
}

// EventEntryFromEvent converts an Event to a single EventEntry.
// Deprecated for multi-block assistant events — use EventEntriesFromEvent.
// Kept for callers that only need the first entry (or non-assistant events).
func EventEntryFromEvent(ev Event) (EventEntry, bool) {
	entries := EventEntriesFromEvent(ev)
	if len(entries) == 0 {
		return EventEntry{}, false
	}
	return entries[0], true
}

// EventEntriesFromEvent converts an Event to zero or more EventEntry values.
// Assistant messages can contain multiple content blocks (thinking + tool_use + text);
// each block that maps to a known type produces its own entry so downstream consumers
// (EventLog, dashboard) don't silently drop blocks after the first.
func EventEntriesFromEvent(ev Event) []EventEntry {
	return EventEntriesFromEventAt(ev, time.Now().UnixMilli())
}

// EventEntriesFromEventAt is the caller-supplied-now variant used by readLoop
// to share a single time.Now() call between ev.recvAt assignment and entry
// timestamping. Public callers still use EventEntriesFromEvent. R67-PERF-9.
func EventEntriesFromEventAt(ev Event, nowMS int64) []EventEntry {
	// Replay events are a passthrough-internal CLI ack for messages naozhi
	// already showed to the user via the optimistic bubble. Writing them to
	// EventLog causes double-display on the dashboard. readLoop already
	// short-circuits replay events before logEventAt, but belt-and-suspenders:
	// if any future caller passes a replay directly, still skip.
	if ev.Type == "user" && ev.IsReplay {
		return nil
	}
	now := nowMS
	base := EventEntry{Time: now}

	switch ev.Type {
	case "system":
		entry := base
		entry.Type = "system"
		entry.Summary = ev.SubType
		if ev.SubType == "init" {
			return nil
		}
		switch ev.SubType {
		case "task_started":
			entry.Type = "task_start"
			entry.TaskID = ev.TaskID
			entry.ToolUseID = ev.ToolUseID
			entry.TaskType = ev.TaskType
			if ev.Description != "" {
				entry.Summary = TruncateRunes(ev.Description, 120)
			}
		case "task_progress", "task_updated":
			entry.Type = "task_progress"
			entry.TaskID = ev.TaskID
			entry.ToolUseID = ev.ToolUseID
			if ev.Description != "" {
				entry.Summary = TruncateRunes(ev.Description, 120)
			}
			entry.LastTool = ev.LastToolName
			if ev.Usage != nil {
				entry.ToolUses = ev.Usage.ToolUses
				entry.Tokens = ev.Usage.TotalTokens
				entry.DurationMS = ev.Usage.DurationMS
			}
		case "task_notification":
			entry.Type = "task_done"
			entry.TaskID = ev.TaskID
			entry.ToolUseID = ev.ToolUseID
			if ev.Description != "" {
				entry.Summary = TruncateRunes(ev.Description, 120)
			}
			entry.Status = ev.Status
			if ev.Usage != nil {
				entry.ToolUses = ev.Usage.ToolUses
				entry.Tokens = ev.Usage.TotalTokens
				entry.DurationMS = ev.Usage.DurationMS
			}
		case "stop_hook_summary", "turn_duration", "hook_started", "hook_response":
			return nil
		}
		return []EventEntry{entry}
	case "assistant":
		if ev.Message == nil {
			return nil
		}
		// Pre-size to the content block count: single-block events pay 1
		// alloc (same as the old nil+append path), and multi-block events
		// (thinking+tool_use+text) avoid 2-3 append-driven growth reallocs.
		out := make([]EventEntry, 0, len(ev.Message.Content))
		for _, block := range ev.Message.Content {
			entry := base
			switch block.Type {
			case "thinking":
				entry.Type = "thinking"
				entry.Summary = TruncateRunes(block.Text, 120)
				entry.Detail = TruncateRunes(block.Text, 2000)
			case "tool_use":
				entry.Type = "tool_use"
				entry.Summary = block.Name
				entry.Tool = block.Name
				entry.Detail = formatToolDetail(block)
				switch block.Name {
				case "Agent":
					inp := parseAgentInput(block.Input)
					entry.Type = "agent"
					entry.Subagent = inp.SubagentType
					if entry.Subagent == "" {
						entry.Subagent = inp.Name
					}
					entry.TeamName = inp.TeamName
					entry.Summary = TruncateRunes(inp.Description, 120)
					entry.Background = inp.RunInBackground
					entry.ToolUseID = block.ID
				case "TodoWrite":
					if todos, ok := ParseTodos(block.Input); ok {
						entry.Type = "todo"
						entry.Tool = "TodoWrite"
						entry.Summary = TodosSummary(todos)
						// Dashboard renderTodoList expects a JSON array of
						// TodoItem, not the full `{"todos":[...]}` envelope
						// that block.Input carries. Marshal the decoded slice
						// so the frontend sees `[{...}, {...}]` and renders
						// the checklist; otherwise JSON.parse yields an
						// object, Array.isArray returns false, and the UI
						// silently falls back to the one-line summary.
						entry.Detail = TodosDetailJSON(todos)
					}
				}
			case "text":
				entry.Type = "text"
				entry.Summary = TruncateRunes(block.Text, 120)
				entry.Detail = TruncateRunes(block.Text, 16000)
			default:
				continue
			}
			out = append(out, entry)
		}
		// Surface the AskUserQuestion card as a dedicated EventEntry alongside
		// the tool_use bubble. Placing it AFTER the tool_use entry keeps the
		// chronological transcript order (tool_use → interactive card) and
		// preserves the Agent → task_started tool_use_id linkage above.
		if ev.AskQuestion != nil {
			entry := base
			entry.Type = "ask_question"
			entry.Tool = "AskUserQuestion"
			entry.ToolUseID = ev.AskQuestion.ToolUseID
			// Summary is a one-line digest used for sidebar preview;
			// AskQuestion field carries the full card payload.
			if len(ev.AskQuestion.Items) > 0 {
				entry.Summary = TruncateRunes(ev.AskQuestion.Items[0].Question, 120)
			} else {
				entry.Summary = "AskUserQuestion"
			}
			entry.AskQuestion = ev.AskQuestion
			out = append(out, entry)
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case "result":
		entry := base
		entry.Type = "result"
		entry.Summary = TruncateRunes(ev.Result, 200)
		entry.Detail = TruncateRunes(ev.Result, 16000)
		entry.Cost = ev.CostUSD
		return []EventEntry{entry}
	}
	return nil
}

// logEventAt converts an Event to one or more EventEntry values and appends them to the event log.
// readLoop passes the same time.Now() value that stamps ev.recvAt so timestamps match. R67-PERF-9.
func (p *Process) logEventAt(ev Event, nowMS int64) {
	entries := EventEntriesFromEventAt(ev, nowMS)
	if len(entries) == 0 {
		return
	}
	// Update process-level cost tracking for result events.
	if ev.Type == "result" {
		p.totalCost.Store(math.Float64bits(ev.CostUSD))
	}
	// AppendBatch holds l.mu and notifies subscribers ONCE rather than
	// once per entry. Multi-block assistant events (thinking + tool_use +
	// text) would otherwise acquire both locks N times and wake
	// eventPushLoop spuriously for each block.
	p.eventLog.AppendBatch(entries)
}

// agentInput holds the parsed fields from an Agent tool call input.
type agentInput struct {
	SubagentType    string `json:"subagent_type"`
	Name            string `json:"name"`
	TeamName        string `json:"team_name"`
	Description     string `json:"description"`
	RunInBackground bool   `json:"run_in_background"`
}

func parseAgentInput(input json.RawMessage) agentInput {
	if len(input) == 0 {
		return agentInput{}
	}
	var inp agentInput
	if err := json.Unmarshal(input, &inp); err != nil {
		// R188-PANIC-H1: upgrade from Debug to Warn. Silent zero-value return
		// produces blank agent cards in the dashboard; at Warn operators can
		// trace which CLI emitted a malformed Agent.input and whether the
		// schema drifted (e.g. CLI emitting "input": "string" instead of
		// {"subagent_type": ...}).
		slog.Warn("parseAgentInput: unmarshal failed",
			"err", err, "input_len", len(input))
	}
	return inp
}

// label returns the preferred human-readable identifier for an Agent tool call.
// Used by tests that lock the Agent event formatting contract.
func (a agentInput) label() string {
	if a.SubagentType != "" {
		return a.SubagentType
	}
	if a.Name != "" {
		return a.Name
	}
	return a.TeamName
}

func formatToolDetail(block ContentBlock) string {
	if len(block.Input) == 0 {
		return block.Name
	}
	return FormatToolInput(block.Name, block.Input)
}

func shortPath(p string) string {
	const homePrefix = "/home/"
	if i := strings.Index(p, homePrefix); i >= 0 {
		rest := p[i+len(homePrefix):]
		if j := strings.Index(rest, "/"); j >= 0 {
			return "~" + rest[j:]
		}
	}
	if len(p) > 50 {
		return "..." + p[len(p)-47:]
	}
	return p
}

// FormatToolInput extracts a human-readable summary from a tool's JSON input.
// Uses per-tool struct parsing to avoid map allocation on the hot path.
func FormatToolInput(toolName string, input json.RawMessage) string {
	if len(input) == 0 {
		return toolName
	}

	switch toolName {
	case "Read", "Write", "Edit":
		var s struct {
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal(input, &s) == nil && s.FilePath != "" {
			return toolName + " " + shortPath(s.FilePath)
		}
	case "Glob":
		var s struct {
			Pattern string `json:"pattern"`
		}
		if json.Unmarshal(input, &s) == nil && s.Pattern != "" {
			// R187-PERF-L1: cap pattern to prevent an adversarial LLM response
			// from inflating EventLog entries (300 runes matches the default
			// tail below).
			return toolName + " " + TruncateRunes(s.Pattern, 300)
		}
	case "Grep":
		var s struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if json.Unmarshal(input, &s) == nil && s.Pattern != "" {
			// R187-PERF-L1: cap pattern (see Glob note).
			result := toolName + " " + TruncateRunes(s.Pattern, 300)
			if s.Path != "" {
				result += " in " + shortPath(s.Path)
			}
			return result
		}
	case "Bash":
		var s struct {
			Description string `json:"description"`
			Command     string `json:"command"`
		}
		if json.Unmarshal(input, &s) == nil {
			if s.Description != "" {
				return toolName + " " + s.Description
			}
			if s.Command != "" {
				return toolName + " " + TruncateRunes(s.Command, 80)
			}
		}
	case "Agent":
		var s struct {
			Description string `json:"description"`
		}
		if json.Unmarshal(input, &s) == nil && s.Description != "" {
			return toolName + " " + TruncateRunes(s.Description, 60)
		}
	default:
		// R188-PERF-P2-C: replaced map[string]json.RawMessage decode with
		// concrete struct — json.Decoder ignores unknown fields by default so
		// MCP tools that add new schemas still work, and we skip the reflect
		// + map alloc cost on the unknown-tool fallback path.
		// Fallback: try common keys with a struct (rare path for unknown tools)
		var inp struct {
			Description string `json:"description"`
			FilePath    string `json:"file_path"`
			Path        string `json:"path"`
			Command     string `json:"command"`
			Pattern     string `json:"pattern"`
			Prompt      string `json:"prompt"`
		}
		if json.Unmarshal(input, &inp) == nil {
			// Avoid a []string{...} slice literal on the unknown-tool
			// fallback path — a chain of short-circuit checks matches the
			// previous semantics without the per-call slice alloc.
			switch {
			case inp.Description != "":
				return toolName + " " + TruncateRunes(inp.Description, 80)
			case inp.FilePath != "":
				return toolName + " " + TruncateRunes(inp.FilePath, 80)
			case inp.Path != "":
				return toolName + " " + TruncateRunes(inp.Path, 80)
			case inp.Command != "":
				return toolName + " " + TruncateRunes(inp.Command, 80)
			case inp.Pattern != "":
				return toolName + " " + TruncateRunes(inp.Pattern, 80)
			case inp.Prompt != "":
				return toolName + " " + TruncateRunes(inp.Prompt, 80)
			}
		}
	}

	return toolName + ": " + TruncateRunes(string(input), 300)
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

// ProtocolName returns the protocol name.
func (p *Process) ProtocolName() string {
	return p.protocol.Name()
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

// InjectHistory pre-populates the event log with historical entries. Also
// seeds the SubagentLinker so team agent rows in the dashboard banner can
// resume the task_id → jsonl mapping established in a previous process
// lifetime (RFC v4 agent-team-ui §3.3.7 — A3 defence against CLI-dead
// respawn changing session_uuid).
func (p *Process) InjectHistory(entries []EventEntry) {
	p.eventLog.AppendBatch(entries)
	if p.linker == nil {
		return
	}
	p.linker.SeedFromHistory(entries)

	// A3 second-leg: entries persisted by an older naozhi (before the RFC v4
	// §3.2.2 four-field backfill shipped) lack InternalAgentID / JSONLPath,
	// so SeedFromHistory skips them. Yet the in-process team agents those
	// entries describe may still be running under the live shim — their
	// task_id → jsonl mapping has to come from an active Resolve. Walk the
	// batch for terminal-but-unresolved "agent" / "task_start" pairs and
	// kick off async Resolves so the dashboard drill-in has something to
	// serve when the user clicks.
	//
	// agent: agent entries carry {ToolUseID, Subagent, Background};
	//        task_start entries carry {TaskID, ToolUseID} and usually arrive
	//        right after the paired agent entry.
	// We index task_start by ToolUseID so a single pass assembles the args
	// Resolve wants: (taskID, toolUseID, name, description, agentToolUseMS).
	// De-dupe by task_id so we fire at most one Resolve per unique task.
	// Ring-buffered history commonly carries many task_progress entries
	// for the same task_id; the fast path inside Resolve is cached after
	// the first hit but we want to avoid the goroutine churn.
	seen := make(map[string]struct{})
	taskStartByToolUse := make(map[string]EventEntry, len(entries))
	for _, e := range entries {
		if e.Type == "task_start" && e.ToolUseID != "" {
			taskStartByToolUse[e.ToolUseID] = e
		}
	}
	kick := func(taskID, toolUseID, name, desc string, wallclock int64) {
		if taskID == "" {
			return
		}
		if _, ok := seen[taskID]; ok {
			return
		}
		seen[taskID] = struct{}{}
		linker := p.linker
		go linker.Resolve(taskID, toolUseID, name, desc, wallclock)
	}
	for _, e := range entries {
		switch e.Type {
		case "agent":
			if e.ToolUseID == "" || e.InternalAgentID != "" {
				continue
			}
			ts, ok := taskStartByToolUse[e.ToolUseID]
			if !ok || ts.TaskID == "" {
				continue
			}
			name := e.Subagent
			if name == "" {
				name = e.TeamName
			}
			kick(ts.TaskID, e.ToolUseID, name, e.Summary, e.Time)
		case "task_start", "task_progress":
			// Orphan task path — the originating agent entry was evicted
			// from the ring buffer before the replay window. Without this
			// the dashboard sees a banner row (rebuilt from task_progress
			// via the frontend) but Linker.Query returns ok=false for the
			// task_id, so the HTTP endpoint serves 202 forever. Fast-path
			// Resolve by task_id works because Claude 2.1.132 names the
			// jsonl file after the task_id directly.
			if e.TaskID == "" || e.InternalAgentID != "" {
				continue
			}
			kick(e.TaskID, e.ToolUseID, e.Subagent, e.Summary, e.Time)
		}
	}
}

// InitLinker wires a SubagentLinker into the process. Called by Wrapper.Spawn
// once the working directory is known. Safe to call before the first system.init
// event — the Linker is context-free until SetContext fires from readLoop's
// init handler.
//
// The OnResolve callback writes the resolved (internal_agent_id, jsonl_path,
// first_prompt_id) tuple back onto the matching "agent" / "task_start"
// EventEntry so persistHistory flushes a self-contained record.
func (p *Process) InitLinker(cwd string) {
	p.cwd = cwd
	p.linker = NewSubagentLinker()
	log := p.eventLog
	p.linker.OnResolve(func(taskID, toolUseID, internalAgentID string) {
		if toolUseID == "" || log == nil {
			return
		}
		info, _ := p.linker.Query(taskID)
		log.SetAgentInternalID(toolUseID, internalAgentID, info.JSONLPath, info.FirstPromptID)
	})
}

// Linker returns the SubagentLinker, or nil when none has been installed
// (test fakes / unusual spawn paths).
func (p *Process) Linker() *SubagentLinker {
	return p.linker
}

// EventLog returns the process's underlying *EventLog so the server-side
// tailer registry can register its SetOnAgentTaskDone callback. Returning
// the concrete type is a minor API widening but stays symmetric with
// Linker() above — both are escape hatches for tightly-coupled server
// integrations that the public method set does not otherwise expose.
func (p *Process) EventLog() *EventLog {
	return p.eventLog
}

// SetCwdForLinker plumbs the working directory into the Linker after a shim
// reconnect. SpawnReconnect does not have cwd handy (shim owns it); the
// session router provides it once the session record is re-read. Safe to
// call any time — updates projectDir and, when the reconnect handshake
// already carried a session_id, also populates parentSessionID so Resolve
// can succeed without waiting for the next live system.init event (which
// never fires when the CLI is idle post-reconnect).
func (p *Process) SetCwdForLinker(cwd string) {
	if p.linker == nil || cwd == "" {
		return
	}
	p.cwd = cwd
	projectDir := resolveProjectDir(cwd)
	p.linker.mu.RLock()
	session := p.linker.parentSessionID
	p.linker.mu.RUnlock()
	// On reconnect the wrapper populates proc.SessionID from
	// handle.Hello.SessionID BEFORE the first live init event arrives —
	// mirror that into the linker so Resolve works immediately on
	// historical agent tasks replayed via InjectHistory. If readLoop
	// later ingests an init with a different id, the normal SetContext
	// call in the readLoop path updates it.
	if session == "" && p.SessionID != "" {
		session = p.SessionID
	}
	p.linker.SetContext(projectDir, session)
}

// EventEntries returns a copy of all event log entries.
func (p *Process) EventEntries() []EventEntry {
	return p.eventLog.Entries()
}

// EventLastN returns the most recent n event log entries.
func (p *Process) EventLastN(n int) []EventEntry {
	return p.eventLog.LastN(n)
}

// EventEntriesSince returns event log entries after the given unix ms timestamp.
func (p *Process) EventEntriesSince(afterMS int64) []EventEntry {
	return p.eventLog.EntriesSince(afterMS)
}

// EventEntriesBefore returns up to `limit` event log entries strictly older
// than beforeMS, in chronological order. Used by dashboard pagination to
// load earlier pages of history.
func (p *Process) EventEntriesBefore(beforeMS int64, limit int) []EventEntry {
	return p.eventLog.EntriesBefore(beforeMS, limit)
}

// LastEntryOfType returns the most recent event entry with the given type.
func (p *Process) LastEntryOfType(typ string) EventEntry {
	return p.eventLog.LastEntryOfType(typ)
}

// TurnAgents returns the sub-agent types spawned in the current turn.
func (p *Process) TurnAgents() []SubagentInfo {
	return p.eventLog.TurnAgents()
}

// LastActivitySummary returns the summary of the most recent tool_use/thinking
// entry, as maintained atomically by EventLog.Append.
func (p *Process) LastActivitySummary() string {
	return p.eventLog.LastActivitySummary()
}

// LastEventAt returns the wall-clock time of the most recent live event
// observed by this process's EventLog. Zero Time means no live event has
// landed yet. Consumed by Router.Cleanup to treat a long-running turn
// (e.g. a 20-minute code analysis) as still active as long as the CLI
// keeps emitting tool_use / thinking / assistant events, rather than
// falsely flagging it as stuck when session.lastActive was last touched
// at Send entry. Lock-free.
func (p *Process) LastEventAt() time.Time {
	return p.eventLog.LastEventAt()
}

// UserTurnCount returns the cumulative count of "user" entries this Process
// has observed since spawn. Pass-through to EventLog; consumed by
// ManagedSession.Snapshot to populate SessionSnapshot.MessageCount for
// sidebar / main-header chip display.
func (p *Process) UserTurnCount() int64 {
	return p.eventLog.UserTurnCount()
}

// SubscribeEvents returns a notification channel and unsubscribe function.
func (p *Process) SubscribeEvents() (<-chan struct{}, func()) {
	return p.eventLog.Subscribe()
}

// LastSeq returns the last received shim sequence number (for reconnect).
func (p *Process) LastSeq() int64 { return p.lastSeq.Load() }
