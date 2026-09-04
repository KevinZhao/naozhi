package cli

// process_readloop.go — inbound shim socket read goroutine and heartbeat.
// Owns readLoop, heartbeatLoop and shimMsg (the inbound wire frame, also
// consumed by wrapper.go's Spawn handshake). maxScannerBufBytes /
// lineBufShrinkThreshold live in process.go's const block with the other
// timing-budget knobs.

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/textutil"
)

// shimMsg is a minimal struct for parsing shim protocol messages in readLoop.
// Code is a (int64, bool) pair rather than *int so a cli_exited frame does not
// heap-allocate; Present=false when the "code" key is absent.
type shimMsg struct {
	Type   string      `json:"type"`
	Seq    int64       `json:"seq,omitempty"`
	Line   string      `json:"line,omitempty"`
	Msg    string      `json:"msg,omitempty"`
	Code   shimMsgCode `json:"code,omitempty"`
	Signal string      `json:"signal,omitempty"`
}

// shimMsgCode wraps an int64 so json.Unmarshal can distinguish absent from
// explicit zero without allocating *int. Decode-only. int64 (not int) so a
// large exit code does not wrap on a 32-bit build.
type shimMsgCode struct {
	Value   int64
	Present bool
}

// UnmarshalJSON implements json.Unmarshaler; json.Unmarshal calls it only when
// the object contains a "code" key, so an absent key leaves Present=false.
func (c *shimMsgCode) UnmarshalJSON(data []byte) error {
	c.Present = true
	// Hand-parse the raw integer token from []byte: string(data) would allocate
	// on every cli_exited frame on the readLoop hot path. Negative exit codes
	// (signal kill = -1) are accepted.
	v, err := parseJSONInt64Bytes(data)
	if err != nil {
		return err
	}
	c.Value = v
	return nil
}

// parseJSONInt64Bytes parses a JSON integer token directly from a []byte
// slice without allocating a string. Rules match JSON integer semantics:
//   - optional leading '-' for negatives
//   - one or more ASCII decimal digits
//   - no leading '+', no leading zeros (except bare "0")
//   - empty input and non-digit characters are rejected
//   - overflow beyond int64 range returns an error
func parseJSONInt64Bytes(data []byte) (int64, error) {
	if len(data) == 0 {
		return 0, errJSONIntEmpty
	}
	neg := false
	i := 0
	if data[0] == '-' {
		neg = true
		i++
		if i >= len(data) {
			return 0, errJSONIntInvalid
		}
	}
	// Reject leading '+'.
	if data[i] == '+' {
		return 0, errJSONIntInvalid
	}
	// Reject leading zero on multi-digit numbers (e.g. "01").
	if data[i] == '0' && len(data)-i > 1 {
		return 0, errJSONIntInvalid
	}
	var v uint64
	for ; i < len(data); i++ {
		b := data[i]
		if b < '0' || b > '9' {
			return 0, errJSONIntInvalid
		}
		digit := uint64(b - '0')
		// Check overflow: uint64 max is 18446744073709551615.
		if v > (maxUint64-digit)/10 {
			return 0, errJSONIntOverflow
		}
		v = v*10 + digit
	}
	if neg {
		// int64 min magnitude is 9223372036854775808.
		if v > 1<<63 {
			return 0, errJSONIntOverflow
		}
		return -int64(v), nil
	}
	if v > 1<<63-1 {
		return 0, errJSONIntOverflow
	}
	return int64(v), nil
}

const maxUint64 = ^uint64(0)

var (
	errJSONIntEmpty    = shimIntParseError("empty JSON integer token")
	errJSONIntInvalid  = shimIntParseError("invalid JSON integer token")
	errJSONIntOverflow = shimIntParseError("JSON integer overflows int64")
)

// shimIntParseError is a plain string error type so these sentinel values
// are package-level constants with no heap allocation.
type shimIntParseError string

func (e shimIntParseError) Error() string { return string(e) }

// readLoop reads NDJSON messages from the shim socket and dispatches events.
func (p *Process) readLoop() {
	log := p.slogger()
	// Defers run LIFO: recover (state→Dead, onTurnDone) → CloseSubscribers →
	// close(done) → close(eventCh). isChanAlive relies on done closing BEFORE
	// eventCh so a producer guarded by "is done open?" never sends on a closed
	// eventCh (see drainStaleEvents / isChanAlive in process_turn.go). If you
	// reorder these defers, re-verify that invariant.
	defer close(p.eventCh)
	defer close(p.done)
	defer p.eventLog.CloseSubscribers()
	// Panic recover: a malformed shim message must not stall the session as
	// "running" forever — log the stack and transition to Dead so the router
	// reaps it. This fires BEFORE the deferred close(p.done), so onTurnDone
	// callbacks observe p.done still open while State==Dead; they must use
	// IsRunning / State, never p.done, as the "torn down" signal.
	defer func() {
		if r := recover(); r != nil {
			log.Error("readLoop panic recovered",
				"panic", r, "stack", string(debug.Stack()))
			p.setDeathReason(DeathReasonReadLoopPanic)
			p.mu.Lock()
			p.state = StateDead
			cb := p.onTurnDone
			p.mu.Unlock()
			if cb != nil {
				cb()
			}
			// Unblock SendPassthrough callers parked on slot.resultCh/errCh;
			// they don't consume eventCh, so the deferred close(eventCh) alone
			// would leave them blocked until the totalTimeout+30s tripwire.
			// discardAllPending is idempotent.
			p.discardAllPending(ErrProcessExited)
		}
	}()

	// Reuse the line accumulator across iterations; 4096 matches bufio's
	// default buffer so single-chunk lines rarely grow. Grown capacity is
	// carried forward via lineBuf = line below.
	lineBuf := make([]byte, 0, 4096)
	for {
		line, capExceeded, readErr := readShimLine(p.shimR, lineBuf)
		// Carry grown capacity forward so one large event doesn't force every
		// later iteration to re-grow from 4 KiB — but shrink back to a fresh
		// buffer on capExceeded (don't pin a ~16 MiB array forever) or when a
		// legitimate event pushed cap past lineBufShrinkThreshold. Single-step
		// assignment avoids pinning a transient second reference to `line`.
		if capExceeded || cap(line) > lineBufShrinkThreshold {
			lineBuf = make([]byte, 0, 4096)
		} else {
			lineBuf = line
		}
		if capExceeded {
			log.Warn("readLoop: oversized shim message, skipping", "size", len(line))
			if readErr != nil {
				p.classifyEOF(readErr, true, log)
				break
			}
			continue
		}
		if readErr != nil {
			p.classifyEOF(readErr, false, log)
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

		if p.handleShimMessage(msg, log) == shimDispatchReturn {
			return
		}
	}

	// Fell out of the loop without cli_exited: the death reason was already
	// stamped by classifyEOF (Kill() surfaces as net.ErrClosed → ShimEOF).
	p.transitionToDead()
}

// shimDispatchOutcome encodes the readLoop control transition produced by
// handleShimMessage. shimDispatchContinue is the zero value so the default
// path is cheapest; shimDispatchReturn means the outer loop must unwind
// (cli_exited terminal frame or a stdout dispatch observed killCh).
type shimDispatchOutcome int

const (
	shimDispatchContinue shimDispatchOutcome = iota
	shimDispatchReturn
)

// classifyEOF stamps the deathReason for a shim-socket read error and emits one
// log line. afterDrain flags the post-oversize-drain branch: a closure preceded
// by a >maxScannerBufBytes frame is stamped ShimOversizeThenEOF/Err so health
// dashboards can separate a clean shim shutdown from a degraded shim (#1288).
// No return value: any non-nil readErr breaks the loop regardless.
func (p *Process) classifyEOF(readErr error, afterDrain bool, log *slog.Logger) {
	closed := errors.Is(readErr, io.EOF) || errors.Is(readErr, net.ErrClosed)
	if closed {
		if afterDrain {
			log.Info("readLoop: shim connection closed after oversize drain")
			p.setDeathReason(DeathReasonShimOversizeThenEOF)
			return
		}
		log.Info("readLoop: shim connection closed")
		p.setDeathReason(DeathReasonShimEOF)
		return
	}
	if afterDrain {
		log.Warn("readLoop: shim read error after oversize drain", "err", readErr)
		p.setDeathReason(DeathReasonShimOversizeThenErr)
		return
	}
	log.Warn("readLoop: shim read error", "err", readErr)
	p.setDeathReason(DeathReasonShimReadErr)
}

// handleShimMessage dispatches one parsed shim frame so readLoop's body stays
// at the I/O + framing layer. Returns shimDispatchReturn when readLoop must
// unwind (cli_exited terminal frame or a stdout dispatch observed killCh).
func (p *Process) handleShimMessage(msg shimMsg, log *slog.Logger) shimDispatchOutcome {
	switch msg.Type {
	case "stdout":
		return p.handleShimStdout(msg, log)

	case "stderr":
		log.Debug("cli stderr", "line", sanitizeStderrLine(msg.Line))

	case "cli_exited":
		p.handleShimCLIExited(msg, log)
		return shimDispatchReturn

	case "pong":
		// Signal heartbeat loop that shim is responsive.
		select {
		case p.pongRecv <- struct{}{}:
		default:
		}

	case "error":
		// The shim wire is a semi-trusted boundary (a degraded/tampered shim
		// could emit arbitrary bytes), so sanitize like cli_exited.Signal. The
		// shim puts the text in `msg` (ServerMsg{Type:"error", Msg}); Line is a
		// fallback for legacy frames.
		errText := msg.Msg
		if errText == "" {
			errText = msg.Line
		}
		log.Warn("shim error", "msg", osutil.SanitizeForLog(errText, 256))
	}
	return shimDispatchContinue
}

// rpcErrorTurnEnd reports whether err is a protocol RPC-error sentinel that
// ReadEvent returns (with done=true) to signal "the backend rejected the
// request — close the turn". When it is, ok=true and tag is the short
// backend prefix for the synthesized result text. A non-RPC error (e.g. an
// unparseable frame) returns ok=false so the readLoop skips it.
//
// Every backend whose ReadEvent surfaces a post-handshake RPC error this way
// MUST register its sentinel here, else handleShimStdout drops the error and
// the session hangs in state=running. ACP/kiro: ErrACPRPC (session/prompt
// reject); codex: ErrCodexRPC (deferred turn/start reject, #2216).
func rpcErrorTurnEnd(err error) (tag string, ok bool) {
	switch {
	case errors.Is(err, ErrACPRPC):
		return "[kiro] ", true
	case errors.Is(err, ErrCodexRPC):
		return "[codex] ", true
	default:
		return "", false
	}
}

// handleShimStdout decodes a stdout frame into one or more protocol Events
// and runs each through HandleEvent / dispatchProtocolEvent. Returns
// shimDispatchReturn when dispatch reports killCh fired.
func (p *Process) handleShimStdout(msg shimMsg, log *slog.Logger) shimDispatchOutcome {
	p.lastSeq.Store(msg.Seq)
	// Prefer ReadEventInto so the dominant single-event frame reuses
	// p.readEventBuf instead of allocating a []Event per stdout line (#1676).
	var (
		events []Event
		err    error
	)
	// done is intentionally discarded (#2303): turn-end is driven only by a
	// result Event in the dispatch loop below (or the rpcErrorTurnEnd synthesis
	// on err), never by this advisory bool. A protocol that needs a turn to
	// close MUST emit a result Event — see ProtocolCore.ReadEvent.
	if ri, ok := p.protocol.(eventReaderInto); ok {
		events, _, err = ri.ReadEventInto(msg.Line, p.readEventBuf[:0])
	} else {
		events, _, err = p.protocol.ReadEvent(msg.Line)
	}
	if err != nil {
		// Backend RPC error (ACP/kiro session/prompt reject; codex's deferred
		// turn/start reply, e.g. -32001 overload): the turn is over from the
		// backend's POV, so synthesize a visible "result" and let the active
		// Send() unblock — otherwise state stays "running" forever. New
		// protocols must register their sentinel in rpcErrorTurnEnd.
		if tag, ok := rpcErrorTurnEnd(err); ok {
			events = []Event{{
				Type:    "result",
				SubType: "error",
				Result:  tag + err.Error(),
			}}
			log.Warn("readLoop: backend returned RPC error; surfacing as failed turn",
				"err", err, "seq", msg.Seq)
			// Fall through into the normal turn-end dispatch path
			// below so the assistant bubble + state transition happen.
		} else {
			log.Warn("readLoop: skip unparseable event", "err", err, "seq", msg.Seq)
			return shimDispatchContinue
		}
	}
	// The only multi-event frame today is ACP's stopReason response (assistant
	// text, result); iterating preserves single-event claude semantics.
	for _, ev := range events {
		if ev.Type == "" {
			continue
		}
		// control_ack resolves a pending SetModel waiter and must never reach
		// HandleEvent / EventLog / the dashboard — it is an RPC ack, not
		// conversation content (docs/rfc/dashboard-model-effort-control.md §4.4).
		if ev.Type == "control_ack" {
			p.deliverControlAck(ev)
			continue
		}
		if p.protocol.HandleEvent(p.shimStdinWriter(), ev) {
			continue
		}
		if p.dispatchProtocolEvent(ev, log) {
			return shimDispatchReturn
		}
	}
	return shimDispatchContinue
}

// handleShimCLIExited finalises a cli_exited terminal frame: stamps
// deathReason (sanitising any shim-supplied signal name), transitions State to
// Dead, and closes the shim socket so heartbeatLoop stops pinging a dead fd.
func (p *Process) handleShimCLIExited(msg shimMsg, log *slog.Logger) {
	var code int64
	if msg.Code.Present {
		code = msg.Code.Value
	}
	log.Info("CLI exited via shim", "code", code)
	reason := DeathReasonCLIExited
	if code != 0 {
		reason = DeathReasonCLIExited + "_code_" + strconv.FormatInt(code, 10)
	} else if msg.Signal != "" {
		// msg.Signal comes from a separate, tamperable process and flows into
		// slog attrs and /api/sessions JSON → HTML, so sanitize it; the numeric
		// branch is safe via FormatInt.
		reason = DeathReasonCLIExited + "_signal_" + osutil.SanitizeForLog(msg.Signal, 32)
	}
	p.setDeathReason(reason)
	p.transitionToDead()
	// Close the shim conn so heartbeatLoop stops writing pings into a dead
	// socket and the fd is released promptly (otherwise it leaks to GC if the
	// process is never Kill/Detach'd). closeShimConn is sync.Once-guarded.
	p.closeShimConn()
}

// transitionToDead performs the closing handshake when readLoop concludes a
// process has stopped producing events: flips State to Dead, fires onTurnDone
// once, and unblocks SendPassthrough callers parked on pendingSlots with
// ErrProcessExited. Called from the cli_exited frame (caller stamps deathReason
// and then closeShimConn) and from the fall-out exit (Kill / shim EOF / read
// error; classifyEOF already stamped the reason, and Kill's shimConn.Close is
// what unblocked us). Deliberately does NOT call setDeathReason or
// closeShimConn so each caller keeps its own classification + cleanup contract.
func (p *Process) transitionToDead() {
	p.mu.Lock()
	p.state = StateDead
	cb := p.onTurnDone
	p.mu.Unlock()
	if cb != nil {
		cb()
	}
	// Passthrough slot cleanup: every pending slot's caller is blocked inside
	// SendPassthrough waiting on resultCh/errCh. Fire ErrProcessExited so they
	// unblock with a clear error.
	p.discardAllPending(ErrProcessExited)
}

// readShimLine reads one complete shim message line from r, accumulating
// ReadSlice chunks until '\n'. lineBuf's capacity is reused; the caller decides
// whether to retain `line` or shrink (see readLoop). Pure I/O. Failure modes:
//   - capExceeded=true: the line would exceed maxScannerBufBytes; the rest of
//     the overlong line is drained so the next call starts at a message
//     boundary. Caller discards `line`. A read error while draining is carried
//     in readErr so death can be classified as "after oversize drain".
//   - readErr != nil, capExceeded=false: primary read error (io.EOF /
//     net.ErrClosed / I/O fault); `line` may hold a partial message.
func readShimLine(r *bufio.Reader, lineBuf []byte) (line []byte, capExceeded bool, readErr error) {
	line = lineBuf[:0]
	// chunkTerminated: the cap-exceeding chunk already contained '\n', so the
	// reader is positioned at the next message and we MUST NOT drain — that
	// would consume the following message (#1014).
	chunkTerminated := false
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) > 0 {
			if len(line)+len(chunk) > maxScannerBufBytes {
				capExceeded = true
				// ReadSlice returns nil error iff the chunk ended on '\n'.
				chunkTerminated = err == nil
				break
			}
			line = append(line, chunk...)
		}
		if err == nil {
			return line, false, nil // terminator found
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // keep reading until newline or cap
		}
		readErr = err
		return line, false, readErr
	}
	// Drain the rest of the overlong line so its tail isn't read as a separate
	// message, unless the cap-exceeding chunk already terminated it.
	if !chunkTerminated {
		for {
			_, err := r.ReadSlice('\n')
			if err == nil {
				break
			}
			if !errors.Is(err, bufio.ErrBufferFull) {
				readErr = err
				break
			}
		}
	}
	return line, true, readErr
}

// passthroughShouldFanOut reports whether a passthrough-mode assistant event
// should be delivered to onEvent callbacks. Mirrors the gate in the legacy Send
// path (process_send.go): only events carrying a thinking or tool_use content
// block, or an AskQuestion payload, warrant fan-out; text-only assistant events
// are excluded so replyTracker walks don't fire per streamed chunk. A nil
// ev.Message is treated as not fan-out-worthy.
func passthroughShouldFanOut(ev Event) bool {
	if ev.AskQuestion != nil {
		return true
	}
	if ev.Message == nil {
		return false
	}
	for _, block := range ev.Message.Content {
		if block.Type == "thinking" || block.Type == "tool_use" {
			return true
		}
	}
	return false
}

// dispatchProtocolEvent runs the per-Event side of readLoop: passthrough hooks,
// linker plumbing, EventLog append, mid-turn reconnect bookkeeping, and the
// non-blocking handoff to Send via eventCh. Returns true if a kill signal was
// observed during dispatch and the caller should unwind the read loop.
func (p *Process) dispatchProtocolEvent(ev Event, log *slog.Logger) bool {
	// Type:"metadata" is a normalize-channel status frame (kiro _kiro.dev/
	// metadata), not assistant output: apply to atomic state and skip
	// eventCh / EventLog. See docs/rfc/multi-backend.md §8.8.
	if ev.Type == "metadata" {
		p.applyMetadata(ev.Metadata)
		return false
	}

	// One time.Now() shared between ev.recvAt (for drainStaleEvents) and the
	// EventEntry.Time values from logEventAt; UnixMilli cached for the up-to-4
	// uses below.
	now := time.Now()
	nowMS := now.UnixMilli()

	// ---- Passthrough mode hooks ----
	// These run before the legacy eventCh / EventLog delivery paths.
	// They are cheap no-ops when passthrough is not in use (zero
	// pending slots, inTurn=false, protocol doesn't support replay).

	// system/init: mark start of new turn for turn-aggregation owner tracking
	// and watchdog baseline. Unconditional is harmless — onSystemInit only
	// matters when pendingSlots is non-empty and a replay arrives later.
	// isSystemInit 在这里求值一次，下方复用。
	isSystemInit := ev.Type == "system" && ev.SubType == "init"
	if isSystemInit && p.caps.Replay {
		p.onSystemInit()
	}

	// user replay: claim slots into currentTurnSlots. Filter out of
	// EventLog + eventCh so replay events don't pollute the dashboard
	// transcript or trigger legacy result detection.
	if ev.Type == "user" && ev.IsReplay {
		p.slotsMu.Lock()
		p.handleReplayEventLocked(ev)
		p.slotsMu.Unlock()
		return false
	}

	// Passthrough interim assistant events: deliver to currentTurnSlots owners'
	// onEvent so AskUserQuestion cards and thinking/tool_use banners reach the
	// IM tracker (#1958); gated like the legacy Send path via
	// passthroughShouldFanOut. Legacy eventCh delivery still happens below.
	// slotsMu is held only to snapshot owners — callbacks may block on Reply.
	if ev.Type == "assistant" && p.caps.Replay && passthroughShouldFanOut(ev) {
		p.slotsMu.Lock()
		owners := make([]*sendSlot, len(p.currentTurnSlots))
		copy(owners, p.currentTurnSlots)
		p.slotsMu.Unlock()
		for _, owner := range owners {
			if owner.onEvent != nil {
				owner.onEvent(ev)
			}
		}
	}

	// result under passthrough: fan-out to claimed slots and skip
	// legacy eventCh delivery. We still log to EventLog so dashboard
	// sees the turn-complete event.
	if ev.Type == "result" && p.caps.Replay {
		// error_during_execution signals the CLI aborted the turn —
		// e.g. a priority:"now" preempted it. Any older pending slot
		// written before `now` that was never replayed was dropped
		// by the CLI; fire ErrAbortedByUrgent for those.
		if ev.SubType == "error_during_execution" {
			victims := p.reapAbortedPreempted()
			fireAbortErrors(victims)
		}
		owners := p.onTurnResult()
		if len(owners) > 0 {
			p.logEventAt(ev, nowMS)
			// Fire onEvent for each owner's turn-scope callback
			// before delivering the terminal result.
			for _, owner := range owners {
				if owner.onEvent != nil {
					owner.onEvent(ev)
				}
			}
			fanoutTurnResult(owners, ev)
			return false
		}
		// No owner claimed this result. (a) abort with no claimed slots: log
		// here and skip the legacy path — abort errors already fired above.
		// (b) true stray result (reconnect, no active Send): fall through so the
		// unconditional logEventAt below records the turn-complete entry (#1483);
		// under passthrough no legacy eventCh consumer would append it.
		if ev.SubType == "error_during_execution" {
			p.logEventAt(ev, nowMS)
			return false
		}
	}

	// claude advertises the resolved model + binary version in system/init.
	// readLoop is the always-on path (active during reconnect when no Send()
	// consumes events), so capture here too; process_send.go has a parallel
	// hook for when Send() drains init first (same value, first writer wins).
	if isSystemInit && ev.Model != "" {
		p.setModel(ev.Model)
	}
	// Live version reflects the binary THIS process exec'd, not the spawn-time
	// Wrapper.CLIVersion (stale after a host claude upgrade).
	if isSystemInit && ev.ClaudeCodeVersion != "" {
		p.setLiveVersion(ev.ClaudeCodeVersion)
	}
	p.notifyLinker(ev, nowMS, isSystemInit)

	// Always log to EventLog so dashboard subscribers see events
	// even when no Send() is active (e.g., after service restart
	// reconnects to a shim that's mid-turn).
	p.logEventAt(ev, nowMS)

	// A result with no active Send() (reconnect set Running via isMidTurn but
	// the CLI finished first) transitions back to Ready. Gated on
	// reconnectedMidTurn: otherwise State=Running means Send() owns the
	// State→Ready transition via its defer, and racing it would let a second
	// Send() start before that defer runs. The flag is one-shot.
	if ev.Type == "result" && p.reconnectedMidTurn.CompareAndSwap(true, false) {
		p.mu.Lock()
		wasRunning := p.state == StateRunning
		if wasRunning {
			p.state = StateReady
		}
		cb := p.onTurnDone
		p.mu.Unlock()
		if wasRunning && cb != nil {
			// The killCh select in deliverEvent may fire cb again in the same
			// iteration if Kill() raced this path (onTurnDone is idempotent).
			cb()
		}
	}

	return p.deliverEvent(ev, now, log)
}

// notifyLinker forwards system/init context and system/task_started events to
// the SubagentLinker. Re-gates internally on `p.linker != nil` so the caller
// can pass any event without a pre-check.
func (p *Process) notifyLinker(ev Event, nowMS int64, isSystemInit bool) {
	if p.linker == nil {
		return
	}
	if isSystemInit && ev.SessionID != "" {
		// cachedProjectDir avoids a rune scan + os.UserHomeDir syscall per init.
		p.linker.SetContext(p.cachedProjectDir, ev.SessionID)
	}
	// Resolve for BOTH in-process teammates (task_type="in_process_teammate")
	// AND standalone sub-agents (task_type often empty/vendor-specific): both
	// write subagents/agent-<task_id>.jsonl. Exclude local_bash — those only
	// persist to tool-results/ and have no internal transcript.
	if ev.Type != "system" || ev.SubType != "task_started" ||
		ev.TaskType == "local_bash" || ev.TaskID == "" || ev.ToolUseID == "" {
		return
	}
	taskID := ev.TaskID
	toolUseID := ev.ToolUseID
	linker := p.linker
	// Resolved fast-path BEFORE the dispatch so repeated task_started events for
	// the same task_id (reconnect/replay, progress envelopes) don't each pay a
	// schedule + closure capture (#478).
	if info, ok := linker.Query(taskID); ok && info.Resolved {
		return
	}
	// In-flight dedup (#1354): while a Resolve is mid-poll (up to ~3 s) the
	// byTaskID map is still empty for this taskID, so Query alone lets every
	// duplicate task_started in that window schedule a fresh resolve.
	if !linker.TryMarkResolveInflight(taskID) {
		return
	}
	// task_started.description is "<name>: <prompt body>" for teammates and
	// just the prompt for sub-agents; trimming to the name prefix only helps
	// the name-scan fallback. The prefix can carry leading whitespace, hence
	// the second TrimSpace.
	name := strings.TrimSpace(ev.Description)
	if idx := strings.IndexByte(name, ':'); idx > 0 {
		name = strings.TrimSpace(name[:idx])
	}
	// Cap description before handing it to the resolve worker: ev.Description
	// is unbounded text pinned until the resolveSem slot frees, so a burst of
	// multi-KB descriptions × 8 parallel resolves can retain MBs. The linker
	// never decodes it, so a byte cap at a rune boundary suffices (≤ 8000
	// bytes ⇒ ≤ 8000 runes) and skips a per-event utf8 scan.
	const maxResolveDescBytes = 8000
	desc := ev.Description
	if len(desc) > maxResolveDescBytes {
		desc = desc[:textutil.TruncateAtRuneBoundary(desc, maxResolveDescBytes)]
	}
	// Hand off to the linker's worker pool (bounded queue, inline fallback on
	// overflow) instead of a per-event goroutine parked on resolveSem (#415).
	linker.DispatchResolve(p.lifecycleContext(), taskID, toolUseID, name, desc, nowMS)
}

// deliverEvent runs the post-EventLog dispatch arm of dispatchProtocolEvent:
// killCh probe followed by the non-blocking handoff to eventCh for Send()
// consumption. Returns true when killCh fired and the read loop should unwind.
func (p *Process) deliverEvent(ev Event, now time.Time, log *slog.Logger) bool {
	select {
	case <-p.killCh:
		p.setDeathReason(DeathReasonKilled)
		p.mu.Lock()
		p.state = StateDead
		cb := p.onTurnDone
		p.mu.Unlock()
		if cb != nil {
			cb()
		}
		// Unblock any passthrough SendPassthrough callers immediately.
		// The defer at readLoop end also calls discardAllPending, but
		// that runs after we drain any remaining stdin frames — a kill
		// race with active slots would otherwise wait for the outer
		// loop to fully unwind (tens of ms under load).
		p.discardAllPending(ErrProcessExited)
		return true
	default:
	}

	// Non-blocking handoff to Send(): if the buffer is full (no active Send)
	// the event is already in EventLog for the dashboard. recvAt is set just
	// before handoff so drainStaleEvents can separate events queued before a
	// new turn from events produced for it.
	ev.recvAt = now
	select {
	case p.eventCh <- ev:
	default:
		// Drop is safe (EventLog kept the entry), but a dropped `result` forces
		// a non-Replay Send() into the findResultSince fallback — Warn. Under
		// Replay backends a result lands here only when no slot owns it (an
		// expected pathway), so Debug to avoid masking the real drop case.
		switch {
		case ev.Type == "result" && !p.caps.Replay:
			log.Warn("eventCh full, dropped result", "subtype", ev.SubType)
		case ev.Type == "result":
			log.Debug("eventCh full, dropped result (replay backend)", "subtype", ev.SubType)
		default:
			log.Debug("eventCh full, dropped", "type", ev.Type)
		}
	}
	return false
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
			// Drain pongs queued during a scheduler stall so the pong consumed
			// in the select below is unambiguously the response to the ping
			// we are about to send, not a late one that would declare the shim
			// healthy before it has answered.
			for {
				select {
				case <-p.pongRecv:
					continue
				default:
				}
				break
			}
			// Ping payload is fully static; a pre-marshalled []byte skips the
			// encoder pool + reflection every 30s × N live processes.
			if err := p.shimSendRaw(shimPingBytes); err != nil {
				log.Debug("heartbeat ping failed", "err", err)
				p.Kill()
				return
			}

			// Wait for pong within half the interval. Go ≥1.23 guarantees no
			// tick is delivered after Stop returns, so Reset is safe without the
			// `if !Stop() { <-C }` drain dance (a stale already-delivered tick is
			// discarded by the next iteration). go.mod pins go 1.26.3 — down-
			// revving below 1.23 requires reinstating the explicit drain.
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
