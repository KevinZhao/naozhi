package cli

// process_readloop.go — inbound shim socket read goroutine and heartbeat.
//
// Moved from process.go (Phase 2 of docs/rfc/process-split.md).
// Zero semantic change; pure file move. See the RFC for the full
// mapping.
//
// Related constants stay in process.go's const block:
//   - maxScannerBufBytes / lineBufShrinkThreshold (referenced only
//     here but kept grouped with related DefaultNoOutputTimeout etc.
//     to minimise Phase 2 diff).
//
// `isChanAlive` stays in process.go for Phase 2; it will move to
// process_turn.go in Phase 4 together with drainStaleEvents which is
// its sole user. Keeping the helper at the root keeps Phase 2 self-
// contained.
//
// The shimMsg struct moves here with readLoop because it is only used
// inside readLoop and by wrapper.go's Spawn handshake (same package,
// no import change needed).

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
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
			if ev.Type == "system" && ev.SubType == "init" && p.caps.Replay {
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
					continue
				}
				// No owners claim this result. Under passthrough this means
				// either (a) an abort with no claimed slots, handled above,
				// or (b) stray result during reconnect. Either way skip
				// legacy eventCh; dashboard EventLog already has the entry.
				if ev.SubType == "error_during_execution" {
					p.logEventAt(ev, nowMS)
					continue
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
			if p.linker != nil {
				if ev.Type == "system" && ev.SubType == "init" && ev.SessionID != "" {
					projectDir := resolveProjectDir(p.cwd)
					p.linker.SetContext(projectDir, ev.SessionID)
				}
				// Trigger Resolve for BOTH in-process teammates (TeamCreate's
				// Agent spawns; task_type="in_process_teammate") AND standalone
				// sub-agents (Task(subagent_type=...); task_type often empty
				// or vendor-specific). Both write subagents/agent-<task_id>.
				// jsonl, so the linker's fast path (stat by task_id) is the
				// right common denominator. Exclude local_bash — those only
				// persist to tool-results/ and have no internal transcript.
				if ev.Type == "system" && ev.SubType == "task_started" &&
					ev.TaskType != "local_bash" && ev.TaskID != "" && ev.ToolUseID != "" {
					taskID := ev.TaskID
					toolUseID := ev.ToolUseID
					name := ev.Description
					if nameTrim := strings.TrimSpace(name); nameTrim != "" {
						// task_started.description is "<name>: <prompt body>"
						// for teammates; for sub-agents it's just the prompt.
						// The linker's fast path works either way; trimming
						// to the name prefix only helps the name-scan fallback.
						if idx := strings.IndexByte(nameTrim, ':'); idx > 0 {
							name = strings.TrimSpace(nameTrim[:idx])
						} else {
							name = nameTrim
						}
					}
					linker := p.linker
					go linker.Resolve(taskID, toolUseID, name, ev.Description, nowMS)
				}
			}

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
				// Unblock any passthrough SendPassthrough callers immediately.
				// The defer at readLoop end also calls discardAllPending, but
				// that runs after we drain any remaining stdin frames — a kill
				// race with active slots would otherwise wait for the outer
				// loop to fully unwind (tens of ms under load).
				p.discardAllPending(ErrProcessExited)
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
				// Full buffer: drop is safe (EventLog kept the entry) but
				// dropping a `result` event forces Send() into the
				// findResultSince fallback, so log at Warn for observability.
				if ev.Type == "result" {
					log.Warn("eventCh full, dropped result", "subtype", ev.SubType)
				} else {
					log.Debug("eventCh full, dropped", "type", ev.Type)
				}
			}

		case "stderr":
			log.Debug("cli stderr", "line", sanitizeStderrLine(msg.Line))

		case "cli_exited":
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
			// closeShimConn is sync.Once-guarded so a later Kill/Detach is safe
			// without producing a "use of closed network connection" debug log
			// on the second close attempt. R219-GO-3.
			p.closeShimConn()
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
			// made Timer.Stop/Reset self-draining at the runtime level, so the
			// historical `if !Stop() { <-C }` dance is redundant on this
			// toolchain. We still call Stop() to release the pending tick
			// immediately rather than waiting for GC.
			//
			// LOCKED to toolchain ≥1.23: go.mod sets `go 1.26.3` so the runtime
			// auto-drain is guaranteed. Down-revving go.mod below 1.23 would
			// reintroduce a stale-tick path where Reset returns without first
			// consuming the previous fire — recreate the explicit drain dance
			// before lowering the toolchain. R222-GO-4.
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
