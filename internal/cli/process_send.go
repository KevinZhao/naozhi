package cli

// process_send.go — user-message outbound path and CLI-level interrupts.
// EventCallback is consumed cross-package (session, dispatch, server); changing
// its signature is breaking. findResultSince / drainStaleEvents: process_turn.go.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/textutil"
)

// EventCallback is called for each intermediate event during Send.
type EventCallback func(ev Event)

// buildUserEntry renders the EventLog entry for a single user message. Shared
// by Send and SendPassthrough: readLoop filters the CLI's replay echo out of
// EventLog, so both paths must append the bubble explicitly.
func buildUserEntry(text string, images []Attachment) clievent.EventEntry {
	entry := clievent.EventEntry{
		Time:    time.Now().UnixMilli(),
		Type:    "user",
		Summary: textutil.TruncateRunes(text, 120),
		Detail:  textutil.TruncateRunes(text, EventDetailMaxRunes),
	}
	if len(images) > 0 {
		entry.Summary += " [+" + strconv.Itoa(len(images)) + " image(s)]"
		thumbs := make([]string, len(images))
		if len(images) == 1 {
			thumbs[0] = MakeThumbnail(images[0].Data, 600)
		} else {
			// Bounded pool: MakeThumbnail's thumbSem already serialises the work, so
			// more than thumbnailWorkerCap goroutines would just block on it (#569).
			workerCount := len(images)
			if workerCount > thumbnailWorkerCap {
				workerCount = thumbnailWorkerCap
			}
			var wg sync.WaitGroup
			wg.Add(workerCount)
			jobs := make(chan int, len(images))
			for w := 0; w < workerCount; w++ {
				go func() {
					defer wg.Done()
					for i := range jobs {
						thumbs[i] = MakeThumbnail(images[i].Data, 600)
					}
				}()
			}
			for i := range images {
				jobs <- i
			}
			close(jobs)
			wg.Wait()
		}
		// ImagePaths lets the dashboard offer "view original" without full-size
		// base64. Emit (thumb, path) pairs only for thumbs that survive sanitize,
		// preserving the index-alignment contract on EventEntry.ImagePaths.
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
// onEvent fires only for assistant events carrying a "thinking" or "tool_use"
// block — not text deltas or ACP tool_call_update progress — so treat it as a
// tool-activity heartbeat, not "new content". Full-stream consumers use
// EventLog.Subscribe; Send logs every event under the same lock, nothing is lost.
func (p *Process) Send(ctx context.Context, text string, images []Attachment, onEvent EventCallback) (*SendResult, error) {
	p.mu.Lock()
	if p.state == StateRunning {
		p.mu.Unlock()
		return nil, fmt.Errorf("process busy (state=%s): %w", p.state, ErrProcessBusy)
	}
	p.state = StateRunning
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		if p.state == StateRunning {
			p.state = StateReady
		}
		p.mu.Unlock()
	}()

	// Drain stale events from a previous turn that completed with no Send()
	// active (readLoop already logged them). After a SIGINT the CLI may still be
	// producing the interrupted result, so wait briefly for it.
	if err := p.drainStaleEvents(ctx); err != nil {
		return nil, err
	}

	// Downscale oversized images once, before the write, so the CLI payload and
	// the dashboard bubble (buildUserEntry) share the same bytes. Best-effort:
	// undecodable or already-small images pass through unchanged.
	images = downscaleImagesForVision(images)

	// Turn start for the EventLog fallback when eventCh drops events.
	turnStartMS := time.Now().UnixMilli()

	if err := p.protocol.WriteMessage(p.shimStdinWriter(), text, images); err != nil {
		return nil, fmt.Errorf("write message: %w", err)
	}

	// Log the user message AFTER a successful write so a rejected write leaves
	// no ghost entry; passthrough.go orders the same way.
	p.eventLog.Append(buildUserEntry(text, images))

	noOutputDur := p.noOutputTimeout
	if noOutputDur <= 0 {
		noOutputDur = DefaultNoOutputTimeout
	}
	totalDur := p.totalTimeout
	if totalDur <= 0 {
		totalDur = DefaultTotalTimeout
	}

	// Watchdog: one periodic timer instead of per-event Stop/drain/Reset. The
	// interval caps timeout precision, fine for minute-scale timeouts. Re-armed
	// after each fire; Stop()+drain on early return via defer.
	checkInterval := noOutputDur / 4
	if checkInterval < time.Second {
		checkInterval = time.Second
	}
	if checkInterval > 30*time.Second {
		checkInterval = 30 * time.Second
	}
	turnStart := time.Now()
	lastOutput := turnStart
	watchdog := time.NewTimer(checkInterval)
	defer func() {
		// Drain if the timer fired undrained so leak detectors see a clean state.
		if !watchdog.Stop() {
			select {
			case <-watchdog.C:
			default:
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			// Don't Kill the CLI: during graceful shutdown router.Shutdown calls
			// Detach() to keep the shim alive for zero-downtime restart. readLoop
			// detects the disconnect and closes eventCh, hitting the !ok branch.
			return nil, ctx.Err()
		case ev, ok := <-p.eventCh:
			if !ok {
				// eventCh closed — process exited. Fall back to EventLog for a result
				// readLoop logged but eventCh dropped or delivered too late.
				if sr := p.findResultSince(turnStartMS); sr != nil {
					return sr, nil
				}
				return nil, ErrProcessExited
			}

			lastOutput = time.Now()

			// Capture session ID from init; always overwrite when non-empty because
			// --resume emits a fresh init with the forked session_id and a
			// first-non-empty guard would pin the stale one (#393).
			if ev.Type == "system" && ev.SubType == "init" {
				p.mu.Lock()
				if ev.SessionID != "" {
					p.sessionID = ev.SessionID
				}
				p.mu.Unlock()
				// system/init advertises the resolved model; SpawnOptions.Model is empty
				// for claude, so this is authoritative. Only overwrite when present.
				if ev.Model != "" {
					p.setModel(ev.Model)
				}
				// Mirror readLoop's hook in case Send drains the init frame first.
				// First writer wins; identical value.
				if ev.ClaudeCodeVersion != "" {
					p.setLiveVersion(ev.ClaudeCodeVersion)
				}
				continue
			}

			// Already logged to EventLog by readLoop; only fan out here.
			if onEvent != nil && ev.Type == "assistant" && ev.Message != nil {
				for _, block := range ev.Message.Content {
					if block.Type == "thinking" || block.Type == "tool_use" {
						onEvent(ev)
						break
					}
				}
			}

			if ev.Type == "result" {
				// Mirror the init-path fix: accept any non-empty session_id so
				// --resume's new ID is recorded even if init was missed (#393).
				p.mu.Lock()
				if ev.SessionID != "" {
					p.sessionID = ev.SessionID
				}
				p.mu.Unlock()
				return &SendResult{
					Text:      ev.Result,
					SessionID: ev.SessionID,
					CostUSD:   ev.CostUSD,
				}, nil
			}
		case <-watchdog.C:
			sr, err := p.handleWatchdogTick(time.Now(), lastOutput, turnStart, turnStartMS, noOutputDur, totalDur)
			if sr != nil || err != nil {
				return sr, err
			}
			// Re-arm: C was just drained, so Reset on the expired timer is safe.
			watchdog.Reset(checkInterval)
		}
	}
}

// clearInflightFlags resets the interrupt atomics after a watchdog kill: a
// leftover flag would make a future Send on a recycled Process burn the 500ms
// settle window for a result that never arrives (#770). Held under p.mu to
// match Interrupt() / InterruptViaControl(), so a concurrent Interrupt and a
// watchdog kill cannot race the flags into a torn state.
func (p *Process) clearInflightFlags() {
	p.mu.Lock()
	p.interrupted.Store(false)
	p.interruptedRun.Store(false)
	p.mu.Unlock()
}

// handleWatchdogTick evaluates the no-output and total-turn deadlines for one
// watchdog wakeup. Returns (sr, nil) when a deadline elapsed but a result
// already landed in EventLog (eventCh dropped it); (nil, err) when it elapsed
// with no fallback (Kill() already issued); (nil, nil) when neither fired and
// the caller should re-arm.
func (p *Process) handleWatchdogTick(
	now, lastOutput, turnStart time.Time,
	turnStartMS int64,
	noOutputDur, totalDur time.Duration,
) (*SendResult, error) {
	if now.Sub(lastOutput) >= noOutputDur {
		if sr := p.findResultSince(turnStartMS); sr != nil {
			return sr, nil
		}
		// Set death reason BEFORE Kill so readLoop's shim_eof/shim_read_error
		// classification (triggered by shimConn.Close) cannot overwrite the true
		// root cause; setDeathReason is first-writer-wins.
		p.setDeathReason(DeathReasonNoOutputTimeout)
		p.slogger().Error("watchdog: no output timeout", "timeout", noOutputDur)
		p.Kill()
		// Clear inflight settle flags so drainStaleEvents' 500ms wait cannot
		// fire against a watchdog-killed process (#770; see clearInflightFlags).
		p.clearInflightFlags()
		return nil, fmt.Errorf("%w (%s)", ErrNoOutputTimeout, noOutputDur)
	}
	if now.Sub(turnStart) >= totalDur {
		if sr := p.findResultSince(turnStartMS); sr != nil {
			return sr, nil
		}
		p.setDeathReason(DeathReasonTotalTimeout)
		p.slogger().Error("watchdog: total timeout", "timeout", totalDur)
		p.Kill()
		p.clearInflightFlags()
		return nil, fmt.Errorf("%w (%s)", ErrTotalTimeout, totalDur)
	}
	return nil, nil
}

// Interrupt sends SIGINT to the CLI process via shim.
func (p *Process) Interrupt() {
	if !p.Alive() {
		return
	}
	// Store the atomics under p.mu so Send()'s State→Running transition (also
	// under p.mu) serialises with us; otherwise interrupted=true could land with
	// interruptedRun=false and the interrupted result would leak into the next turn.
	p.mu.Lock()
	state := p.state
	p.interrupted.Store(true)
	if state == StateRunning {
		p.interruptedRun.Store(true)
	}
	p.mu.Unlock()
	// While spawning the CLI's REPL isn't up and silently drops SIGINT: skip the
	// wire send and leave interruptedRun unset so drainStaleEvents does not enter
	// the settle loop (there is no stale result to absorb).
	if state == StateSpawning {
		return
	}
	if err := p.shimSend(shimClientMsg{Type: "interrupt"}); err != nil {
		slog.Warn("interrupt failed", "err", err)
	}
}

// InterruptViaControl aborts the active turn via an in-band control_request
// on stdin (stream-json only) — no SIGINT, no shim interrupt command. The CLI
// (verified on 2.1.119) kills in-flight tools within ~300ms, emits a `result`,
// and the session stays usable. Returns nil (written; next Send drains the
// result), ErrNoActiveTurn (idle; nothing written, no flags set),
// ErrInterruptUnsupported (fall back to Interrupt()), or a wrapped transport
// error (flags rolled back so the next Send does not burn the settle budget).
func (p *Process) InterruptViaControl() error {
	if !p.Alive() {
		return ErrNoActiveTurn
	}
	// Snapshot state and pre-commit the atomics under p.mu so a concurrent Send()
	// flipping State to Running cannot race us into "wrote control_request but
	// skipped the settle flags". CompareAndSwap records which flags WE set, so a
	// write-failure rollback cannot clobber a concurrent Interrupt()'s flags.
	var iSet, rSet bool
	p.mu.Lock()
	state := p.state
	if state == StateRunning {
		iSet = p.interrupted.CompareAndSwap(false, true)
		rSet = p.interruptedRun.CompareAndSwap(false, true)
	}
	p.mu.Unlock()
	// Do NOT write the control_request when idle: the CLI would buffer it for
	// the next turn and produce a spurious control_response against a turn the
	// caller never intended to cancel.
	if state != StateRunning {
		return ErrNoActiveTurn
	}
	reqID := "naozhi-int-" + strconv.FormatInt(p.interruptSeq.Add(1), 10)
	if err := p.protocol.WriteInterrupt(p.shimStdinWriter(), reqID); err != nil {
		// Nothing reached the CLI, so no trailing result to drain. Roll back ONLY
		// the flags we CAS'd — a concurrent Interrupt() that won owns its flag.
		if iSet {
			p.interrupted.Store(false)
		}
		if rSet {
			p.interruptedRun.Store(false)
		}
		return fmt.Errorf("write interrupt control_request: %w", err)
	}
	return nil
}

// setModelAckTimeout caps how long SetModel waits for the CLI's ack. claude
// processes control requests in streaming gaps — a mid-turn ack took 7.5s on
// 2.1.251 (docs/rfc/dashboard-model-effort-control.md §1 F14) — so anything
// single-digit risks false timeouts; 30s matches acpHandshakeTimeout's scale.
const setModelAckTimeout = 30 * time.Second

// registerControlAck installs an ack waiter for a control request_id. The
// channel receives nil (success) or an error carrying the CLI's rejection
// text; it is buffered so a delivery racing the timeout never blocks readLoop.
func (p *Process) registerControlAck(reqID string) chan error {
	ch := make(chan error, 1)
	p.controlAckMu.Lock()
	if p.controlAcks == nil {
		p.controlAcks = make(map[string]chan error, 1)
	}
	p.controlAcks[reqID] = ch
	p.controlAckMu.Unlock()
	return ch
}

// unregisterControlAck removes an ack waiter (deferred by SetModel so a
// late/never ack cannot leak the map entry).
func (p *Process) unregisterControlAck(reqID string) {
	p.controlAckMu.Lock()
	delete(p.controlAcks, reqID)
	p.controlAckMu.Unlock()
}

// deliverControlAck routes a control_ack Event from readLoop to its waiter.
// Unmatched acks (waiter timed out, or an interrupt's control_response —
// those never register) are dropped: fire-and-forget, no turn state.
func (p *Process) deliverControlAck(ev Event) {
	p.controlAckMu.Lock()
	ch, ok := p.controlAcks[ev.RPCRequestID]
	if ok {
		delete(p.controlAcks, ev.RPCRequestID)
	}
	p.controlAckMu.Unlock()
	if !ok {
		return
	}
	if ev.SubType == "error" {
		// ev.Result was sanitized at the protocol layer (parseControlAck /
		// ACP interception) — safe for slog + dashboard toast.
		ch <- fmt.Errorf("%w: %s", ErrSetModelRejected, ev.Result)
		return
	}
	ch <- nil
}

// ErrSetModelRejected wraps a CLI-side rejection of set_model (org policy or
// unknown model). The wrapped text is the CLI's own message; callers surface it
// verbatim and MUST NOT record the override (RFC §6 R8: ack-before-persist).
var ErrSetModelRejected = errors.New("set_model rejected by CLI")

// SetModel switches the live session's model in place without restarting the
// CLI (protocol mapping: ModelSetter godoc). It registers an ack waiter under a
// fresh request_id, writes the request, and blocks until ack / process death /
// ctx / timeout. Safe mid-turn. Returns nil on ack (kiro's RPC {} carries no
// model echo, so ack == confirmation), ErrSetModelRejected-wrapped when the CLI
// refused (caller must not persist), ErrSetModelUnsupported when the protocol
// has no runtime channel (record-only), or a transport/death/timeout error.
// Policy-free: kiro's set_model resets the effort tier; the Router handles that.
func (p *Process) SetModel(ctx context.Context, model string) error {
	ms, ok := p.protocol.(ModelSetter)
	if !ok {
		return ErrSetModelUnsupported
	}
	if !p.Alive() {
		return fmt.Errorf("set_model: process not alive")
	}
	reqID := "naozhi-setmodel-" + strconv.FormatInt(p.interruptSeq.Add(1), 10)
	ch := p.registerControlAck(reqID)
	defer p.unregisterControlAck(reqID)
	if err := ms.WriteSetModel(p.shimStdinWriter(), reqID, model); err != nil {
		return err
	}
	timer := time.NewTimer(setModelAckTimeout)
	defer timer.Stop()
	select {
	case err := <-ch:
		if err == nil {
			// Neither backend echoes the model on ack (claude's live value only refreshes
			// on the next system/init), so update the live-process mirror here; Snapshot
			// prefers proc.Model() over the persisted session field.
			p.setModel(model)
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-p.killCh:
		return fmt.Errorf("set_model: process terminated while awaiting ack")
	case <-p.done:
		// Natural exit (EOF → readLoop returned) never closes killCh; without this
		// arm the ack wait would sit out the full timeout against a dead process.
		return fmt.Errorf("set_model: process exited while awaiting ack")
	case <-timer.C:
		return fmt.Errorf("set_model: no ack within %s", setModelAckTimeout)
	}
}
