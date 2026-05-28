package cli

// process_readloop.go — inbound shim socket read goroutine and heartbeat.
//
// Owns: readLoop (inbound stdout pump), the shim heartbeat loop, and
// shimMsg (the inbound wire frame, also consumed by wrapper.go's Spawn
// handshake).
//
// Related constants — maxScannerBufBytes / lineBufShrinkThreshold — live
// in process.go's const block alongside DefaultNoOutputTimeout because
// they are timing-budget knobs grouped semantically rather than
// physically.
//
// R227-ARCH-19: dropped the "Phase 2 of process-split / zero semantic
// change" preamble; refactor is complete, history lives in git log.

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
)

// shimMsg is a minimal struct for parsing shim protocol messages in readLoop.
//
// R222-PERF-13: Code uses a custom (int, bool) pair instead of *int so
// every cli_exited frame avoids the 1× heap allocation that *int would
// trigger when json.Unmarshal materialises the integer behind a pointer.
// json.Unmarshal calls UnmarshalJSON which sets CodePresent=true; an
// absent "code" field leaves CodePresent=false. Equivalent to the
// pointer encoding's "distinguishes 0 from absent" guarantee.
type shimMsg struct {
	Type   string      `json:"type"`
	Seq    int64       `json:"seq,omitempty"`
	Line   string      `json:"line,omitempty"`
	Code   shimMsgCode `json:"code,omitempty"`
	Signal string      `json:"signal,omitempty"`
}

// shimMsgCode wraps an int so json.Unmarshal can distinguish absent
// from explicit zero without allocating *int. Decode-only; never
// emitted from naozhi side. R222-PERF-13.
type shimMsgCode struct {
	Value   int
	Present bool
}

// UnmarshalJSON implements json.Unmarshaler so the shim protocol's
// optional "code" field decodes without per-message heap allocation.
// json.Unmarshal calls this method only when the JSON object actually
// contains a "code" key — absent keys leave the zero value
// (Present=false). R222-PERF-13.
func (c *shimMsgCode) UnmarshalJSON(data []byte) error {
	c.Present = true
	// Delegate to json.Unmarshal for full int parsing semantics (rejects
	// strings/bools/floats outside int range identically to *int).
	return json.Unmarshal(data, &c.Value)
}

// readLoop reads NDJSON messages from the shim socket and dispatches events.
func (p *Process) readLoop() {
	log := p.slogger()
	// RNEW-007: Defers execute LIFO. Declaration order below is:
	//   close(eventCh) -> close(done) -> CloseSubscribers -> recover
	// Execution order on return is the reverse:
	//   1. recover block: transition p.state to StateDead and fire onTurnDone
	//   2. CloseSubscribers: unblock EventLog subscribers
	//   3. close(done): signal readLoop exit to waiters
	//   4. close(eventCh): isChanAlive relies on done closing BEFORE eventCh so
	//      any producer guarded by "is done open?" never sends on a closed
	//      eventCh. See drainStaleEvents / isChanAlive (defined in
	//      process_turn.go) for the invariant.
	// If you reorder these defers, re-verify the isChanAlive invariant.
	defer close(p.eventCh)
	defer close(p.done)
	defer p.eventLog.CloseSubscribers()
	// Panic recover: a malformed shim message or protocol bug must not take
	// the whole process down silently. We log stack + transition to Dead so
	// the router can reap this session and the dashboard surfaces the failure
	// instead of the user seeing a stalled "running" forever.
	//
	// R222-GO-9 onTurnDone partial-state contract: when this recover fires,
	// onTurnDone is invoked BEFORE the deferred close(p.done) above runs (the
	// recover defer was registered AFTER close(p.done), so it executes first
	// in LIFO order). Callbacks therefore observe p.done still open even
	// though State==Dead. This is intentional: onTurnDone semantics are
	// "turn boundary reached, reap progress" rather than "process channels
	// fully torn down" — the normal terminal path also fires onTurnDone
	// before the deferred channel closes. Callbacks must NOT use p.done as
	// a "process is fully torn down" signal; use IsRunning / GetState
	// instead, both of which already reflect StateDead at this point.
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
		line, capExceeded, readErr := readShimLine(p.shimR, lineBuf)
		// Propagate grown capacity so the next iteration starts with the
		// expanded backing array instead of reverting to the original 4096.
		// Without this, a single large event forces every subsequent
		// iteration to re-grow from 4KB through a chain of doublings.
		//
		// Exception 1: on capExceeded we shrink back to a fresh 4KB buffer.
		// Holding onto a ~16MB backing array forever because one malformed
		// shim message grew us there is a silent memory hog.
		//
		// Exception 2: if a single legitimate large event pushed capacity
		// past lineBufShrinkThreshold (256 KiB), reset too. Most
		// stream-json events are <4 KiB; common tool_result / assistant
		// text chunks run 50-200 KiB and we want to RETAIN that capacity
		// (pay the realloc once per session, not once per event). Only
		// truly exceptional events (>256 KiB) trigger the shrink, so the
		// permanent RSS footprint is bounded by 50 sessions × 256 KiB
		// ≈ 12.8 MiB worst case, an acceptable ceiling relative to the
		// realloc churn the lower threshold caused on every common event.
		// Decide lineBuf in a single step. The capExceeded branch previously
		// did `lineBuf = line` then immediately `lineBuf = make(...)`, which
		// pinned a transient second reference to `line`'s large backing
		// array until the next GC. Folding the two cases into one guard
		// also avoids doing an unnecessary `make` on the hot path when the
		// shrink isn't needed.
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

	// readLoop fell out of the read loop without hitting cli_exited — the
	// caller-facing reason was already recorded above when the read error was
	// classified (shim EOF / read error / drained). If none of those paths
	// fired, Kill() was what unblocked ReadSlice via shimConn.Close, which
	// surfaces as net.ErrClosed and is already classified as DeathReasonShimEOF.
	p.transitionToDead()
}

// shimDispatchOutcome encodes the readLoop control transition produced by
// handleShimMessage. shimDispatchContinue is the zero value so the default
// path through readLoop is the cheapest; shimDispatchReturn signals the
// outer loop must unwind (cli_exited terminal frame or a stdout dispatch
// observed killCh). R214-CODE-3.
type shimDispatchOutcome int

const (
	shimDispatchContinue shimDispatchOutcome = iota
	shimDispatchReturn
)

// classifyEOF stamps the appropriate deathReason for a shim-socket read
// error and emits a single log line at the matching level. afterDrain
// flags the post-oversize-drain branch so the log message reflects which
// readLoop path observed the error. Pure side-effects: no return value
// because the caller's break-from-loop decision is independent of the
// classification (any non-nil readErr breaks). R214-CODE-3.
//
// R20260527-GO-19 (#1288): the afterDrain && closed arm previously stamped
// DeathReasonShimEOF — semantically misleading because the shim socket
// closure was preceded by an oversize protocol frame (the drain loop was
// running BECAUSE we had just refused a >maxScannerBufBytes line). Health
// dashboards conflating the two arms could not distinguish a normal shim
// shutdown from a degraded shim that emitted a malformed/giant frame
// before the pipe gave up. Stamp DeathReasonShimOversizeThenEOF so the
// two operational signatures are separable. The log lines were already
// distinct (kept as-is); only the deathReason channel changes.
func (p *Process) classifyEOF(readErr error, afterDrain bool, log *slog.Logger) {
	closed := errors.Is(readErr, io.EOF) || errors.Is(readErr, net.ErrClosed)
	if closed {
		if afterDrain {
			// R20260527-GO-19 (#1288): preserve the fact that a
			// capExceeded oversize line preceded the EOF. Collapsing
			// this into plain DeathReasonShimEOF hid the upstream
			// shim-side overflow that triggered the close, making
			// dashboard/health forensics ambiguous between "clean
			// shim shutdown" and "shim died right after emitting
			// >maxScannerBufBytes".
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

// handleShimMessage dispatches one parsed shim frame. Carved out of
// readLoop's inner switch so the outer loop body stays at the I/O +
// framing layer and per-frame protocol decisions live here. Returns
// shimDispatchReturn when readLoop must unwind (cli_exited terminal frame
// or a stdout dispatch observed killCh). R214-CODE-3.
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
		// Sanitize shim-supplied message: shim wire is a semi-trusted
		// boundary (degraded/tampered shim could emit arbitrary bytes).
		// Mirrors the R183-SEC-H1 / R184-SEC-M1 policy used for
		// cli_exited.Signal and ACP rpc error messages.
		log.Warn("shim error", "msg", osutil.SanitizeForLog(msg.Line, 256))
	}
	return shimDispatchContinue
}

// handleShimStdout decodes a stdout frame into one or more protocol Events
// and runs each through HandleEvent / dispatchProtocolEvent. Returns
// shimDispatchReturn when dispatch reports killCh fired so the readLoop
// teardown path can unwind. R214-CODE-3.
func (p *Process) handleShimStdout(msg shimMsg, log *slog.Logger) shimDispatchOutcome {
	p.lastSeq.Store(msg.Seq)
	events, _, err := p.protocol.ReadEvent(msg.Line)
	if err != nil {
		// ACP RPC errors: kiro returned an error response to a request
		// we sent (typically session/prompt). The turn is over from
		// kiro's POV — done=true comes back from ReadEvent so we
		// can synthesize a visible "result" event and let the active
		// Send() unblock. Without this, state stays "running" forever
		// (operator-visible as "kiro session never replies"; reproduced
		// 2026-05-19 r3-cancel/r3-lifecycle stuck after restart).
		if errors.Is(err, ErrACPRPC) {
			events = []Event{{
				Type:    "result",
				SubType: "error",
				Result:  "[kiro] " + err.Error(),
			}}
			log.Warn("readLoop: kiro returned RPC error; surfacing as failed turn",
				"err", err, "seq", msg.Seq)
			// Fall through into the normal turn-end dispatch path
			// below so the assistant bubble + state transition happen.
		} else {
			log.Warn("readLoop: skip unparseable event", "err", err, "seq", msg.Seq)
			return shimDispatchContinue
		}
	}
	// ReadEvent now returns a slice. Today the only multi-event frame
	// is ACPProtocol's stopReason response, which emits
	// (assistant text, result) — iterating preserves the single-event
	// claude semantics while letting the ACP turn-end split land
	// naturally. dispatchProtocolEvent reports back when killCh fired
	// so the outer readLoop can return and trigger teardown.
	for _, ev := range events {
		if ev.Type == "" {
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
// deathReason (sanitising any shim-supplied signal name), transitions
// State to Dead, and closes the shim socket so heartbeatLoop stops
// pinging into a dead fd. The caller (handleShimMessage) returns
// shimDispatchReturn so readLoop unwinds. R214-CODE-3.
func (p *Process) handleShimCLIExited(msg shimMsg, log *slog.Logger) {
	code := 0
	if msg.Code.Present {
		code = msg.Code.Value
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
	p.transitionToDead()
	// Close shim conn so heartbeatLoop stops writing pings into a dead
	// socket and the bufio.Writer's fd is released promptly. Without
	// this, if the process isn't subsequently Kill/Detach'd (e.g. when
	// Router.Cleanup evicts it from the map), the fd leaks to GC.
	// closeShimConn is sync.Once-guarded so a later Kill/Detach is safe
	// without producing a "use of closed network connection" debug log
	// on the second close attempt. R219-GO-3.
	p.closeShimConn()
}

// transitionToDead performs the closing handshake when readLoop concludes a
// process has stopped producing events: flips State to Dead, fires the
// onTurnDone callback once, and unblocks any SendPassthrough callers parked
// on pendingSlots with ErrProcessExited.
//
// Called from two readLoop exit points:
//
//  1. The cli_exited shim message (orderly CLI exit). Caller sets the
//     deathReason explicitly and follows up with closeShimConn() to release
//     the heartbeat fd. R219-GO-3.
//
//  2. The fallback exit when readLoop falls past the read loop without a
//     cli_exited frame (Kill() / shim EOF / read error). Caller has already
//     stamped DeathReasonShimEOF when classifying the read error; no
//     closeShimConn here because Kill()/shimConn.Close is what unblocked us.
//
// This helper deliberately does NOT call setDeathReason or closeShimConn so
// each caller keeps its specific death-classification + cleanup contract.
//
// onTurnDone idempotency: this function may run after a partial-state
// recovery in the panic defer (R222-GO-9). The defer guards against
// double-fire by checking p.state before calling cb, so callers here can
// rely on at-most-once semantics for the callback per readLoop instance.
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
// chunks across ReadSlice calls until a '\n' is found. Two distinct failure
// modes are surfaced via the (capExceeded, readErr) pair:
//
//   - capExceeded=true: the assembled line would exceed maxScannerBufBytes.
//     The helper drains the rest of the overlong line so the next call
//     starts cleanly at the following message boundary. Caller should
//     discard `line` and continue. If draining hits its own read error,
//     readErr carries it forward so the caller can classify cause-of-death
//     under the "after oversize drain" branch.
//
//   - readErr != nil (with capExceeded=false): a primary read error
//     (io.EOF / net.ErrClosed / unexpected I/O fault). `line` may carry
//     a partial message from before the error; caller decides whether
//     to process or drop it.
//
// lineBuf is the previous iteration's accumulator: the helper truncates
// it to length 0 and reuses its capacity. Caller owns the lifetime —
// after this returns, caller must decide whether to retain (line) for
// the next call (saves alloc) or shrink back to a fresh 4 KiB buffer.
// See readLoop for the lineBufShrinkThreshold decision.
//
// R182-GO-P1-2: errors.Is on bufio.ErrBufferFull (not == comparison) so
// future middleware that wraps the error still matches.
//
// No side effects on Process state — pure I/O. Extracted from readLoop
// so per-fix churn on the line accumulator (R182-GO-P1-2 / R225-CR-7 /
// R229-PERF-3 ground) lands in this helper instead of further bloating
// the long readLoop body.
func readShimLine(r *bufio.Reader, lineBuf []byte) (line []byte, capExceeded bool, readErr error) {
	line = lineBuf[:0]
	// chunkTerminated tracks whether the cap-exceeding chunk already contained
	// the message terminator ('\n'). When true, the next call starts cleanly
	// at the next message boundary and we MUST NOT drain — draining would
	// consume the following message. R234-PERF-13 (#1014).
	chunkTerminated := false
	for {
		chunk, err := r.ReadSlice('\n')
		if len(chunk) > 0 {
			if len(line)+len(chunk) > maxScannerBufBytes {
				capExceeded = true
				// ReadSlice returns nil error iff chunk ended on '\n'; that
				// chunk IS the message terminator, so the bufio reader is
				// already positioned at the next message and the drain
				// loop below would erroneously eat it.
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
	// capExceeded path: drain the rest of the overlong line so the next call
	// doesn't read its tail as a separate message. Skip when the cap-exceeding
	// chunk already terminated the line (see chunkTerminated above).
	// ReadSlice returns nil on '\n' delimiter; ErrBufferFull means buffer
	// filled with no newline.
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

// dispatchProtocolEvent runs the per-Event side of readLoop: passthrough hooks,
// linker plumbing, EventLog append, mid-turn reconnect bookkeeping, and the
// non-blocking handoff to Send via eventCh. Returns true if a kill signal was
// observed during dispatch and the caller should unwind the read loop.
//
// Extracted from the inline switch body when Protocol.ReadEvent moved from a
// single Event to a slice (ACP turn-end emits assistant+result), so each
// dispatched Event sees the full pipeline regardless of how many wire frames
// fed it.
func (p *Process) dispatchProtocolEvent(ev Event, log *slog.Logger) bool {
	// Type:"metadata" is a normalize-channel event (kiro
	// _kiro.dev/metadata today; future backends use the same
	// shape). Apply to Process atomic state and skip downstream
	// dispatch — these are status frames, not assistant output, so
	// they should not flow through eventCh / EventLog. Snapshot
	// reads the values lock-free.
	// See docs/rfc/multi-backend.md §8.8.
	if ev.Type == "metadata" {
		p.applyMetadata(ev.Metadata)
		return false
	}

	// Capture one time.Now() shared between ev.recvAt (handed to
	// drainStaleEvents) and the EventEntry.Time values produced by
	// logEventAt. Previously the two read wall-clock independently,
	// which is measurable at 5-50 events/s × N active sessions.
	// R67-PERF-9. Also cache UnixMilli once — used up to 4× per
	// event by the dispatch below.
	now := time.Now()
	nowMS := now.UnixMilli()

	// ---- Passthrough mode hooks ----
	// These run before the legacy eventCh / EventLog delivery paths.
	// They are cheap no-ops when passthrough is not in use (zero
	// pending slots, inTurn=false, protocol doesn't support replay).

	// system/init: mark start of new turn for turn-aggregation owner
	// tracking and watchdog baseline. Keeping this unconditional is
	// harmless — onSystemInit only matters when pendingSlots is
	// non-empty and a replay arrives later.
	// R234-PERF-12: 同一帧后续 (line ~518) 还要再判一次 system/init，
	// 提前在这里求值一次复用，省两次 string 比较
	// (5-50 events/s × N session)。
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
		// No owners claim this result. Under passthrough this means
		// either (a) an abort with no claimed slots, handled above,
		// or (b) stray result during reconnect. Either way skip
		// legacy eventCh; dashboard EventLog already has the entry.
		if ev.SubType == "error_during_execution" {
			p.logEventAt(ev, nowMS)
			return false
		}
		// Fall through to legacy path only for true stray results.
	}

	// SubagentLinker plumbing for RFC v4 agent-team-ui.
	//   - system.init carries the parent session_uuid used as the
	//     sub-key under ~/.claude/projects/<projectDir>/.
	//   - system.task_started with task_type=="in_process_teammate"
	//     is our cue that the CLI has (or is about to) write
	//     subagents/agent-<hex>.jsonl; kick off an async Resolve
	//     bounded by the linker's retry budget so readLoop stays
	//     responsive.
	// UI Round 5 R5-3: claude advertises the resolved model in
	// system/init. readLoop is the always-on path (active even
	// during reconnect when no Send() is consuming events), so
	// capture here too — the parallel hook in process_send.go
	// covers the case where Send() drains init before readLoop
	// observes it (race; first to call setModel wins, both
	// values are the same so it doesn't matter). Only overwrite
	// when init event actually carries a model value.
	// isSystemInit 已在上方求值，直接复用 (R234-PERF-12)。
	if isSystemInit && ev.Model != "" {
		p.setModel(ev.Model)
	}
	// R237-GO-5 (#628): linker plumbing extracted into notifyLinker so
	// dispatchProtocolEvent stays focused on the EventLog/eventCh dispatch
	// pipeline. Behaviour is byte-identical; the helper internally re-
	// gates on linker presence and Type/SubType.
	p.notifyLinker(ev, nowMS, isSystemInit)

	// Always log to EventLog so dashboard subscribers see events
	// even when no Send() is active (e.g., after service restart
	// reconnects to a shim that's mid-turn).
	p.logEventAt(ev, nowMS)

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
		wasRunning := p.state == StateRunning
		if wasRunning {
			p.state = StateReady
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

	return p.deliverEvent(ev, now, log)
}

// notifyLinker forwards system/init context and system/task_started events
// to the SubagentLinker. Extracted from dispatchProtocolEvent so the linker
// plumbing (~50 lines mixing context-set + Resolve fan-out) doesn't crowd
// the parent function's flat dispatch reading; behaviour is byte-identical.
// R237-GO-5 (#628). Re-gates internally on `p.linker != nil` so the caller
// can pass any event without a pre-check.
func (p *Process) notifyLinker(ev Event, nowMS int64, isSystemInit bool) {
	if p.linker == nil {
		return
	}
	if isSystemInit && ev.SessionID != "" {
		projectDir := resolveProjectDir(p.cwd)
		p.linker.SetContext(projectDir, ev.SessionID)
	}
	// Trigger Resolve for BOTH in-process teammates (TeamCreate's
	// Agent spawns; task_type="in_process_teammate") AND standalone
	// sub-agents (Task(subagent_type=...); task_type often empty
	// or vendor-specific). Both write subagents/agent-<task_id>.jsonl,
	// so the linker's fast path (stat by task_id) is the right common
	// denominator. Exclude local_bash — those only persist to
	// tool-results/ and have no internal transcript.
	if ev.Type != "system" || ev.SubType != "task_started" ||
		ev.TaskType == "local_bash" || ev.TaskID == "" || ev.ToolUseID == "" {
		return
	}
	taskID := ev.TaskID
	toolUseID := ev.ToolUseID
	linker := p.linker
	// R241-PERF-5 (#478): cache fast-path BEFORE the goroutine spawn so
	// repeated task_started events for the same task_id (which can fire
	// across reconnect/replay paths or claude's own progress envelopes)
	// do not each pay a goroutine schedule. Resolve already runs the
	// same byTaskID RLock check inside, but reaching it requires the
	// goroutine to be scheduled and to allocate a closure capture for
	// taskID/toolUseID/name/desc — Query short-circuits that.
	if info, ok := linker.Query(taskID); ok && info.Resolved {
		return
	}
	// task_started.description is "<name>: <prompt body>" for
	// teammates; for sub-agents it's just the prompt. The
	// linker's fast path works either way; trimming to the
	// name prefix only helps the name-scan fallback. Single
	// TrimSpace pass — the colon-prefix branch trims again
	// because the prefix can carry leading whitespace from
	// the producer. (R227-PERF-20)
	name := strings.TrimSpace(ev.Description)
	if idx := strings.IndexByte(name, ':'); idx > 0 {
		name = strings.TrimSpace(name[:idx])
	}
	// R225-CR-10 / R230B-PERF-7: cap description before handing it to a
	// goroutine closure. ev.Description is unbounded user/agent text that
	// the Resolve goroutine pins until the resolveSem slot frees, so a
	// burst of multi-KB descriptions × 8 max parallel resolves can retain
	// MBs of strings transiently. SubagentLinker only retains the string
	// for the bounded resolveSem window and never decodes it, so a
	// byte-level cap is sufficient: any UTF-8 payload ≤ 8000 bytes already
	// contains ≤ 8000 runes (and at the 2000-rune retention budget
	// previously used here, 2000 × 4 max bytes/rune = 8000). Skipping the
	// rune-decode loop avoids the per-event utf8 scan on the readLoop hot
	// path. Cut at the nearest rune boundary so any operator-side dump of
	// the value remains valid UTF-8.
	// R260528-GO-1: Resolve dropped its dead description parameter; the
	// truncate-before-closure block (formerly R225-CR-10 / R230B-PERF-7)
	// guarded a goroutine leak that no longer exists.
	go linker.Resolve(p.lifecycleContext(), taskID, toolUseID, name, nowMS)
}

// deliverEvent runs the post-EventLog dispatch arm of dispatchProtocolEvent:
// killCh probe followed by the non-blocking handoff to eventCh for Send()
// consumption. Returns true when killCh fired and the read loop should
// unwind. Extracted from dispatchProtocolEvent so the kill/deliver decision
// reads as a single helper and the parent function ends with a flat tail
// call. R237-GO-5 (#628). Behaviour is byte-identical to the inline
// version: same death-reason set, same drainage, same drop-log levels.
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
		// Full buffer: drop is safe (EventLog kept the entry) but
		// dropping a `result` event forces a non-Replay Send() into the
		// findResultSince fallback, so log at Warn for observability.
		//
		// R225-CR-13: under Replay-capable backends (passthrough), result
		// events fall through here only when there is no owning slot
		// (e.g. all slots already claimed and fanned out, or pre-handshake
		// strays). That is an expected pathway, not an observability
		// signal — keep it at Debug to avoid noise that masks the real
		// non-Replay drop case below.
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
			// R225-GO-6: drain any pongs that may have queued in pongRecv
			// during a scheduler stall in this heartbeatLoop goroutine.
			// pongRecv was bumped to capacity maxMisses+1 so the readLoop's
			// non-blocking send no longer drops pongs during stalls — but a
			// pong delivered just before this iteration also must not
			// satisfy the wait below for the *next* ping (otherwise we'd
			// declare the shim healthy before it has had a chance to
			// respond to the upcoming ping). Empty the buffer first so the
			// pong consumed in the select is unambiguously the response to
			// the ping we are about to send.
			for {
				select {
				case <-p.pongRecv:
					continue
				default:
				}
				break
			}
			// R222-PERF-14: heartbeat ping payload is fully static (no
			// runtime field). Using a pre-marshalled []byte skips the
			// encodeShimMsg pool acquire + json.Encoder reflection
			// every 30s × N live processes.
			if err := p.shimSendRaw(shimPingBytes); err != nil {
				log.Debug("heartbeat ping failed", "err", err)
				p.Kill()
				return
			}

			// Wait for pong within half the interval. Note on drain: Go 1.23+
			// guarantees that after Stop returns, no further ticks will be
			// delivered to the timer's channel — i.e. Reset is safe without
			// the historical `if !Stop() { <-C }` drain dance. (Already-
			// delivered ticks still need to be drained by the receiver in
			// the normal way, but here the only receiver is the select
			// below, which discards a stale tick on the next iteration.)
			//
			// LOCKED to toolchain ≥1.23: go.mod sets `go 1.26.3` so the
			// post-Stop no-future-tick guarantee holds. Down-revving go.mod
			// below 1.23 would reintroduce a stale-tick path where Reset
			// returns while a previous fire is still pending in the channel —
			// recreate the explicit drain dance before lowering the
			// toolchain. R222-GO-4.
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
