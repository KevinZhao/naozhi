package cli

// process_send.go — user-message outbound path and CLI-level interrupts.
//
// This file owns:
//   - EventCallback type (cross-package public; consumed by
//     cli.SendPassthrough, session.managed.Send/SendPassthrough,
//     session.testutil.TestProcess, dispatch, server — changing its
//     signature is breaking across 4 packages).
//   - buildUserEntry (also called by cli.passthrough.go).
//   - Send — legacy non-passthrough outbound.
//   - Interrupt / InterruptViaControl — turn-abort primitives.
//
// findResultSince and drainStaleEvents live in process_turn.go.
//
// R227-ARCH-19: dropped the "Phase 3 of process-split / zero semantic
// change" preamble; refactor is complete, history lives in git log.

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/textutil"
)

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
		Summary: textutil.TruncateRunes(text, 120),
		Detail:  textutil.TruncateRunes(text, eventDetailMaxRunes),
	}
	if len(images) > 0 {
		entry.Summary += " [+" + strconv.Itoa(len(images)) + " image(s)]"
		thumbs := make([]string, len(images))
		if len(images) == 1 {
			thumbs[0] = MakeThumbnail(images[0].Data, DefaultThumbnailMaxDim)
		} else {
			// R225-PERF-15: cap concurrent JPEG-encode goroutines so a single
			// 20-image upload cannot saturate every CPU at once and starve
			// other sessions' Send paths. 4 keeps small-batch latency near
			// the unbounded baseline (image/jpeg encode is ~10-30 ms each)
			// while bounding worst-case CPU use. Distinct from
			// thumbDecodeConcurrency (thumbnail.go's image decode semaphore):
			// this gates per-message goroutine fan-out within a single Send,
			// while the package-level decoder semaphore bounds total RAM
			// across simultaneous Sends. Two layers, two purposes.
			const thumbnailConcurrency = 4
			sem := make(chan struct{}, thumbnailConcurrency)
			var wg sync.WaitGroup
			for i, img := range images {
				wg.Add(1)
				sem <- struct{}{}
				go func(i int, data []byte) {
					defer wg.Done()
					defer func() { <-sem }()
					thumbs[i] = MakeThumbnail(data, DefaultThumbnailMaxDim)
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
//
// onEvent semantics (R229-GO-3): the callback fires only for assistant events
// whose Message.Content contains at least one "thinking" or "tool_use" block.
// Plain assistant text deltas (block.Type=="text") and ACP tool_call_update
// progress events (ev.SubType=="tool_result", ev.Message==nil) do NOT trigger
// it, so callers driving streaming-progress UIs (cron status updates, upstream
// /api/sessions/{key}/progress, dashboard interim bubbles) MUST treat onEvent
// as "long-running tool activity heartbeat", not "any new content arrived".
// Subscribers that need the full event stream should attach via EventLog.Subscribe
// instead — Send writes every event to the log under the same lock that this
// callback fires from, so no events are lost.
func (p *Process) Send(ctx context.Context, text string, images []ImageData, onEvent EventCallback) (*SendResult, error) {
	p.mu.Lock()
	if p.State == StateRunning {
		p.mu.Unlock()
		return nil, fmt.Errorf("process busy (state=%s): %w", p.State, ErrProcessBusy)
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
				// UI Round 5 R5-3: claude advertises the resolved model
				// in system/init (e.g.
				// "global.anthropic.claude-opus-4-7[1m]"). Spawn-time
				// SpawnOptions.Model is empty for claude (CLI resolves
				// from env / defaults internally), so this is the
				// authoritative source. Only overwrite when init event
				// actually has a value — guards against future CLI
				// versions dropping the field.
				if ev.Model != "" {
					p.setModel(ev.Model)
				}
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
