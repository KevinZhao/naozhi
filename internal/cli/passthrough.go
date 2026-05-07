package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"
)

// newSlotUUID returns a 128-bit random hex string suitable for the Claude
// CLI's uuid field. We don't need RFC4122 formatting — CLI treats it as an
// opaque round-trippable blob. Using crypto/rand avoids a new dep (google/uuid)
// while still giving enough entropy that collisions are astronomically rare.
func newSlotUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// SendPassthrough writes a user message to the CLI in passthrough mode and
// waits for the matching turn result. Multiple goroutines can call this
// concurrently on the same Process — ordering is preserved by atomically
// appending the sendSlot and writing stdin under a single lock.
//
// Unlike the legacy Send, SendPassthrough does not hold any "process busy"
// flag: the CLI's own commandQueue handles queuing. naozhi only does the
// uuid/text ↔ slot bookkeeping.
//
// Passthrough requires Protocol.SupportsReplay(); callers must check that
// upfront and fall back to Send when false. Calling SendPassthrough on a
// replay-less protocol would hang because no replay events arrive to claim
// the slot.
//
// priority: "" | "now" | "next" | "later". "now" aborts the in-flight turn.
func (p *Process) SendPassthrough(ctx context.Context, text string, images []ImageData,
	onEvent EventCallback, priority string) (*SendResult, error) {

	if !p.protocol.SupportsReplay() {
		return nil, fmt.Errorf("passthrough: protocol %s does not support replay", p.protocol.Name())
	}

	// Fast reject: dead process won't produce a result.
	if !p.Alive() {
		return nil, ErrProcessExited
	}

	slot := &sendSlot{
		id:        p.slotIDGen.Add(1),
		uuid:      newSlotUUID(),
		text:      text,
		priority:  priority,
		onEvent:   onEvent,
		resultCh:  make(chan *SendResult, 1),
		errCh:     make(chan error, 1),
		enqueueAt: time.Now(),
	}

	// Lock order: shimWMu → slotsMu. This is the only place we take both.
	// Holding shimWMu across slotsMu+append+write ensures the slot lands in
	// pendingSlots in the exact order the NDJSON line hits the shim socket.
	// Without this, two concurrent Send calls could see the inverse order
	// (slot A queued first, but stdin B written first) and break FIFO-based
	// turn-result attribution.
	p.shimWMu.Lock()
	p.slotsMu.Lock()

	if len(p.pendingSlots) >= maxPendingSlots {
		p.slotsMu.Unlock()
		p.shimWMu.Unlock()
		return nil, ErrTooManyPending
	}
	p.pendingSlots = append(p.pendingSlots, slot)
	p.slotsMu.Unlock()

	// shimSendLocked equivalent for user messages: caller already holds
	// shimWMu. We pass stdinWriter but cannot use the fast path — we need
	// to push the raw NDJSON directly through the shim protocol so the shim
	// sees one atomic "write" frame. The underlying shimWriter fast-path in
	// shimStdinWriter already does this (single-line atomic shimSend).
	// However, that path would re-acquire shimWMu via shimSend → dead-lock.
	// We use WriteUserMessageLocked which is documented to skip the outer
	// lock (the Protocol guarantees atomic write under already-held shimWMu).
	//
	// Concretely: our shimWriter.Write detects a single-line '\n'-terminated
	// buffer and calls p.shimSend which takes shimWMu — that would dead-lock
	// us. Workaround: write directly via a thin helper that reuses
	// shimSendLocked.
	writeErr := p.writeUserMessageUnderShimLock(slot.uuid, text, images, priority)
	slot.writtenAt = time.Now()
	p.shimWMu.Unlock()

	if writeErr != nil {
		// CLI never saw this message — remove slot now; caller gets the raw
		// error. FIFO is not harmed because we wrote nothing to stdin.
		p.removeSlotByID(slot.id)
		// If the write failed because the process died between Alive()
		// check and shim write, surface ErrProcessExited so upstream
		// sees the canonical "process gone" signal rather than an opaque
		// shim-pipe error.
		if !p.Alive() {
			return nil, ErrProcessExited
		}
		return nil, fmt.Errorf("passthrough write: %w", writeErr)
	}

	// Mirror Send's EventLog.Append for the user message so a later
	// subscribe (session switch, reconnect) can re-render the bubble.
	// readLoop filters the CLI's replay echo out of EventLog to avoid
	// double-display against the dashboard's optimistic bubble, so without
	// this append the user turn only lives in the client DOM and the JSONL
	// on disk — not in naozhi's in-memory transcript. Placed after the
	// successful stdin write so a rejected write (e.g. closed shim socket)
	// does not leave a ghost entry; subscribers already poll after any
	// Append via the live notify-subscribers path.
	p.eventLog.Append(buildUserEntry(text, images))

	// Defensive bail timer. Passthrough does not have a per-turn watchdog
	// (CLI 本身和 shim 的 heartbeat 负责探测进程级死锁；slot 级超时由 bail
	// 兜底)。Set to totalTimeout + 30s so in the rare case where readLoop
	// and shim heartbeat both miss, the Send caller still unblocks.
	//
	// Phase A.8 reflection: v2.2 RFC specified a process-level watchdog
	// keyed to turnStartedAt. In practice the CLI's heartbeat + cli_exited
	// path (discardAllPending) covers hard dead-process scenarios, and
	// shim's own watchdog covers hung-CLI scenarios. The per-slot bail is
	// sufficient defensive cover; we add a process-level loop only if
	// Phase C testing shows stuck slots in the wild.
	total := p.totalTimeout
	if total <= 0 {
		total = DefaultTotalTimeout
	}
	bail := time.NewTimer(total + 30*time.Second)
	defer bail.Stop()

	select {
	case res := <-slot.resultCh:
		return res, nil
	case err := <-slot.errCh:
		return nil, err
	case <-ctx.Done():
		// Tombstone: keep slot in pendingSlots so FIFO positioning survives.
		// When the real result arrives, fanout sees canceled=true and drops.
		p.slotsMu.Lock()
		slot.canceled = true
		p.slotsMu.Unlock()
		return nil, ctx.Err()
	case <-bail.C:
		// Pure defensive fallback. Record slot as canceled so a late result
		// won't try to write to a resultCh whose caller is long gone.
		p.slotsMu.Lock()
		slot.canceled = true
		p.slotsMu.Unlock()
		slog.Warn("passthrough: slot orphaned", "slot_id", slot.id, "elapsed", time.Since(slot.enqueueAt))
		return nil, ErrOrphanedSlot
	}
}

// writeUserMessageUnderShimLock writes one NDJSON user-message line directly
// to the shim, bypassing shimWriter's fast-path that would re-acquire
// shimWMu. Caller MUST hold shimWMu.
func (p *Process) writeUserMessageUnderShimLock(uuidStr, text string, images []ImageData, priority string) error {
	// We can't feed through stdinWriter (shimWriter) because its Write path
	// calls p.shimSend which takes shimWMu. Build the payload ourselves via
	// the protocol and push via shimSendLocked.
	//
	// For ClaudeProtocol, WriteUserMessageLocked expects an io.Writer that
	// accepts one '\n'-terminated line. We feed a thin capture-writer and
	// forward the captured bytes through shimSendLocked.
	cap := &captureWriter{}
	if err := p.protocol.WriteUserMessageLocked(cap, uuidStr, text, images, priority); err != nil {
		return err
	}
	line := cap.bytes
	// Strip trailing newline — shimSendLocked re-adds its own framing via the
	// shim's "write" frame structure.
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	if len(line) > maxStdinLineBytes {
		return fmt.Errorf("%w: %d bytes > %d", ErrMessageTooLarge, len(line), maxStdinLineBytes)
	}
	return p.shimSendLocked(shimClientMsg{Type: "write", Line: string(line)})
}

// captureWriter is an io.Writer that accumulates bytes into an in-memory slice.
// Used by writeUserMessageUnderShimLock to route Protocol.WriteUserMessageLocked
// output through the shim's "write" frame instead of directly to stdin.
type captureWriter struct {
	bytes []byte
}

func (c *captureWriter) Write(b []byte) (int, error) {
	c.bytes = append(c.bytes, b...)
	return len(b), nil
}

// removeSlotByID removes a single slot from pendingSlots. Used on write-fail.
// FIFO preserved for the remaining entries.
func (p *Process) removeSlotByID(id uint64) {
	p.slotsMu.Lock()
	defer p.slotsMu.Unlock()
	for i, s := range p.pendingSlots {
		if s.id == id {
			p.pendingSlots = append(p.pendingSlots[:i], p.pendingSlots[i+1:]...)
			return
		}
	}
}

// removeSlotsLocked strips the given slots from pendingSlots while preserving
// relative order of the rest. Caller must hold slotsMu.
func (p *Process) removeSlotsLocked(victims []*sendSlot) {
	if len(victims) == 0 {
		return
	}
	// Fast path: for ≤4 victims (common passthrough case: 1 owner per turn)
	// a linear scan is allocation-free and faster than building a map.
	kept := p.pendingSlots[:0]
	if len(victims) <= 4 {
		for _, s := range p.pendingSlots {
			isVictim := false
			for _, v := range victims {
				if s.id == v.id {
					isVictim = true
					break
				}
			}
			if !isVictim {
				kept = append(kept, s)
			}
		}
	} else {
		victimSet := make(map[uint64]struct{}, len(victims))
		for _, v := range victims {
			victimSet[v.id] = struct{}{}
		}
		for _, s := range p.pendingSlots {
			if _, isVictim := victimSet[s.id]; !isVictim {
				kept = append(kept, s)
			}
		}
	}
	// Zero out the tail so GC can reclaim the trailing slot references.
	for i := len(kept); i < len(p.pendingSlots); i++ {
		p.pendingSlots[i] = nil
	}
	// Reclaim capacity when the slice has shrunk to <25% of its backing
	// array: a session that briefly burst to maxPendingSlots then went
	// idle would otherwise permanently hold a maxPendingSlots-sized
	// backing array for the rest of its lifetime. Only copy if the win
	// is real (len*4 < cap) to avoid thrashing on steady-state traffic.
	if cap(kept) > 8 && len(kept)*4 < cap(kept) {
		shrunk := make([]*sendSlot, len(kept), len(kept)+2)
		copy(shrunk, kept)
		kept = shrunk
	}
	p.pendingSlots = kept
}

// findSlotByUUIDLocked returns the first pending slot whose uuid matches.
// Caller must hold slotsMu.
func (p *Process) findSlotByUUIDLocked(u string) *sendSlot {
	if u == "" {
		return nil
	}
	for _, s := range p.pendingSlots {
		if s.uuid == u {
			return s
		}
	}
	return nil
}

// handleReplayEventLocked dispatches a user replay event. The CLI emits
// replays in one of two shapes:
//
//  1. Independent replay — uuid == naozhi's uuid, content carries exactly
//     the original text. These appear both for single-message turns and
//     for each message in a burst that the CLI will later batch. Matched
//     by uuid.
//
//  2. Merged replay — uuid is CLI-synthesized (not in naozhi's pending
//     slots). Content is a single string that concatenates the batch with
//     a space separator (observed in live tests, not block arrays as the
//     earlier RFC assumed). naozhi CANNOT reliably split this string, so
//     we claim all not-yet-replayed pending slots instead: a merged replay
//     means "every currently-enqueued, still-unclaimed message is part of
//     this turn".
//
// Caller must hold slotsMu. This is the only place currentTurnSlots grows
// for user replays.
func (p *Process) handleReplayEventLocked(ev Event) {
	// Independent replay: uuid matches a pending slot.
	if slot := p.findSlotByUUIDLocked(ev.UUID); slot != nil {
		if slot.replayed {
			slog.Debug("passthrough: replay uuid already claimed", "uuid", ev.UUID, "slot_id", slot.id)
			return
		}
		slot.replayed = true
		p.currentTurnSlots = append(p.currentTurnSlots, slot)
		slog.Debug("passthrough: independent replay matched", "uuid", ev.UUID,
			"slot_id", slot.id, "turn_slots", len(p.currentTurnSlots))
		return
	}

	// Merged replay: sweep up every pending slot that hasn't been claimed
	// yet. This is correct because the CLI only generates a merged replay
	// when it is about to start a turn that consumes all in-flight stdin
	// user messages — there is no scenario where only a subset of pending
	// slots should land in the merged group.
	claimed := 0
	for _, s := range p.pendingSlots {
		if s.replayed {
			continue
		}
		s.replayed = true
		p.currentTurnSlots = append(p.currentTurnSlots, s)
		claimed++
	}
	slog.Debug("passthrough: merged replay swept", "uuid", ev.UUID,
		"claimed", claimed, "turn_slots", len(p.currentTurnSlots),
		"pending_total", len(p.pendingSlots))
}

// extractReplayTexts pulls out text blocks from a user-replay event's content.
// Returns an empty slice if the event doesn't carry text blocks (e.g. a
// tool_result user event that slipped in; caller filters those upstream).
func extractReplayTexts(ev Event) []string {
	if ev.Message == nil {
		return nil
	}
	out := make([]string, 0, len(ev.Message.Content))
	for _, b := range ev.Message.Content {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return out
}

// fanoutTurnResult delivers one CLI result event to every slot the turn
// claimed. The first claimed slot gets the full SendResult; the rest get a
// follower SendResult with MergedWithHead pointing at the head slot.
//
// This is called from readLoop after releasing slotsMu to avoid holding the
// mutex across channel sends (which can be blocking if the Send caller has
// returned but resultCh has cap 1, it won't block in practice).
func fanoutTurnResult(owners []*sendSlot, ev Event) {
	slog.Debug("passthrough: fanout", "owners", len(owners),
		"result_len", len(ev.Result), "session", ev.SessionID)
	if len(owners) == 0 {
		slog.Warn("passthrough: orphan result, no slot claim",
			"session", ev.SessionID, "result_len", len(ev.Result))
		return
	}

	head := owners[0]
	mergedCount := len(owners)

	headRes := &SendResult{
		Text:        ev.Result,
		SessionID:   ev.SessionID,
		CostUSD:     ev.CostUSD,
		MergedCount: mergedCount,
	}
	deliverSlotResult(head, headRes)

	if mergedCount == 1 {
		return
	}
	for _, slot := range owners[1:] {
		folRes := &SendResult{
			Text:           "",
			SessionID:      ev.SessionID,
			CostUSD:        0,
			MergedCount:    mergedCount,
			MergedWithHead: head.id,
			HeadText:       ev.Result,
		}
		deliverSlotResult(slot, folRes)
	}
}

// deliverSlotResult writes to slot.resultCh unless the slot was canceled. The
// resultCh has cap 1 so non-blocking send is safe — a full channel would mean
// fanout is running twice against the same slot, which should never happen.
func deliverSlotResult(s *sendSlot, r *SendResult) {
	if s.isCanceled() {
		return
	}
	select {
	case s.resultCh <- r:
	default:
		slog.Warn("passthrough: resultCh full, dropping", "slot_id", s.id)
	}
}

// discardAllPending is used when the CLI is known dead or the session is
// reset. All pending + currentTurn slots receive the given error; caller
// should not touch slot state afterwards.
//
// currentTurnSlots 必须和 pendingSlots 一起被通知：有些 slot 已经被 replay
// 从 pendingSlots 移入 currentTurnSlots，若只 nil 掉 currentTurnSlots 而不
// 给它们投 errCh，对应的 SendPassthrough goroutine 会阻塞到 total+30s bail
// timer 才返回，IM 用户表现为"无响应"而非明确错误，同时 goroutine + 栈
// 内存悬挂 5.5 分钟。R192-CLI-P0-DiscardCurrentTurn。
func (p *Process) discardAllPending(reason error) {
	p.slotsMu.Lock()
	victims := make([]*sendSlot, 0, len(p.pendingSlots)+len(p.currentTurnSlots))
	victims = append(victims, p.pendingSlots...)
	victims = append(victims, p.currentTurnSlots...)
	p.pendingSlots = nil
	p.currentTurnSlots = nil
	p.inTurn = false
	p.slotsMu.Unlock()

	for _, s := range victims {
		if s.isCanceled() {
			continue
		}
		select {
		case s.errCh <- reason:
		default:
		}
	}
}

// DiscardPassthroughPending is the exported surface for session/router to
// trigger on /new, /clear, or a forced reset. Exported name is explicit so
// callers don't confuse it with the internal discardAllPending used on
// process death.
func (p *Process) DiscardPassthroughPending(reason error) {
	p.discardAllPending(reason)
}

// onSystemInit marks the start of a new turn for the watchdog clock. It does
// NOT clear currentTurnSlots: live experiments confirm the CLI emits
// independent `isReplay:true` user events for enqueued messages *between*
// turns (after the previous turn's result and before the next turn's init).
// Those slot claims must survive into the next turn's result fan-out.
// currentTurnSlots is instead zeroed by onTurnResult after the result is
// delivered. See docs/rfc/passthrough-mode-validation.md (post-burst
// observation) and §5.2.3 state machine.
//
// Also flips Process.State → Running so InterruptViaControl and the
// dashboard "isRunning" probe see the passthrough turn as active. The
// legacy Send path owns this flip inside Send() itself; passthrough needs
// its own hook because the Send slot goroutine blocks on resultCh without
// touching State.
func (p *Process) onSystemInit() {
	p.slotsMu.Lock()
	p.turnStartedAt = time.Now()
	p.inTurn = true
	p.slotsMu.Unlock()

	p.mu.Lock()
	if p.State == StateReady || p.State == StateSpawning {
		p.State = StateRunning
	}
	p.mu.Unlock()
}

// onTurnResult is called when readLoop sees a result event. It snapshots the
// turn's claimed slots, strips them from pendingSlots, and returns them to
// the caller for out-of-lock fanout.
//
// When isAbort is true (result.subtype == "error_during_execution"), the
// CLI dropped the in-flight turn — typically due to a priority:"now" message
// preempting it. Any pending slot that was written before the preemption
// and never got its replay was discarded by the CLI. We identify them as
// "unreplayed slots that existed before the earliest unclaimed priority:now
// slot" and fire ErrAbortedByUrgent so their SendPassthrough callers unblock.
// The caller still owns fanout for the actual claimed turn slots; those
// just get the (empty) error result as a regular fanout.
func (p *Process) onTurnResult() (owners []*sendSlot, aborted []*sendSlot) {
	p.slotsMu.Lock()
	owners = p.currentTurnSlots
	p.currentTurnSlots = nil
	p.inTurn = false
	p.removeSlotsLocked(owners)
	pendingLeft := len(p.pendingSlots)
	p.slotsMu.Unlock()

	// Mirror the Send path's State→Ready transition on the last passthrough
	// turn so the dashboard / IsRunning / InterruptViaControl see the session
	// as idle. Only flip when we actually consumed owners (i.e. passthrough
	// really drove this turn); a stray result with no slot claim might be a
	// legacy Send path or a reconnect replay — leave that to the existing
	// handler below in readLoop.
	if len(owners) > 0 && pendingLeft == 0 {
		p.mu.Lock()
		if p.State == StateRunning {
			p.State = StateReady
		}
		p.mu.Unlock()
	}
	return owners, nil
}

// reapAbortedPreempted collects pending slots that were discarded by the CLI
// when a priority:"now" preempted the active turn. An affected slot is one
// that: (a) was not replayed before the abort result arrived, and (b) is
// itself not a priority:"now" slot (those triggered the preemption and
// should proceed into the next turn, not be reaped).
//
// Returns the victims after removing them from pendingSlots. Called on
// result.subtype == "error_during_execution".
func (p *Process) reapAbortedPreempted() []*sendSlot {
	p.slotsMu.Lock()
	defer p.slotsMu.Unlock()
	var victims []*sendSlot
	kept := p.pendingSlots[:0]
	for _, s := range p.pendingSlots {
		if !s.replayed && s.priority != "now" {
			victims = append(victims, s)
			continue
		}
		kept = append(kept, s)
	}
	for i := len(kept); i < len(p.pendingSlots); i++ {
		p.pendingSlots[i] = nil
	}
	p.pendingSlots = kept
	return victims
}

// fireAbortErrors delivers ErrAbortedByUrgent to each aborted slot's caller.
// Uses isCanceled() (not direct field read) to match deliverSlotResult and
// avoid the "concurrent canceled-field write vs fireAbortErrors read" race
// when reapAbortedPreempted releases slotsMu before fireAbortErrors runs.
func fireAbortErrors(victims []*sendSlot) {
	for _, s := range victims {
		if s.isCanceled() {
			continue
		}
		select {
		case s.errCh <- ErrAbortedByUrgent:
		default:
		}
	}
}

// PassthroughActive returns true when there is at least one pending slot or
// currentTurn slot. Used by callers (dashboard, watchdog) to decide whether
// result events should be routed through fanout vs. the legacy eventCh path.
func (p *Process) PassthroughActive() bool {
	p.slotsMu.Lock()
	defer p.slotsMu.Unlock()
	return len(p.pendingSlots) > 0 || len(p.currentTurnSlots) > 0
}

// PassthroughDepth returns the current pending slot count. Used by dispatch
// for background pressure signaling.
func (p *Process) PassthroughDepth() int {
	p.slotsMu.Lock()
	defer p.slotsMu.Unlock()
	return len(p.pendingSlots)
}

// SupportsPassthrough reports whether this Process's backing protocol can run
// in passthrough mode. Currently equivalent to Protocol.SupportsReplay()
// because replay events are required for naozhi ↔ CLI slot matching.
func (p *Process) SupportsPassthrough() bool {
	return p.protocol.SupportsReplay()
}
