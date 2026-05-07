package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

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

// shimMsg is a minimal struct for parsing shim protocol messages in readLoop.
type shimMsg struct {
	Type   string `json:"type"`
	Seq    int64  `json:"seq,omitempty"`
	Line   string `json:"line,omitempty"`
	Code   *int   `json:"code,omitempty"`
	Signal string `json:"signal,omitempty"`
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
	// mu protects State / SessionID / onTurnDone / totalCost. Read-only
	// accessors (GetState / IsRunning / GetSessionID / TotalCost) use RLock
	// so concurrent ListSessions snapshots across N sessions and M tabs
	// proceed in parallel instead of serialising through a single Mutex.
	// Write paths (readLoop state transitions, Send State→Running, Interrupt
	// snapshot-and-flag) continue to use Lock to preserve the existing
	// "read State and set interrupted together under one lock" contract.
	// R70-PERF-L3.
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
	totalCost float64
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

// shimWriter wraps shim protocol write commands as an io.Writer.
// Thread-safe: readLoop (HandleEvent) and Send (WriteMessage) may call concurrently.
//
// Lock ordering: shimWriter.mu -> Process.shimWMu.
// Write() holds w.mu, then calls p.shimSend() which acquires p.shimWMu internally.
// Callers that already hold p.shimWMu (e.g. Kill's pre-close write) must NOT
// go through shimWriter.Write — use shimSendLocked directly to avoid a reverse
// lock ordering and potential deadlock.
type shimWriter struct {
	p   *Process
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *shimWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Fast path: buffer is empty and data is a single complete line ending in '\n'.
	// This is the normal path from Protocol.WriteMessage.
	// The embedded-newline guard ensures multi-line data falls through to the
	// slow path which splits on '\n' correctly.
	if w.buf.Len() == 0 && len(data) > 0 && data[len(data)-1] == '\n' &&
		bytes.IndexByte(data[:len(data)-1], '\n') == -1 {
		if len(data)-1 > maxStdinLineBytes {
			return 0, fmt.Errorf("%w: %d bytes > %d", ErrMessageTooLarge, len(data)-1, maxStdinLineBytes)
		}
		trimmed := string(data[:len(data)-1])
		if err := w.p.shimSend(shimClientMsg{Type: "write", Line: trimmed}); err != nil {
			return 0, err
		}
		return len(data), nil
	}

	// Slow path: fragmented writes, use buffer.
	w.buf.Write(data)
	for {
		line, err := w.buf.ReadBytes('\n')
		if err != nil {
			// No newline yet — put the partial data back
			w.buf.Write(line)
			break
		}
		// ReadBytes guarantees len(line) >= 1 when err == nil (line ends in '\n'),
		// but stay defensive: a zero-length line would panic on the slice below.
		if len(line) == 0 {
			continue
		}
		if len(line)-1 > maxStdinLineBytes {
			// The offending line was already consumed from w.buf by ReadBytes
			// above; discard any trailing partial lines so the next Write()
			// doesn't concatenate fresh data onto a broken prefix the shim
			// never received.
			w.buf.Reset()
			return 0, fmt.Errorf("%w: %d bytes > %d", ErrMessageTooLarge, len(line)-1, maxStdinLineBytes)
		}
		trimmed := string(line[:len(line)-1])
		if err := w.p.shimSend(shimClientMsg{Type: "write", Line: trimmed}); err != nil {
			// Same reason as the size-limit branch: the failed line was
			// already consumed, so leaving the remainder in the buffer would
			// produce a corrupted stitched message on retry.
			w.buf.Reset()
			return 0, err
		}
	}
	return len(data), nil
}

// shimClientMsg is the outgoing message format to the shim.
type shimClientMsg struct {
	Type  string `json:"type"`
	Line  string `json:"line,omitempty"`
	Token string `json:"token,omitempty"`
	Seq   int64  `json:"last_seq,omitempty"`
}

// shimSendEnc pairs a pooled bytes.Buffer with a json.Encoder bound to it.
// Both are reused across calls so the hot shimSend path has zero encoder
// allocations. The Encoder holds a *bytes.Buffer by pointer, so resetting
// the buffer between uses is safe — the Encoder writes into the same buffer
// on every call.
type shimSendEnc struct {
	buf *bytes.Buffer
	enc *json.Encoder
}

var shimSendBufPool = sync.Pool{
	New: func() any {
		buf := new(bytes.Buffer)
		enc := json.NewEncoder(buf)
		// Shim wire messages carry user content that may contain '<', '>',
		// '&' (code blocks, HTML snippets). The default json.Marshal HTML-
		// escape would deliver `\u003c` style strings to the shim and on to
		// the Claude CLI stdin, subtly mangling payloads.
		enc.SetEscapeHTML(false)
		return &shimSendEnc{buf: buf, enc: enc}
	},
}

// encodeShimMsg marshals msg into a fresh pooled buffer with HTML escaping
// disabled. Caller MUST Put the returned buffer back into shimSendBufPool
// (typically via defer) after the Write+Flush completes.
//
// Encoding outside the write lock keeps shimWMu held only for the length of
// the actual socket write: large messages (e.g. 400KB thumbnails) otherwise
// serialize ping/interrupt on the encoder itself.
func encodeShimMsg(msg shimClientMsg) (*shimSendEnc, error) {
	se := shimSendBufPool.Get().(*shimSendEnc)
	se.buf.Reset()
	// Encoder appends its own trailing '\n' per NDJSON framing, so we must
	// not add one manually.
	if err := se.enc.Encode(msg); err != nil {
		// Do not return this entry to the pool: json.Encoder is not
		// documented to leave clean state after a failed Encode, and
		// buf may hold partial bytes. Let GC reclaim it; the pool's New
		// func will allocate a fresh pair on the next Get.
		return nil, err
	}
	return se, nil
}

// shimSendBufMaxCap caps the buffer capacity we return to the pool. Large
// payloads (e.g. 400KB image paste) grow the underlying bytes.Buffer and
// sync.Pool will not trim it; once a few big messages have passed through,
// pooled entries would permanently hold large backing arrays. Entries that
// exceed this cap are dropped so GC reclaims them; the pool's New allocator
// will produce a fresh small buffer on the next Get.
const shimSendBufMaxCap = 64 * 1024

func returnShimSendEnc(se *shimSendEnc) {
	if se.buf.Cap() > shimSendBufMaxCap {
		return
	}
	shimSendBufPool.Put(se)
}

func (p *Process) shimSend(msg shimClientMsg) error {
	se, err := encodeShimMsg(msg)
	if err != nil {
		return err
	}
	defer returnShimSendEnc(se)

	p.shimWMu.Lock()
	defer p.shimWMu.Unlock()
	if _, err := p.shimW.Write(se.buf.Bytes()); err != nil {
		return err
	}
	return p.shimW.Flush()
}

// shimSendLocked is the locked variant of shimSend. The caller MUST hold
// p.shimWMu. Kill() uses this to batch SetWriteDeadline+send+Close under a
// single lock acquisition to avoid racing a concurrent shimSend.
func (p *Process) shimSendLocked(msg shimClientMsg) error {
	se, err := encodeShimMsg(msg)
	if err != nil {
		return err
	}
	defer returnShimSendEnc(se)

	if _, err := p.shimW.Write(se.buf.Bytes()); err != nil {
		return err
	}
	return p.shimW.Flush()
}

// startReadLoop begins the shim message reader goroutine and heartbeat.
func (p *Process) startReadLoop() {
	p.mu.Lock()
	p.State = StateReady
	p.mu.Unlock()
	go p.readLoop()
	go p.heartbeatLoop()
}

// readLoop reads NDJSON messages from the shim socket and dispatches events.
func (p *Process) readLoop() {
	log := p.slogger()
	// RNEW-007: Defers execute LIFO. Declaration order below is:
	//   close(eventCh) -> close(done) -> CloseSubscribers -> recover
	// Execution order on return is the reverse:
	//   1. recover block: transition p.State to StateDead and fire onTurnDone
	//   2. CloseSubscribers: unblock EventLog subscribers
	//   3. close(done): signal readLoop exit to waiters
	//   4. close(eventCh): isChanAlive relies on done closing BEFORE eventCh so
	//      any producer guarded by "is done open?" never sends on a closed
	//      eventCh. See drainStaleEvents / isChanAlive for the invariant.
	// If you reorder these defers, re-verify the isChanAlive invariant.
	defer close(p.eventCh)
	defer close(p.done)
	defer p.eventLog.CloseSubscribers()
	// Panic recover: a malformed shim message or protocol bug must not take
	// the whole process down silently. We log stack + transition to Dead so
	// the router can reap this session and the dashboard surfaces the failure
	// instead of the user seeing a stalled "running" forever.
	defer func() {
		if r := recover(); r != nil {
			log.Error("readLoop panic recovered",
				"panic", r, "stack", string(debug.Stack()))
			p.setDeathReason(DeathReasonReadLoopPanic)
			p.mu.Lock()
			p.State = StateDead
			cb := p.onTurnDone
			p.mu.Unlock()
			if cb != nil {
				cb()
			}
		}
	}()

	// Reuse the line accumulator across iterations to avoid an allocation
	// per event. Most stream-json events are well under 4KB; the 4096 cap
	// matches bufio's default buffer so single-chunk lines rarely grow.
	// We reset length (not capacity) at the top of each iteration, and
	// carry any grown capacity forward via lineBuf = line so a single large
	// event doesn't force every subsequent iteration to re-grow from 4KB.
	lineBuf := make([]byte, 0, 4096)
	for {
		// bufio.ReadBytes grows its internal buffer without bound; a buggy or
		// hostile shim that emits a multi-GB line without '\n' would OOM
		// naozhi before the post-read size check below fires. Accumulate via
		// ReadSlice chunks so we can bail the moment the cap is exceeded.
		line := lineBuf[:0]
		var readErr error
		capExceeded := false
		for {
			chunk, err := p.shimR.ReadSlice('\n')
			if len(chunk) > 0 {
				if len(line)+len(chunk) > maxScannerBufBytes {
					capExceeded = true
					break
				}
				line = append(line, chunk...)
			}
			if err == nil {
				break // terminator found
			}
			// R182-GO-P1-2: use errors.Is so a wrapped ErrBufferFull (from
			// future middleware or bufio chain) still matches. Matches the
			// errors.Is(readErr, io.EOF) style used elsewhere in this loop.
			if errors.Is(err, bufio.ErrBufferFull) {
				continue // keep reading until newline or cap
			}
			readErr = err
			break
		}
		// Propagate grown capacity so the next iteration starts with the
		// expanded backing array instead of reverting to the original 4096.
		// Without this, a single large event forces every subsequent
		// iteration to re-grow from 4KB through a chain of doublings.
		//
		// Exception: on capExceeded we shrink back to a fresh 4KB buffer.
		// Holding onto a ~16MB backing array forever because one malformed
		// shim message grew us there is a silent memory hog; the rare legit
		// large event will pay the re-grow cost again rather than keeping
		// pathological capacity on the hot path.
		lineBuf = line
		if capExceeded {
			log.Warn("readLoop: oversized shim message, skipping", "size", len(line))
			lineBuf = make([]byte, 0, 4096)
			// Drain the rest of this overlong line so the next iteration
			// doesn't read the tail as a separate message.
			for {
				// bufio.ReadSlice only returns nil when the delimiter was
				// found; ErrBufferFull means the internal buffer filled with
				// no '\n'. Any other error terminates the drain.
				_, err := p.shimR.ReadSlice('\n')
				if err == nil {
					break
				}
				// R182-GO-P1-2: errors.Is to survive future wrapping; same
				// reason as the first call site above.
				if !errors.Is(err, bufio.ErrBufferFull) {
					readErr = err
					break
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) || errors.Is(readErr, net.ErrClosed) {
					log.Info("readLoop: shim connection closed after oversize drain")
					p.setDeathReason(DeathReasonShimEOF)
				} else {
					log.Warn("readLoop: shim read error after oversize drain", "err", readErr)
					p.setDeathReason(DeathReasonShimReadErr)
				}
				break
			}
			continue
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, net.ErrClosed) {
				log.Info("readLoop: shim connection closed")
				p.setDeathReason(DeathReasonShimEOF)
			} else {
				log.Warn("readLoop: shim read error", "err", readErr)
				p.setDeathReason(DeathReasonShimReadErr)
			}
			break
		}

		// bufio.ReadBytes('\n') returns the delimiter; strip only the tail '\n'
		// (and optional '\r') instead of bytes.TrimSpace which scans both ends.
		// json.Unmarshal handles leading whitespace inside the payload.
		trimmed := line
		if n := len(trimmed); n > 0 && trimmed[n-1] == '\n' {
			trimmed = trimmed[:n-1]
			if n > 1 && trimmed[n-2] == '\r' {
				trimmed = trimmed[:n-2]
			}
		}
		var msg shimMsg
		if err := json.Unmarshal(trimmed, &msg); err != nil {
			log.Warn("readLoop: skip unparseable shim message", "err", err, "size", len(line))
			continue
		}

		switch msg.Type {
		case "stdout":
			p.lastSeq.Store(msg.Seq)
			ev, _, err := p.protocol.ReadEvent(msg.Line)
			if err != nil {
				log.Warn("readLoop: skip unparseable event", "err", err, "seq", msg.Seq)
				continue
			}
			if ev.Type == "" {
				continue
			}
			if p.protocol.HandleEvent(p.shimStdinWriter(), ev) {
				continue
			}

			// Capture one time.Now() shared between ev.recvAt (handed to
			// drainStaleEvents) and the EventEntry.Time values produced by
			// logEventAt. Previously the two read wall-clock independently,
			// which is measurable at 5-50 events/s × N active sessions.
			// R67-PERF-9.
			now := time.Now()

			// ---- Passthrough mode hooks ----
			// These run before the legacy eventCh / EventLog delivery paths.
			// They are cheap no-ops when passthrough is not in use (zero
			// pending slots, inTurn=false, protocol doesn't support replay).

			// system/init: mark start of new turn for turn-aggregation owner
			// tracking and watchdog baseline. Keeping this unconditional is
			// harmless — onSystemInit only matters when pendingSlots is
			// non-empty and a replay arrives later.
			if ev.Type == "system" && ev.SubType == "init" && p.protocol.SupportsReplay() {
				p.onSystemInit()
			}

			// user replay: claim slots into currentTurnSlots. Filter out of
			// EventLog + eventCh so replay events don't pollute the dashboard
			// transcript or trigger legacy result detection.
			if ev.Type == "user" && ev.IsReplay {
				p.slotsMu.Lock()
				p.handleReplayEventLocked(ev)
				p.slotsMu.Unlock()
				continue // skip logEventAt / eventCh below
			}

			// result under passthrough: fan-out to claimed slots and skip
			// legacy eventCh delivery. We still log to EventLog so dashboard
			// sees the turn-complete event.
			if ev.Type == "result" && p.protocol.SupportsReplay() {
				// error_during_execution signals the CLI aborted the turn —
				// e.g. a priority:"now" preempted it. Any older pending slot
				// written before `now` that was never replayed was dropped
				// by the CLI; fire ErrAbortedByUrgent for those.
				if ev.SubType == "error_during_execution" {
					victims := p.reapAbortedPreempted()
					fireAbortErrors(victims)
				}
				owners, _ := p.onTurnResult()
				if len(owners) > 0 {
					p.logEventAt(ev, now.UnixMilli())
					// Fire onEvent for each owner's turn-scope callback
					// before delivering the terminal result.
					for _, owner := range owners {
						if owner.onEvent != nil {
							owner.onEvent(ev)
						}
					}
					fanoutTurnResult(owners, ev)
					continue
				}
				// No owners claim this result. Under passthrough this means
				// either (a) an abort with no claimed slots, handled above,
				// or (b) stray result during reconnect. Either way skip
				// legacy eventCh; dashboard EventLog already has the entry.
				if ev.SubType == "error_during_execution" {
					p.logEventAt(ev, now.UnixMilli())
					continue
				}
				// Fall through to legacy path only for true stray results.
			}

			// Always log to EventLog so dashboard subscribers see events
			// even when no Send() is active (e.g., after service restart
			// reconnects to a shim that's mid-turn).
			p.logEventAt(ev, now.UnixMilli())

			// If a result event arrives while no Send() is active (e.g.,
			// after shim reconnect set state to Running via isMidTurn but
			// the CLI finished before anyone called Send), transition
			// back to Ready so the dashboard doesn't show a stale "running".
			//
			// The transition is gated on reconnectedMidTurn: outside the
			// reconnect path, State=Running means Send() is actively waiting
			// for this result and owns the State→Ready transition via its
			// defer. Racing readLoop into that transition briefly flips the
			// dashboard to "ready" before Send() returns, and — worse — lets a
			// concurrent Send() start immediately after Send() unlocks mu but
			// before its defer runs. The flag is one-shot: consumed on first
			// stray-result here so a genuine next-turn Send() after reconnect
			// is not confused with another stray result.
			if ev.Type == "result" && p.reconnectedMidTurn.CompareAndSwap(true, false) {
				p.mu.Lock()
				wasRunning := p.State == StateRunning
				if wasRunning {
					p.State = StateReady
				}
				cb := p.onTurnDone
				p.mu.Unlock()
				if wasRunning && cb != nil {
					// R183-CONCUR-M1: the killCh select below may fire cb again
					// in the same readLoop iteration if Kill() was racing this
					// stray-result path. See onTurnDone godoc for the idempotency
					// contract that makes this safe.
					cb()
				}
			}

			select {
			case <-p.killCh:
				p.setDeathReason(DeathReasonKilled)
				p.mu.Lock()
				p.State = StateDead
				cb := p.onTurnDone
				p.mu.Unlock()
				if cb != nil {
					cb()
				}
				return
			default:
			}

			// Deliver to Send() for result detection and callback delivery.
			// Non-blocking: if buffer is full (no active Send), the event
			// is already safely in EventLog for dashboard visibility.
			// recvAt is set just before handoff so drainStaleEvents can tell
			// events queued before a new turn started from events produced
			// for the new turn.
			ev.recvAt = now
			select {
			case p.eventCh <- ev:
			default:
			}

		case "stderr":
			log.Debug("cli stderr", "line", sanitizeStderrLine(msg.Line))

		case "cli_exited":
			code := 0
			if msg.Code != nil {
				code = *msg.Code
			}
			log.Info("CLI exited via shim", "code", code)
			reason := DeathReasonCLIExited
			// R180-PERF-P2: string concat + strconv avoids fmt.Sprintf's
			// reflection + scratch-buffer allocation. The death reason is
			// stored in an atomic.Pointer[string] and consumed by health
			// dashboards, so the cold-path savings are trivial but the
			// replacement is zero-risk.
			if code != 0 {
				reason = DeathReasonCLIExited + "_code_" + strconv.Itoa(code)
			} else if msg.Signal != "" {
				// R183-SEC-H1: msg.Signal is the Signal field of the shim's
				// cli_exited JSON frame. Normal shim builds emit canonical
				// signal names ("SIGKILL", "SIGTERM"), but the shim is a
				// separate process: a tampered shim (local attacker, future
				// downgrade attack via stale binary) could ship arbitrary
				// bytes. deathReason flows into slog attrs and the dashboard
				// JSON for "/api/sessions" → HTML. Mirror the SanitizeForLog
				// pattern (R172-SEC-M4 / R175-SEC-P1) used across the
				// codebase; the numeric `code` branch is safe via Itoa.
				reason = DeathReasonCLIExited + "_signal_" + osutil.SanitizeForLog(msg.Signal, 32)
			}
			p.setDeathReason(reason)
			p.mu.Lock()
			p.State = StateDead
			cb := p.onTurnDone
			p.mu.Unlock()
			if cb != nil {
				cb()
			}
			// Passthrough slot cleanup: every pending slot's caller is
			// blocked inside SendPassthrough waiting on resultCh/errCh.
			// Fire ErrProcessExited so they unblock with a clear error.
			p.discardAllPending(ErrProcessExited)
			// Close shim conn so heartbeatLoop stops writing pings into a dead
			// socket and the bufio.Writer's fd is released promptly. Without
			// this, if the process isn't subsequently Kill/Detach'd (e.g. when
			// Router.Cleanup evicts it from the map), the fd leaks to GC.
			// shimConn.Close is idempotent, so a later Kill/Detach is safe.
			_ = p.shimConn.Close()
			return

		case "pong":
			// Signal heartbeat loop that shim is responsive
			select {
			case p.pongRecv <- struct{}{}:
			default:
			}

		case "error":
			// Sanitize shim-supplied message: shim wire is a semi-trusted
			// boundary (degraded/tampered shim could emit arbitrary bytes).
			// Mirrors the R183-SEC-H1 / R184-SEC-M1 policy used for
			// cli_exited.Signal and ACP rpc error messages.
			log.Warn("shim error", "msg", osutil.SanitizeForLog(msg.Line, 256))
		}
	}

	// readLoop fell out of the read loop without hitting cli_exited — the
	// caller-facing reason was already recorded above when the read error was
	// classified (shim EOF / read error / drained). If none of those paths
	// fired, Kill() was what unblocked ReadSlice via shimConn.Close, which
	// surfaces as net.ErrClosed and is already classified as DeathReasonShimEOF.
	p.mu.Lock()
	p.State = StateDead
	cb := p.onTurnDone
	p.mu.Unlock()
	if cb != nil {
		cb()
	}
	// Passthrough: fan-out ErrProcessExited to any still-blocking SendPassthrough callers.
	p.discardAllPending(ErrProcessExited)
}

// heartbeatLoop sends periodic ping messages to the shim and kills the process
// if 3 consecutive pongs are missed (shim unresponsive or connection broken).
func (p *Process) heartbeatLoop() {
	log := p.slogger()
	defer func() {
		if r := recover(); r != nil {
			log.Error("heartbeatLoop panic recovered",
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	const (
		interval  = 30 * time.Second
		maxMisses = 3
	)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	misses := 0
	pongTimer := time.NewTimer(interval / 2)
	pongTimer.Stop()
	defer pongTimer.Stop()
	for {
		select {
		case <-ticker.C:
			if err := p.shimSend(shimClientMsg{Type: "ping"}); err != nil {
				log.Debug("heartbeat ping failed", "err", err)
				p.Kill()
				return
			}

			// Wait for pong within half the interval. Note on drain: Go 1.23+
			// made Timer.Stop/Reset self-draining at the runtime level, so the
			// historical `if !Stop() { <-C }` dance is redundant on this
			// toolchain. We still call Stop() to release the pending tick
			// immediately rather than waiting for GC.
			pongTimer.Reset(interval / 2)
			select {
			case <-p.pongRecv:
				pongTimer.Stop()
				misses = 0
			case <-pongTimer.C:
				misses++
				log.Debug("heartbeat pong missed", "misses", misses)
				if misses >= maxMisses {
					log.Warn("heartbeat: shim unresponsive, killing process", "misses", misses)
					p.Kill()
					return
				}
			case <-p.done:
				pongTimer.Stop()
				return
			}

		case <-p.done:
			return
		}
	}
}

// findResultSince checks EventLog for a result entry logged after afterMS.
// Used as fallback when eventCh may have dropped events due to full buffer.
func (p *Process) findResultSince(afterMS int64) *SendResult {
	entries := p.eventLog.EntriesSince(afterMS)
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type == "result" {
			return &SendResult{
				Text:      entries[i].Detail,
				SessionID: p.GetSessionID(),
				CostUSD:   entries[i].Cost,
			}
		}
	}
	return nil
}

// EventCallback is called for each intermediate event during Send.
type EventCallback func(ev Event)

// buildUserEntry renders the EventLog entry that represents a single user
// message, including per-image thumbnail generation. Shared between Send
// (legacy collect mode) and SendPassthrough so the dashboard sees the same
// bubble regardless of dispatch path — passthrough mode used to skip this
// because the CLI echoes a replay event, but readLoop filters replays out
// of EventLog (see process.go ~755), so without an explicit append the
// user's typed message disappears on the next session re-subscribe.
func buildUserEntry(text string, images []ImageData) EventEntry {
	entry := EventEntry{
		Time:    time.Now().UnixMilli(),
		Type:    "user",
		Summary: TruncateRunes(text, 120),
		Detail:  TruncateRunes(text, 2000),
	}
	if len(images) > 0 {
		entry.Summary += fmt.Sprintf(" [+%d image(s)]", len(images))
		thumbs := make([]string, len(images))
		if len(images) == 1 {
			thumbs[0] = MakeThumbnail(images[0].Data, 600)
		} else {
			var wg sync.WaitGroup
			for i, img := range images {
				wg.Add(1)
				go func(i int, data []byte) {
					defer wg.Done()
					thumbs[i] = MakeThumbnail(data, 600)
				}(i, img.Data)
			}
			wg.Wait()
		}
		// ImagePaths rides alongside Images so the dashboard can offer
		// "view original" without bloating the eventlog with full-size
		// base64. sanitizeImages can drop entries (invalid/empty data
		// URI), so build ImagePaths from the same index set that
		// survives that filter: walk pre-sanitize, emit (thumb, path)
		// pairs only when the thumb is valid. Preserves the
		// index-alignment contract documented on EventEntry.ImagePaths.
		sanitizedThumbs := make([]string, 0, len(thumbs))
		sanitizedPaths := make([]string, 0, len(images))
		anyPath := false
		for i, t := range thumbs {
			if t == "" || !strings.HasPrefix(t, imageDataURIPrefix) {
				continue
			}
			sanitizedThumbs = append(sanitizedThumbs, t)
			p := ""
			if i < len(images) {
				p = images[i].WorkspacePath
			}
			sanitizedPaths = append(sanitizedPaths, p)
			if p != "" {
				anyPath = true
			}
		}
		if len(sanitizedThumbs) > 0 {
			entry.Images = sanitizedThumbs
		}
		if anyPath {
			entry.ImagePaths = sanitizedPaths
		}
	}
	return entry
}

// Send writes a user message to stdin and reads events until result.
func (p *Process) Send(ctx context.Context, text string, images []ImageData, onEvent EventCallback) (*SendResult, error) {
	p.mu.Lock()
	if p.State == StateRunning {
		p.mu.Unlock()
		return nil, fmt.Errorf("process busy (state=%s)", p.State)
	}
	p.State = StateRunning
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		if p.State == StateRunning {
			p.State = StateReady
		}
		p.mu.Unlock()
	}()

	// Log user message before sending
	p.eventLog.Append(buildUserEntry(text, images))

	// Drain stale events from a previous turn that completed while no Send()
	// was active (e.g., CLI was mid-turn when service restarted and reconnected
	// to shim). These events are already logged to EventLog by readLoop.
	//
	// When the previous turn was interrupted (SIGINT), the CLI may still be
	// producing the interrupted result. Wait briefly for it so it doesn't
	// pollute this turn's event stream.
	if err := p.drainStaleEvents(ctx); err != nil {
		return nil, err
	}

	// Record turn start time so we can check EventLog as fallback if eventCh
	// drops events (non-blocking send when buffer is full).
	turnStartMS := time.Now().UnixMilli()

	if err := p.protocol.WriteMessage(p.shimStdinWriter(), text, images); err != nil {
		return nil, fmt.Errorf("write message: %w", err)
	}

	noOutputDur := p.noOutputTimeout
	if noOutputDur <= 0 {
		noOutputDur = DefaultNoOutputTimeout
	}
	totalDur := p.totalTimeout
	if totalDur <= 0 {
		totalDur = DefaultTotalTimeout
	}

	// Watchdog via a single periodic ticker instead of per-event timer
	// Stop/drain/Reset (three timer-heap ops per event). The ticker interval
	// caps timeout precision, but timeouts are minutes so this is acceptable.
	checkInterval := noOutputDur / 4
	if checkInterval < time.Second {
		checkInterval = time.Second
	}
	if checkInterval > 30*time.Second {
		checkInterval = 30 * time.Second
	}
	turnStart := time.Now()
	lastOutput := turnStart
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Context cancelled (shutdown or user interrupt).
			// Don't Kill the CLI — during graceful shutdown, router.Shutdown
			// calls Detach() to keep the shim alive for zero-downtime restart.
			// The readLoop will detect the disconnection and close eventCh,
			// causing the next iteration to hit the !ok branch and return.
			return nil, ctx.Err()
		case ev, ok := <-p.eventCh:
			if !ok {
				// eventCh closed — process exited. Check EventLog for a result
				// that readLoop already logged but wasn't delivered via eventCh
				// (e.g., non-blocking send dropped it, or it arrived just before
				// the channel closed).
				if sr := p.findResultSince(turnStartMS); sr != nil {
					return sr, nil
				}
				return nil, ErrProcessExited
			}

			lastOutput = time.Now()

			// Capture session ID from first init event.
			// logEvent (called by readLoop) already skips init events.
			if ev.Type == "system" && ev.SubType == "init" {
				p.mu.Lock()
				if p.SessionID == "" {
					p.SessionID = ev.SessionID
				}
				p.mu.Unlock()
				continue
			}

			// Event is already logged to EventLog by readLoop.

			// Deliver intermediate events via callback
			if onEvent != nil && ev.Type == "assistant" && ev.Message != nil {
				for _, block := range ev.Message.Content {
					if block.Type == "thinking" || block.Type == "tool_use" {
						onEvent(ev)
						break
					}
				}
			}

			// Result means this turn is done
			if ev.Type == "result" {
				p.mu.Lock()
				if p.SessionID == "" {
					p.SessionID = ev.SessionID
				}
				p.mu.Unlock()
				return &SendResult{
					Text:      ev.Result,
					SessionID: ev.SessionID,
					CostUSD:   ev.CostUSD,
				}, nil
			}
		case <-ticker.C:
			now := time.Now()
			if now.Sub(lastOutput) >= noOutputDur {
				if sr := p.findResultSince(turnStartMS); sr != nil {
					return sr, nil
				}
				// Set death reason BEFORE Kill so readLoop's shim_eof/
				// shim_read_error classification (triggered by shimConn.Close)
				// cannot overwrite the true root cause. setDeathReason is
				// first-writer-wins, so the earlier set wins the CAS.
				p.setDeathReason(DeathReasonNoOutputTimeout)
				p.slogger().Error("watchdog: no output timeout", "timeout", noOutputDur)
				p.Kill()
				return nil, fmt.Errorf("%w (%s)", ErrNoOutputTimeout, noOutputDur)
			}
			if now.Sub(turnStart) >= totalDur {
				if sr := p.findResultSince(turnStartMS); sr != nil {
					return sr, nil
				}
				p.setDeathReason(DeathReasonTotalTimeout)
				p.slogger().Error("watchdog: total timeout", "timeout", totalDur)
				p.Kill()
				return nil, fmt.Errorf("%w (%s)", ErrTotalTimeout, totalDur)
			}
		}
	}
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
	return p.State == StateRunning
}

// Interrupt sends SIGINT to the CLI process via shim.
func (p *Process) Interrupt() {
	if !p.Alive() {
		return
	}
	// Set the atomics while holding p.mu so Send()'s State→Running transition
	// (also under p.mu) serialises with us. Without the lock coverage, a
	// concurrent Send() could flip State to Running between our unlock and
	// our atomics Store, leaving interrupted=true with interruptedRun=false —
	// drainStaleEvents would then skip the settle wait and the interrupted
	// result event from the in-flight turn would leak into the next turn.
	p.mu.Lock()
	state := p.State
	p.interrupted.Store(true)
	if state == StateRunning {
		p.interruptedRun.Store(true)
	}
	p.mu.Unlock()
	// While the CLI is still spawning, its REPL hasn't initialised and the
	// Claude CLI silently drops SIGINT. Skip the wire send entirely; also
	// avoid marking interruptedRun so drainStaleEvents will not enter the
	// settle loop (interrupted=true alone drains without waiting, since
	// there is no stale result to absorb).
	if state == StateSpawning {
		return
	}
	if err := p.shimSend(shimClientMsg{Type: "interrupt"}); err != nil {
		slog.Warn("interrupt failed", "err", err)
	}
}

// InterruptViaControl requests the CLI to abort the active turn by writing an
// in-band control_request to stdin (stream-json protocol only). Verified
// behaviour on CLI 2.1.119: within ~300ms the CLI kills any in-flight tool
// invocation (bash processes receive SIGKILL), emits a `result` event with
// stop_reason=tool_use (or end_turn for pure-generation turns), and the
// session remains usable for the next user message on the same process.
//
// Unlike Interrupt(), this path:
//   - Does not send SIGINT to the CLI (no signal handler dependency).
//   - Does not cross the shim's interrupt command (uses plain `write`).
//   - Is officially supported by the Claude CLI stream-json protocol.
//
// Return values:
//   - nil: control_request was written; the next Send() will drain the
//     interrupted result via the settle loop.
//   - ErrNoActiveTurn: process is alive but no turn is in flight; nothing
//     was written, no flags were set. Callers should not log success.
//   - ErrInterruptUnsupported: protocol (e.g. ACP) has no stdin-level
//     interrupt primitive; callers should fall back to Interrupt().
//   - wrapped transport error: the write failed; flags are rolled back so
//     a subsequent Send() does not burn the settle budget waiting for a
//     result that will never come.
func (p *Process) InterruptViaControl() error {
	if !p.Alive() {
		return ErrNoActiveTurn
	}
	// Snapshot state and pre-commit the atomics under p.mu so a concurrent
	// Send() flipping State to Running after our read cannot race us into
	// "wrote control_request but skipped the settle flags".
	p.mu.Lock()
	state := p.State
	if state == StateRunning {
		p.interrupted.Store(true)
		p.interruptedRun.Store(true)
	}
	p.mu.Unlock()
	// No turn in flight → nothing to interrupt. Do NOT write the
	// control_request: the CLI would buffer it for the next turn start and
	// produce a spurious control_response against a turn the caller never
	// intended to cancel.
	if state != StateRunning {
		return ErrNoActiveTurn
	}
	// R179-PERF-P3: direct concat + strconv avoids fmt.Sprintf's reflection
	// + scratch buffer. reqID is only used for local control-response echo
	// matching, so format quality doesn't matter.
	reqID := "naozhi-int-" + strconv.FormatInt(p.interruptSeq.Add(1), 10)
	if err := p.protocol.WriteInterrupt(p.shimStdinWriter(), reqID); err != nil {
		// Write failed: no control_request reached the CLI, so there is no
		// trailing result event to drain. Roll the settle flags back
		// explicitly; leaving them set would cost every subsequent Send()
		// a 500ms settle timeout until the process is recycled.
		//
		// Safe against a concurrent real Interrupt() that set the flags
		// between our Store above and this rollback: in that case we'd
		// momentarily underreport, but Interrupt() also writes via shim
		// `interrupt` (SIGINT), and if THAT write succeeded its own
		// semantics apply on the next Send. Mis-clearing here is no worse
		// than the SIGINT path itself failing — both converge on the same
		// "no stale result to drain" state.
		p.interrupted.Store(false)
		p.interruptedRun.Store(false)
		return fmt.Errorf("write interrupt control_request: %w", err)
	}
	return nil
}

// drainStaleEvents clears residual events from previous turns.
// When the previous turn was interrupted (SIGINT), waits briefly for the
// interrupted result event so it doesn't pollute the next turn.
//
// Only drains events whose arrival predates this call. Using a cutoff
// timestamp captured at entry avoids a race where readLoop concurrently
// pushes a fresh event for the *new* turn into eventCh between the caller's
// Send() and this drain; without the guard, that live event would be
// swallowed and the Send would fall back to findResultSince.
func (p *Process) drainStaleEvents(ctx context.Context) error {
	cutoff := time.Now()
	// Read-and-clear interrupted/interruptedRun atomically w.r.t. Interrupt()
	// / InterruptViaControl(), which hold p.mu while Store-ing both flags. A
	// naïve two-call Swap(false) here opened a window where a concurrent
	// Interrupt between the two Swaps could Store interruptedRun=true after
	// we Swap'd interrupted=false — the new Interrupt's intent would be lost
	// (interruptedRun later Swap'd to false here, but interrupted already
	// consumed, so the next Send's drainStaleEvents would see interrupted=
	// false/interruptedRun=false and skip the settle window entirely — the
	// SIGINT-produced result event leaks into the next turn). R39-CONCUR1.
	p.mu.Lock()
	wasInterrupted := p.interrupted.Swap(false)
	wasRunning := p.interruptedRun.Swap(false)
	p.mu.Unlock()
	if wasInterrupted {
		// Only wait for the interrupted result if the CLI was actively
		// processing a turn when Interrupt() was called. An idle process
		// won't produce a result event, so the settle timer would always
		// expire causing an unnecessary 500ms delay.
		if wasRunning {
			slog.Debug("send: draining interrupted turn result")
			settle := time.NewTimer(500 * time.Millisecond)
			defer settle.Stop()
			for {
				select {
				case ev, ok := <-p.eventCh:
					if !ok || ev.Type == "result" {
						goto drain
					}
					if ev.recvAt.After(cutoff) {
						// Event produced after we entered drain belongs to the
						// new turn. Try to put it back (buffered channel may
						// have room); if the channel is already full we fall
						// back to findResultSince which reads from EventLog.
						select {
						case p.eventCh <- ev:
						default:
						}
						goto drain
					}
				case <-settle.C:
					slog.Debug("send: settle timeout, no stale result")
					goto drain
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		} else {
			slog.Debug("send: interrupted but idle, skipping settle wait")
		}
	}
drain:
	// Non-blocking drain of any remaining buffered events that predate the
	// cutoff. Events produced after cutoff are collected and re-enqueued at
	// the end so the live consumer still observes them. Returning the moment
	// we hit one post-cutoff event would leave any interleaved pre-cutoff
	// stragglers in the channel where they would be consumed by the new
	// turn as if they were current — producing phantom tool_use/assistant
	// events from the prior turn.
	//
	// Backing storage is a stack-allocated [4]Event array — post-cutoff events
	// during an interrupt are rare (typically 0-1, occasionally 2-3 from
	// in-flight stream-json blocks). `holdback := holdbackArr[:0]` starts with
	// cap=4, so the common post-interrupt shape appends without heap allocation;
	// append promotes to the heap only when >4 post-cutoff events stack up,
	// which has never been observed in practice. R64-PERF-M7.
	var holdbackArr [4]Event
	holdback := holdbackArr[:0]
	for {
		select {
		case <-ctx.Done():
			// Re-enqueue anything we have already collected so we do not
			// drop the fresh-turn events on cancellation. Guard against the
			// readLoop having closed eventCh concurrently: sending on a
			// closed channel panics regardless of the `default` arm in a
			// select, because the send case is always ready-to-run on a
			// closed channel and select will pick it. EventLog is the
			// authoritative store for logged events, so dropping holdback
			// when eventCh is torn down is safe.
			if isChanAlive(p.done) {
				for _, ev := range holdback {
					select {
					case p.eventCh <- ev:
					default:
					}
				}
			}
			return ctx.Err()
		case ev, ok := <-p.eventCh:
			if !ok {
				// Channel closed (process exited). Any post-cutoff events
				// already in holdback were also logged to EventLog by readLoop
				// before being pushed to eventCh (see logEvent call above), so
				// the live Send() can recover a result via findResultSince().
				// Dropping holdback here is safe because EventLog is authoritative.
				return nil
			}
			if ev.recvAt.After(cutoff) {
				holdback = append(holdback, ev)
			}
			// pre-cutoff events are dropped (drained)
		default:
			// Channel empty — push back any collected post-cutoff events.
			// Same readLoop-closed guard as the ctx.Done arm above.
			if !isChanAlive(p.done) {
				return nil
			}
			for _, ev := range holdback {
				select {
				case p.eventCh <- ev:
				default:
					// eventCh is full; fresh events are being dropped here.
					// findResultSince will recover the result from EventLog but
					// surface the occurrence so operators can enlarge the
					// channel if it persists under load.
					slog.Warn("drainStaleEvents: eventCh full, dropped fresh event",
						"type", ev.Type, "session", ev.SessionID)
				}
			}
			return nil
		}
	}
}

// isChanAlive reports whether done is still open (readLoop still running, so
// eventCh remains safe to send on). readLoop's defers are registered in this
// declaration order: close(eventCh), close(done), CloseSubscribers, recover.
// LIFO execution order is therefore: recover → CloseSubscribers → close(done)
// → close(eventCh). Invariant used here: `done` closes strictly BEFORE
// `eventCh`, so if `done` is still open, `eventCh` is also still open.
func isChanAlive(done <-chan struct{}) bool {
	select {
	case <-done:
		return false
	default:
		return true
	}
}

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
			// Discover's F4 stat-check within 30s.
			if err := syscall.Kill(p.shimPID, syscall.SIGUSR2); err != nil {
				slog.Debug("kill: SIGUSR2 to shim failed (likely already exited)",
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
		// Lazy-allocate out: most assistant events carry a single content block
		// (pure text or a single tool_use). `make([]EventEntry, 0, 1)` would
		// still force a heap alloc for the backing array; `nil` lets the
		// first append pay 1 alloc only when we have real blocks to write.
		var out []EventEntry
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
		p.mu.Lock()
		p.totalCost = ev.CostUSD
		p.mu.Unlock()
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

func getStr(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok || len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
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
			for _, v := range []string{inp.Description, inp.FilePath, inp.Path, inp.Command, inp.Pattern, inp.Prompt} {
				if v != "" {
					return toolName + " " + TruncateRunes(v, 80)
				}
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

// TotalCost returns the cumulative cost.
func (p *Process) TotalCost() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.totalCost
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

// InjectHistory pre-populates the event log with historical entries.
func (p *Process) InjectHistory(entries []EventEntry) {
	p.eventLog.AppendBatch(entries)
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

const maxStderrLogLineBytes = 500

// sanitizeStderrLine removes ANSI escape sequences (SGR color, cursor movement,
// OSC/DCS) and truncates the stderr line so that terminal-aware log viewers
// aren't colorized/repositioned by whatever the Claude CLI wrote, and so a
// runaway stderr cannot fill the journal with a single multi-MB line.
func sanitizeStderrLine(line string) string {
	if line == "" {
		return line
	}
	// Pre-truncate before the ANSI scanner so a pathological single-line
	// OSC sequence (ESC ] ... no BEL/ST for MBs) doesn't force a full-length
	// strings.Builder allocation just to be truncated afterward. The shim
	// caps stdin lines at 12 MB; without this, a crafted line would allocate
	// the full builder before truncation.
	if len(line) > maxStderrLogLineBytes {
		cut := maxStderrLogLineBytes
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut] + "…(truncated)"
	}
	// Fast path: most CLI stderr output is plain log text with neither ANSI
	// escape sequences nor stray control bytes. Scanning once cheaply and
	// returning the original string avoids a strings.Builder allocation and
	// a full-line copy on the common path.
	//
	// R190-SEC-L1: ASCII-only fast path. If the line contains any non-ASCII
	// byte, bail to the slow path so the terminating rune-map can drop
	// C1/bidi/LS/PS codepoints (>= 0x20 at the byte level, >=0xC0 as UTF-8
	// leading bytes). A compromised claude CLI emitting bidi overrides in
	// stderr could otherwise reverse operator journalctl output verbatim.
	clean := true
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == 0x1b || (c < 0x20 && c != '\t') || c >= 0x80 {
			clean = false
			break
		}
	}
	if clean {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); {
		c := line[i]
		if c == 0x1b { // ESC
			// CSI: ESC [ ... final byte in @ .. ~
			if i+1 < len(line) && line[i+1] == '[' {
				j := i + 2
				for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
					j++
				}
				if j < len(line) {
					j++ // consume final byte
				}
				i = j
				continue
			}
			// OSC: ESC ] ... (ST = ESC \ or BEL)
			if i+1 < len(line) && line[i+1] == ']' {
				j := i + 2
				for j < len(line) {
					if line[j] == 0x07 { // BEL
						j++
						break
					}
					if line[j] == 0x1b && j+1 < len(line) && line[j+1] == '\\' {
						j += 2
						break
					}
					j++
				}
				i = j
				continue
			}
			// Two-byte ESC sequence.
			if i+1 < len(line) {
				i += 2
			} else {
				i++
			}
			continue
		}
		// Drop bare control chars (keep \t).
		if c < 0x20 && c != '\t' {
			i++
			continue
		}
		b.WriteByte(c)
		i++
	}
	// The pre-truncation step above already capped the input length; the
	// sanitizer only removes bytes from that capped input (ANSI escapes +
	// control chars), so the resulting builder is guaranteed to be no longer
	// than the pre-truncated input. No post-sanitize truncation needed.
	//
	// R190-SEC-L1: byte-level loop above drops ASCII C0 (< 0x20) but passes
	// C1 controls (U+0080–U+009F), bidi overrides/isolates, LS/PS (>=0x20
	// when decoded). Run one more pass with strings.Map at the rune level to
	// match the policy SanitizeQuote / SanitizeForLog enforce elsewhere.
	return strings.Map(func(r rune) rune {
		if osutil.IsLogInjectionRune(r) {
			return -1
		}
		return r
	}, b.String())
}
