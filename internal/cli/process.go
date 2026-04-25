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
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
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

// ErrNoActiveTurn is returned by InterruptViaControl when the process is not
// currently running a turn (StateSpawning, StateReady, or StateDead). The
// caller didn't do anything wrong, but nothing was interrupted; logs should
// not claim "aborted active turn" in this case.
var ErrNoActiveTurn = errors.New("no active turn to interrupt")

// processCloseTimeout is a var (not const) so tests can override it.
var processCloseTimeout = 5 * time.Second

func (s ProcessState) String() string {
	switch s {
	case StateSpawning:
		return "running" // spawning is transient; visible as running
	case StateReady:
		return "ready"
	case StateRunning:
		return "running"
	case StateDead:
		return "ready" // process exited; session may be resumable
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

	SessionID string
	State     ProcessState
	mu        sync.Mutex

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
	onTurnDone func()
}

// newShimProcess creates a Process connected to a shim.
// The caller must call startReadLoop() after protocol Init.
func newShimProcess(conn net.Conn, reader *bufio.Reader, writer *bufio.Writer,
	proto Protocol, cliPID int, noOutputTimeout, totalTimeout time.Duration) *Process {
	p := &Process{
		shimConn:        conn,
		shimR:           reader,
		shimW:           writer,
		protocol:        proto,
		cliPID:          cliPID,
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
	// Panic recover: a malformed shim message or protocol bug must not take
	// the whole process down silently. We log stack + transition to Dead so
	// the router can reap this session and the dashboard surfaces the failure
	// instead of the user seeing a stalled "running" forever.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("readLoop panic recovered",
				"panic", r, "stack", string(debug.Stack()))
			p.mu.Lock()
			p.State = StateDead
			p.mu.Unlock()
		}
	}()
	defer close(p.eventCh)
	defer close(p.done)
	defer p.eventLog.CloseSubscribers()

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
			if err == bufio.ErrBufferFull {
				continue // keep reading until newline or cap
			}
			readErr = err
			break
		}
		// Propagate grown capacity so the next iteration starts with the
		// expanded backing array instead of reverting to the original 4096.
		// Without this, a single large event forces every subsequent
		// iteration to re-grow from 4KB through a chain of doublings.
		lineBuf = line
		if capExceeded {
			slog.Warn("readLoop: oversized shim message, skipping", "size", len(line))
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
				if err != bufio.ErrBufferFull {
					readErr = err
					break
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) || errors.Is(readErr, net.ErrClosed) {
					slog.Info("readLoop: shim connection closed after oversize drain")
				} else {
					slog.Warn("readLoop: shim read error after oversize drain", "err", readErr)
				}
				break
			}
			continue
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, net.ErrClosed) {
				slog.Info("readLoop: shim connection closed")
			} else {
				slog.Warn("readLoop: shim read error", "err", readErr)
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
			slog.Warn("readLoop: skip unparseable shim message", "err", err, "size", len(line))
			continue
		}

		switch msg.Type {
		case "stdout":
			p.lastSeq.Store(msg.Seq)
			ev, _, err := p.protocol.ReadEvent(msg.Line)
			if err != nil {
				slog.Warn("readLoop: skip unparseable event", "err", err, "seq", msg.Seq)
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

			// Always log to EventLog so dashboard subscribers see events
			// even when no Send() is active (e.g., after service restart
			// reconnects to a shim that's mid-turn).
			p.logEventAt(ev, now.UnixMilli())

			// If a result event arrives while no Send() is active (e.g.,
			// after shim reconnect set state to Running via isMidTurn but
			// the CLI finished before anyone called Send), transition
			// back to Ready so the dashboard doesn't show a stale "running".
			if ev.Type == "result" {
				p.mu.Lock()
				wasRunning := p.State == StateRunning
				if wasRunning {
					p.State = StateReady
				}
				cb := p.onTurnDone
				p.mu.Unlock()
				if wasRunning && cb != nil {
					cb()
				}
			}

			select {
			case <-p.killCh:
				p.mu.Lock()
				p.State = StateDead
				p.mu.Unlock()
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
			slog.Debug("cli stderr", "line", sanitizeStderrLine(msg.Line))

		case "cli_exited":
			code := 0
			if msg.Code != nil {
				code = *msg.Code
			}
			slog.Info("CLI exited via shim", "code", code)
			p.mu.Lock()
			p.State = StateDead
			p.mu.Unlock()
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
			slog.Warn("shim error", "msg", msg.Line)
		}
	}

	p.mu.Lock()
	p.State = StateDead
	p.mu.Unlock()
}

// heartbeatLoop sends periodic ping messages to the shim and kills the process
// if 3 consecutive pongs are missed (shim unresponsive or connection broken).
func (p *Process) heartbeatLoop() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("heartbeatLoop panic recovered",
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
				slog.Debug("heartbeat ping failed", "err", err)
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
				slog.Debug("heartbeat pong missed", "misses", misses)
				if misses >= maxMisses {
					slog.Warn("heartbeat: shim unresponsive, killing process", "misses", misses)
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
	userEntry := EventEntry{
		Time:    time.Now().UnixMilli(),
		Type:    "user",
		Summary: TruncateRunes(text, 120),
		Detail:  TruncateRunes(text, 2000),
	}
	if len(images) > 0 {
		userEntry.Summary += fmt.Sprintf(" [+%d image(s)]", len(images))
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
		filtered := thumbs[:0]
		for _, t := range thumbs {
			if t != "" {
				filtered = append(filtered, t)
			}
		}
		userEntry.Images = filtered
	}
	p.eventLog.Append(userEntry)

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
				slog.Error("watchdog: no output timeout", "timeout", noOutputDur)
				p.Kill()
				return nil, fmt.Errorf("%w (%s)", ErrNoOutputTimeout, noOutputDur)
			}
			if now.Sub(turnStart) >= totalDur {
				if sr := p.findResultSince(turnStartMS); sr != nil {
					return sr, nil
				}
				slog.Error("watchdog: total timeout", "timeout", totalDur)
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
	p.mu.Lock()
	defer p.mu.Unlock()
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
	reqID := fmt.Sprintf("naozhi-int-%d", p.interruptSeq.Add(1))
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
	if p.interrupted.Swap(false) {
		// Only wait for the interrupted result if the CLI was actively
		// processing a turn when Interrupt() was called. An idle process
		// won't produce a result event, so the settle timer would always
		// expire causing an unnecessary 500ms delay.
		if p.interruptedRun.Swap(false) {
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
// eventCh remains safe to send on). readLoop defers `close(p.done)` followed
// by `close(p.eventCh)` in LIFO order — if p.done is open, eventCh is too.
func isChanAlive(done <-chan struct{}) bool {
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// Kill forcefully terminates the CLI process via shim.
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
		defer p.shimWMu.Unlock()
		p.shimConn.SetWriteDeadline(time.Now().Add(time.Second)) //nolint:errcheck
		_ = p.shimSendLocked(shimClientMsg{Type: "kill"})
		p.shimConn.Close()
	})
}

// Close gracefully shuts down by closing CLI stdin via shim.
func (p *Process) Close() {
	_ = p.shimSend(shimClientMsg{Type: "close_stdin"})
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
	p.shimConn.SetWriteDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	_ = p.shimSendLocked(shimClientMsg{Type: "detach"})
	p.shimConn.Close()
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

// logEvent converts an Event to one or more EventEntry values and appends them to the event log.
func (p *Process) logEvent(ev Event) {
	p.logEventAt(ev, time.Now().UnixMilli())
}

// logEventAt is the caller-supplied-now variant used by readLoop to reuse
// the same time.Now() value that stamps ev.recvAt. R67-PERF-9.
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
		slog.Debug("parseAgentInput: unmarshal failed", "err", err)
	}
	return inp
}

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
			return toolName + " " + s.Pattern
		}
	case "Grep":
		var s struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if json.Unmarshal(input, &s) == nil && s.Pattern != "" {
			result := toolName + " " + s.Pattern
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
		// Fallback: try common keys with a map (rare path for unknown tools)
		var inp map[string]json.RawMessage
		if json.Unmarshal(input, &inp) == nil {
			for _, key := range []string{"description", "file_path", "path", "command", "pattern", "prompt"} {
				if v := getStr(inp, key); v != "" {
					return toolName + " " + TruncateRunes(v, 80)
				}
			}
		}
	}

	return toolName + ": " + TruncateRunes(string(input), 300)
}

// GetState returns the current process state.
func (p *Process) GetState() ProcessState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.State
}

// SetOnTurnDone sets the callback invoked by readLoop when a result event
// transitions the process from Running to Ready without an active Send().
// Thread-safe: may be called while readLoop is running.
func (p *Process) SetOnTurnDone(fn func()) {
	p.mu.Lock()
	p.onTurnDone = fn
	p.mu.Unlock()
}

// GetSessionID returns the session ID in a thread-safe manner.
func (p *Process) GetSessionID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.SessionID
}

// TotalCost returns the cumulative cost.
func (p *Process) TotalCost() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
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
	// Fast path: most CLI stderr output is plain log text with neither ANSI
	// escape sequences nor stray control bytes. Scanning once cheaply and
	// returning the original string avoids a strings.Builder allocation and
	// a full-line copy on the common path.
	clean := true
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == 0x1b || (c < 0x20 && c != '\t') {
			clean = false
			break
		}
	}
	if clean {
		if len(line) > maxStderrLogLineBytes {
			cut := maxStderrLogLineBytes
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			return line[:cut] + "…(truncated)"
		}
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
	out := b.String()
	if len(out) > maxStderrLogLineBytes {
		// Byte-level truncation would cut CJK/emoji runes mid-sequence and
		// produce invalid UTF-8 that some slog handlers (JSON) replace with
		// U+FFFD or reject outright. Walk backwards to the nearest rune start.
		cut := maxStderrLogLineBytes
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + "…(truncated)"
	}
	return out
}
