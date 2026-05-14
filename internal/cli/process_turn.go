package cli

// process_turn.go — turn-boundary coordination helpers.
//
// Moved from process.go (Phase 4 of docs/rfc/process-split.md).
// Zero semantic change; pure file move.
//
// This file owns:
//   - findResultSince: EventLog fallback scanner used by Send when
//     eventCh drops or closes before a result is delivered; also used
//     by drainStaleEvents' timeout branches.
//   - drainStaleEvents: settle-window guard that runs at the top of
//     every Send turn to absorb any interrupted-turn residue before
//     the new prompt is written.
//   - isChanAlive: tiny invariant helper guarding against send-on-
//     closed-eventCh panics; relies on readLoop's defer ordering.
//   - sanitizeStderrLine + maxStderrLogLineBytes: ANSI / log-injection
//     scrubber for CLI stderr, only referenced from readLoop but kept
//     here to keep the turn-file's "log hygiene" job on one surface.

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
)

// interruptedSettleWindow caps how long a fresh Send waits for the previous
// turn's result event to flush after Interrupt() was called. 500 ms balances
// "long enough that an in-flight result still drains" against "short enough
// that the new prompt isn't perceptibly delayed". Round-tested across
// multiple review rounds; change with care and re-run drainStaleEvents tests.
const interruptedSettleWindow = 500 * time.Millisecond

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
			settle := time.NewTimer(interruptedSettleWindow)
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
// eventCh remains safe to send on). Invariant used here: readLoop closes
// `done` strictly BEFORE `eventCh` on exit — so if `done` is still open,
// `eventCh` is also still open. See process_readloop.go for the defer chain
// that establishes this ordering.
func isChanAlive(done <-chan struct{}) bool {
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// maxStderrLogLineBytes caps stderr log lines so a runaway CLI stderr
// cannot fill journald with a single multi-MB message. Used only by
// sanitizeStderrLine below.
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
		// Drop bare ASCII C0 control chars (keep \t).
		if c < 0x20 && c != '\t' {
			i++
			continue
		}
		// Non-ASCII: decode rune and drop if it's a known log-injection
		// codepoint (C1 controls, bidi overrides/isolates, LS/PS). Folding
		// this into the byte-level loop avoids the second strings.Map pass
		// which always allocates a fresh backing string even on a no-op.
		if c >= 0x80 {
			r, sz := utf8.DecodeRuneInString(line[i:])
			if osutil.IsLogInjectionRune(r) {
				i += sz
				continue
			}
			b.WriteString(line[i : i+sz])
			i += sz
			continue
		}
		b.WriteByte(c)
		i++
	}
	// The pre-truncation step above already capped the input length; the
	// sanitizer only removes bytes from that capped input (ANSI escapes +
	// control chars + log-injection runes), so the resulting builder is
	// guaranteed to be no longer than the pre-truncated input.
	return b.String()
}
