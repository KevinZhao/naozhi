package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// newSlotUUID returns a 128-bit random hex string for the Claude CLI's uuid
// field (an opaque round-tripped blob; RFC4122 formatting is not needed). On
// a crypto/rand failure it falls back to a hashed counter (uuidFallbackSeq,
// shared with newEventUUID) rather than an all-zero UUID, which would break
// slot FIFO matching.
func newSlotUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		slog.Warn("crypto/rand.Read failed for slot UUID; using fallback identity", "err", err)
		sum := sha256.Sum256([]byte("naozhi-slot-uuid-fallback-" + strconv.FormatInt(int64(uuidFallbackSeq.Add(1)), 10)))
		copy(b[:], sum[:])
	}
	return hex.EncodeToString(b[:])
}

// SendPassthrough writes a user message to the CLI in passthrough mode and
// waits for the matching turn result. Safe for concurrent callers: ordering is
// preserved by appending the sendSlot and writing stdin under one lock. Unlike
// Send it holds no "busy" flag — the CLI's own commandQueue queues turns and
// naozhi only does uuid ↔ slot bookkeeping. Requires
// Protocol.SupportsReplay() (callers fall back to Send otherwise; without
// replay events nothing would ever claim the slot).
// priority: "" | "now" | "next" | "later"; "now" aborts the in-flight turn.
func (p *Process) SendPassthrough(ctx context.Context, text string, images []Attachment,
	onEvent EventCallback, priority string) (*SendResult, error) {

	if !p.caps.Replay {
		return nil, fmt.Errorf("passthrough: protocol %s does not support replay", p.protocol.Name())
	}

	// Fast reject: dead process won't produce a result.
	if !p.Alive() {
		return nil, ErrProcessExited
	}

	// Shrink oversized inline images once before the write (mirrors Send) so
	// the CLI payload, the dashboard bubble and every replay share the bytes.
	images = downscaleImagesForVision(images)

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

	// Lock order: shimWMu → slotsMu (the only place both are taken). Holding
	// shimWMu across append+write guarantees pendingSlots order equals the
	// order lines hit the shim socket; otherwise two concurrent sends could
	// invert them and break FIFO turn-result attribution.
	p.shimWMu.Lock()
	p.slotsMu.Lock()

	if len(p.pendingSlots) >= maxPendingSlots {
		p.slotsMu.Unlock()
		p.shimWMu.Unlock()
		return nil, ErrTooManyPending
	}
	p.pendingSlots = append(p.pendingSlots, slot)
	p.slotsMu.Unlock()

	// stdinWriter (shimWriter) would re-acquire shimWMu via shimSend and
	// deadlock, so write through a helper that reuses shimSendLocked.
	writeErr := p.writeUserMessageUnderShimLock(slot.uuid, text, images, priority)
	slot.writtenAt = time.Now()
	p.shimWMu.Unlock()

	if writeErr != nil {
		// CLI never saw this message; FIFO is intact because nothing was
		// written. Surface the canonical ErrProcessExited if the process died
		// between the Alive() check and the write.
		p.removeSlotByID(slot.id)
		if !p.Alive() {
			return nil, ErrProcessExited
		}
		return nil, fmt.Errorf("passthrough write: %w", writeErr)
	}

	// Mirror Send's user-entry Append so a later subscribe can re-render the
	// bubble (readLoop filters the CLI's replay echo out of EventLog). After
	// the successful write so a rejected write leaves no ghost entry.
	p.eventLog.Append(buildUserEntry(text, images))

	// Defensive bail timer: passthrough has no per-turn watchdog (CLI 本身和
	// shim 的 heartbeat 负责探测进程级死锁；slot 级超时由 bail 兜底)，so
	// totalTimeout + 30s still unblocks the caller if both miss.
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
		// Tombstone: keep the slot so FIFO positioning survives; fanout
		// sees canceled=true and drops the late result.
		p.slotsMu.Lock()
		slot.canceled.Store(true)
		p.slotsMu.Unlock()
		return nil, ctx.Err()
	case <-bail.C:
		// Mark canceled so a late result does not target a gone caller.
		p.slotsMu.Lock()
		slot.canceled.Store(true)
		p.slotsMu.Unlock()
		slog.Warn("passthrough: slot orphaned", "slot_id", slot.id, "elapsed", time.Since(slot.enqueueAt))
		return nil, ErrOrphanedSlot
	}
}

// writeUserMessageUnderShimLock writes one NDJSON user-message line directly
// to the shim via a pooled capture writer + shimSendLocked, bypassing
// shimWriter's fast path that would re-acquire shimWMu. Caller MUST hold
// shimWMu.
func (p *Process) writeUserMessageUnderShimLock(uuidStr, text string, images []Attachment, priority string) error {
	cw := captureWriterPool.Get().(*captureWriter)
	cw.bytes = cw.bytes[:0]
	// Don't return oversized buffers (multi-MB image messages) to the pool;
	// they would pin heap for the worker's lifetime.
	defer func() {
		if cap(cw.bytes) > captureWriterMaxKeepBytes {
			return
		}
		captureWriterPool.Put(cw)
	}()
	if err := p.protocol.WriteUserMessageLocked(cw, uuidStr, text, images, priority); err != nil {
		return err
	}
	line := cw.bytes
	// The shim "write" frame carries its own line framing.
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	if len(line) > maxStdinLineBytes {
		return fmt.Errorf("%w: %d bytes > %d", ErrMessageTooLarge, len(line), maxStdinLineBytes)
	}
	// string(line) copies, so the pooled buffer is free to reuse after Put.
	return p.shimSendLocked(shimClientMsg{Type: "write", Line: string(line)})
}

// captureWriter is an io.Writer that accumulates bytes into an in-memory
// slice so Protocol.WriteUserMessageLocked output can be routed through the
// shim's "write" frame.
type captureWriter struct {
	bytes []byte
}

func (c *captureWriter) Write(b []byte) (int, error) {
	c.bytes = append(c.bytes, b...)
	return len(b), nil
}

// captureWriterPool reuses captureWriter values and their backing slices
// across passthrough sends. Get callers MUST reset via `c.bytes = c.bytes[:0]`.
var captureWriterPool = sync.Pool{
	New: func() any {
		return &captureWriter{bytes: make([]byte, 0, 4096)}
	},
}

// captureWriterMaxKeepBytes caps the backing slice size that survives a Put
// back into captureWriterPool; 64KiB covers every text-only payload while
// letting the GC reclaim image-blob outliers.
const captureWriterMaxKeepBytes = 64 * 1024

// removeSlotByID removes a single slot from pendingSlots. Used on write-fail.
// FIFO preserved for the remaining entries.
func (p *Process) removeSlotByID(id uint64) {
	p.slotsMu.Lock()
	defer p.slotsMu.Unlock()
	for i, s := range p.pendingSlots {
		if s.id == id {
			old := p.pendingSlots
			p.pendingSlots = append(old[:i], old[i+1:]...)
			// Zero the tail so GC can reclaim the dropped slot.
			old[len(old)-1] = nil
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
	// ≤4 victims (the common case): allocation-free linear scan beats a map.
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
	for i := len(kept); i < len(p.pendingSlots); i++ {
		p.pendingSlots[i] = nil
	}
	// Shrink when <25% of the backing array is live so a burst to
	// maxPendingSlots does not pin that array for the session's lifetime.
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

// handleReplayEventLocked dispatches a user replay event. Two shapes:
// independent (uuid == a pending slot's uuid, matched by uuid) and merged
// (CLI-synthesised uuid, content is the batch joined with spaces — not
// splittable, so every not-yet-replayed pending slot is claimed: a merged
// replay means the turn consumes all in-flight messages). Caller must hold
// slotsMu; this is the only place currentTurnSlots grows for user replays.
func (p *Process) handleReplayEventLocked(ev Event) {
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

	// Merged replay: sweep every unclaimed pending slot.
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

// fanoutTurnResult delivers one CLI result event to every slot the turn
// claimed: the head slot gets the full SendResult, followers get
// MergedWithHead pointing at it. Called from readLoop after releasing slotsMu
// so channel sends never happen under the lock.
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
		ModelUsage:  ev.ModelUsage,
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
// should not touch slot state afterwards. currentTurnSlots 必须和 pendingSlots
// 一起被通知，否则已被 replay 认领的 slot 会阻塞到 total+30s bail timer，IM
// 用户表现为"无响应"而非明确错误。
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
// trigger on /new, /clear, or a forced reset.
func (p *Process) DiscardPassthroughPending(reason error) {
	p.discardAllPending(reason)
}

// onSystemInit marks the start of a new turn. It does NOT clear
// currentTurnSlots: the CLI emits replay events for enqueued messages
// between turns and those claims must survive into the next fan-out;
// onTurnResult zeroes them after delivery. Also flips State → Running so
// InterruptViaControl and the dashboard see the passthrough turn as active
// (Send does this itself; passthrough callers block on resultCh instead).
func (p *Process) onSystemInit() {
	p.slotsMu.Lock()
	p.turnStartedAt = time.Now()
	p.inTurn = true
	p.slotsMu.Unlock()

	p.mu.Lock()
	if p.state == StateReady || p.state == StateSpawning {
		p.state = StateRunning
	}
	p.mu.Unlock()
}

// onTurnResult is called when readLoop sees a result event. It snapshots the
// turn's claimed slots, strips them from pendingSlots, and returns them for
// out-of-lock fanout (an aborted turn's victims are handled separately by
// reapAbortedPreempted).
func (p *Process) onTurnResult() []*sendSlot {
	p.slotsMu.Lock()
	owners := p.currentTurnSlots
	p.currentTurnSlots = nil
	p.inTurn = false
	p.removeSlotsLocked(owners)
	pendingLeft := len(p.pendingSlots)
	p.slotsMu.Unlock()

	// Mirror Send's State→Ready on the last passthrough turn. Only when
	// owners were consumed: a result with no claim may be a Send-path turn or
	// a reconnect replay, which readLoop handles itself.
	if len(owners) > 0 && pendingLeft == 0 {
		p.mu.Lock()
		if p.state == StateRunning {
			p.state = StateReady
		}
		p.mu.Unlock()
	}
	return owners
}

// reapAbortedPreempted collects pending slots the CLI discarded when a
// priority:"now" preempted the active turn (result.subtype ==
// "error_during_execution"): slots not yet replayed that are not themselves
// priority:"now" (those proceed into the next turn). Returns the victims
// after removing them from pendingSlots.
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
// isCanceled() (atomic) is required: slotsMu is already released here.
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
// in passthrough mode (replay events are required for slot matching).
func (p *Process) SupportsPassthrough() bool {
	return p.caps.Replay
}
