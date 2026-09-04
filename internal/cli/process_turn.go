package cli

// process_turn.go — turn-boundary helpers: findResultSince (EventLog fallback
// for Send), drainStaleEvents (settle-window guard at the top of every turn),
// isChanAlive (send-on-closed-eventCh guard) and sanitizeStderrLine.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/osutil"
)

// stderrSanitizeBuilderPool reuses strings.Builders across sanitizeStderrLine
// slow-path calls (#1015); b.String() copies, so returning to the pool is safe.
var stderrSanitizeBuilderPool = sync.Pool{
	New: func() any { return &strings.Builder{} },
}

// interruptedSettleWindow caps how long a fresh Send waits for the interrupted
// previous turn's result to flush: long enough for an in-flight result to drain,
// short enough that the new prompt isn't perceptibly delayed.
const interruptedSettleWindow = 500 * time.Millisecond

// findResultSince checks EventLog for a result entry logged after afterMS; the
// fallback when eventCh may have dropped events. The "result" entry carries
// only cost + turn metadata (its Detail is empty to avoid a duplicate dashboard
// bubble), so the reply text is recovered from the preceding "text" entry (#1805).
func (p *Process) findResultSince(afterMS int64) *SendResult {
	entries := p.eventLog.EntriesSince(afterMS)
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type != "result" {
			continue
		}
		text := entries[i].Detail
		if text == "" {
			// Detail holds the full text; Summary is only the truncated preview.
			text = lastTextEntryBefore(entries, i)
		}
		return &SendResult{
			Text:      text,
			SessionID: p.SessionID(),
			CostUSD:   entries[i].Cost,
		}
	}
	return nil
}

// lastTextEntryBefore returns the Detail of the most recent assistant "text"
// entry at an index < resultIdx, or "" when the turn produced no text entry.
func lastTextEntryBefore(entries []clievent.EventEntry, resultIdx int) string {
	for j := resultIdx - 1; j >= 0; j-- {
		if entries[j].Type == "text" && entries[j].Detail != "" {
			return entries[j].Detail
		}
	}
	return ""
}

// drainStaleEvents clears residual events from previous turns; after an
// Interrupt() it waits briefly for the interrupted result so it doesn't pollute
// the next turn. Only events whose arrival predates the entry-time cutoff are
// drained: readLoop may concurrently push a fresh event for the *new* turn, and
// swallowing it would force Send into the findResultSince fallback.
func (p *Process) drainStaleEvents(ctx context.Context) error {
	cutoff := time.Now()
	// Read-and-clear both flags under p.mu, which Interrupt() holds while storing
	// them; two unlocked Swaps could lose a concurrent Interrupt, skipping the
	// settle window so the SIGINT result leaks into the next turn.
	p.mu.Lock()
	wasInterrupted := p.interrupted.Swap(false)
	wasRunning := p.interruptedRun.Swap(false)
	p.mu.Unlock()

	// Stack-allocated [4]Event backing: post-cutoff events during an interrupt
	// are rare (0-3), so the common case appends without heap allocation.
	var holdbackArr [4]Event
	holdback := holdbackArr[:0]

	if wasInterrupted {
		// An idle process produces no result event, so the settle timer would
		// always expire; only wait when a turn was actually running.
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
						// Post-cutoff event belongs to the *new* turn: hold it aside
						// (re-enqueued at the end of drain) and KEEP waiting for the
						// interrupted result — bailing now would let that result
						// arrive after the sweep and leak into the new turn (#773).
						holdback = append(holdback, ev)
						continue
					}
					// Pre-cutoff non-result event: drained, keep waiting for the result.
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
	// Non-blocking drain of remaining pre-cutoff events; post-cutoff events are
	// held back and re-enqueued at the end. Returning at the first post-cutoff
	// event would leave interleaved pre-cutoff stragglers to surface in the new
	// turn as phantom events.
	for {
		select {
		case <-ctx.Done():
			// Re-enqueue held events. Guard on p.done: a send on a closed channel
			// panics even with a `default` arm. EventLog is authoritative, so
			// dropping holdback when eventCh is torn down is safe.
			if isChanAlive(p.done) {
				for _, ev := range holdback {
					p.safeReenqueue(ev)
				}
			}
			return ctx.Err()
		case ev, ok := <-p.eventCh:
			if !ok {
				// Process exited. holdback events are already in EventLog, so Send()
				// recovers via findResultSince(); dropping them is safe.
				return nil
			}
			if ev.recvAt.After(cutoff) {
				holdback = append(holdback, ev)
			}
			// pre-cutoff events are dropped (drained)
		default:
			// Channel empty — push back held events (same closed guard as above).
			if !isChanAlive(p.done) {
				return nil
			}
			for _, ev := range holdback {
				p.safeReenqueue(ev)
			}
			return nil
		}
	}
}

// safeReenqueue pushes a held-back post-cutoff event back onto p.eventCh,
// non-blocking. The caller's isChanAlive(p.done) check and this send are not
// atomic — readLoop may close `done` then `eventCh` in between (#1779) and a
// send on a closed channel panics even with `default` — so the send is wrapped
// in recover; the event is already in EventLog, so nothing is lost.
func (p *Process) safeReenqueue(ev Event) {
	defer func() {
		if r := recover(); r != nil {
			// eventCh closed between the isChanAlive guard and this send.
			slog.Debug("drainStaleEvents: re-enqueue raced eventCh close, dropped",
				"type", ev.Type, "session", ev.SessionID)
		}
	}()
	select {
	case p.eventCh <- ev:
	default:
		// findResultSince recovers the result from EventLog, but surface the
		// drop so operators can enlarge the channel if it persists under load.
		slog.Warn("drainStaleEvents: eventCh full, dropped fresh event",
			"type", ev.Type, "session", ev.SessionID)
	}
}

// isChanAlive reports whether done is still open, hence eventCh is safe to send
// on: readLoop's defer chain closes `done` strictly BEFORE `eventCh`, so an open
// `done` implies an open `eventCh`.
func isChanAlive(done <-chan struct{}) bool {
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// maxStderrLogLineBytes caps stderr log lines so a runaway CLI stderr cannot
// fill journald with a single multi-MB message.
const maxStderrLogLineBytes = 500

// sanitizeStderrLine removes ANSI escape sequences (SGR, cursor movement,
// OSC/DCS) and truncates the stderr line so log viewers aren't colorized or
// repositioned by CLI output and one line cannot flood the journal.
func sanitizeStderrLine(line string) string {
	if line == "" {
		return line
	}
	// Pre-truncate before the ANSI scanner so a pathological multi-MB OSC
	// sequence (no BEL/ST) doesn't allocate a full-length Builder just to be
	// truncated afterward.
	if len(line) > maxStderrLogLineBytes {
		cut := maxStderrLogLineBytes
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut] + "…(truncated)"
	}
	// Fast path: plain ASCII log text returns without a Builder copy. Any
	// non-ASCII byte takes the slow path so C1/bidi/LS/PS codepoints are dropped
	// — a compromised CLI emitting bidi overrides could otherwise reverse
	// operator journalctl output.
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
	b := stderrSanitizeBuilderPool.Get().(*strings.Builder)
	b.Reset()
	b.Grow(len(line))
	defer stderrSanitizeBuilderPool.Put(b)
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
		// Non-ASCII: drop known log-injection runes (C1, bidi overrides/isolates,
		// LS/PS) inline rather than via a second, always-allocating strings.Map.
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
	// The sanitizer only removes bytes from the pre-truncated input, so the
	// result is never longer than maxStderrLogLineBytes plus the marker.
	return b.String()
}
