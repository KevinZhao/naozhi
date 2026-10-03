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
	"sync/atomic"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// slotUUIDFallbackSeq is the monotonic counter newSlotUUID's crypto/rand
// fallback reads. It used to be shared with the event-log ring's minting path;
// that sharing was incidental (the two hash prefixes already keep the outputs
// disjoint) and the ring now lives in internal/eventlog/ring, so each side keeps
// its own counter. Uniqueness is only ever needed within one fallback path.
var slotUUIDFallbackSeq atomic.Int64

// newSlotUUID returns a 128-bit random hex string for the Claude CLI's uuid
// field (an opaque round-tripped blob; RFC4122 formatting is not needed). On
// a crypto/rand failure it falls back to a hashed counter rather than an
// all-zero UUID, which would break slot FIFO matching.
func newSlotUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		slog.Warn("crypto/rand.Read failed for slot UUID; using fallback identity", "err", err)
		sum := sha256.Sum256([]byte("naozhi-slot-uuid-fallback-" + strconv.FormatInt(int64(slotUUIDFallbackSeq.Add(1)), 10)))
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
func (p *Process) SendPassthrough(ctx context.Context, text string, images []clievent.Attachment,
	onEvent clievent.EventCallback, priority string) (*clievent.SendResult, error) {

	if !p.caps.Replay {
		return nil, fmt.Errorf("passthrough: protocol %s does not support replay", p.protocol.Name())
	}

	// Fast reject: dead process won't produce a result.
	if !p.Alive() {
		return nil, clierr.ErrProcessExited
	}

	// Shrink oversized inline images once before the write (mirrors Send) so
	// the CLI payload, the dashboard bubble and every replay share the bytes.
	images = downscaleImagesForVision(images)

	slot := &sendSlot{
		id:        p.slots.idGen.Add(1),
		uuid:      newSlotUUID(),
		text:      text,
		priority:  priority,
		onEvent:   onEvent,
		resultCh:  make(chan *clievent.SendResult, 1),
		errCh:     make(chan error, 1),
		enqueueAt: time.Now(),
	}

	// Lock order: the link's write lock → slotsMu (the only place both are
	// taken). Holding the write lock across append+write guarantees
	// pendingSlots order equals the order lines hit the shim socket;
	// otherwise two concurrent sends could invert them and break FIFO
	// turn-result attribution.
	queued := true
	writeErr := p.link.withWriteLock(func() error {
		p.slots.mu.Lock()
		if len(p.slots.pending) >= maxPendingSlots {
			p.slots.mu.Unlock()
			queued = false
			return nil
		}
		p.slots.pending = append(p.slots.pending, slot)
		p.slots.mu.Unlock()
		// stdinWriter (shimWriter) would re-acquire the write lock and
		// deadlock, so write through a helper that uses sendLocked.
		return p.writeUserMessageUnderShimLock(slot.uuid, text, images, priority)
	})
	if !queued {
		return nil, clierr.ErrTooManyPending
	}

	if writeErr != nil {
		// CLI never saw this message; FIFO is intact because nothing was
		// written. Surface the canonical clierr.ErrProcessExited if the process died
		// between the Alive() check and the write.
		p.removeSlotByID(slot.id)
		if !p.Alive() {
			return nil, clierr.ErrProcessExited
		}
		return nil, fmt.Errorf("passthrough write: %w", writeErr)
	}

	// Mirror Send's user-entry Append so a later subscribe can re-render the
	// bubble (readLoop filters the CLI's replay echo out of ring.EventLog). After
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
		p.slots.mu.Lock()
		slot.canceled.Store(true)
		p.slots.mu.Unlock()
		return nil, ctx.Err()
	case <-bail.C:
		// Mark canceled so a late result does not target a gone caller.
		p.slots.mu.Lock()
		slot.canceled.Store(true)
		p.slots.mu.Unlock()
		slog.Warn("passthrough: slot orphaned", "slot_id", slot.id, "elapsed", time.Since(slot.enqueueAt))
		return nil, clierr.ErrOrphanedSlot
	}
}

// writeUserMessageUnderShimLock writes one NDJSON user-message line directly
// to the shim via a pooled capture writer + sendLocked, bypassing
// shimWriter's fast path that would re-acquire the write lock. Caller MUST
// be inside the link's withWriteLock.
func (p *Process) writeUserMessageUnderShimLock(uuidStr, text string, images []clievent.Attachment, priority string) error {
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
		return fmt.Errorf("%w: %d bytes > %d", clierr.ErrMessageTooLarge, len(line), maxStdinLineBytes)
	}
	// string(line) copies, so the pooled buffer is free to reuse after Put.
	return p.link.sendLocked(shimClientMsg{Type: "write", Line: string(line)})
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
	p.slots.mu.Lock()
	defer p.slots.mu.Unlock()
	for i, s := range p.slots.pending {
		if s.id == id {
			old := p.slots.pending
			p.slots.pending = append(old[:i], old[i+1:]...)
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
	kept := p.slots.pending[:0]
	if len(victims) <= 4 {
		for _, s := range p.slots.pending {
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
		for _, s := range p.slots.pending {
			if _, isVictim := victimSet[s.id]; !isVictim {
				kept = append(kept, s)
			}
		}
	}
	for i := len(kept); i < len(p.slots.pending); i++ {
		p.slots.pending[i] = nil
	}
	// Shrink when <25% of the backing array is live so a burst to
	// maxPendingSlots does not pin that array for the session's lifetime.
	if cap(kept) > 8 && len(kept)*4 < cap(kept) {
		shrunk := make([]*sendSlot, len(kept), len(kept)+2)
		copy(shrunk, kept)
		kept = shrunk
	}
	p.slots.pending = kept
}

// findSlotByUUIDLocked returns the first pending slot whose uuid matches.
// Caller must hold slotsMu.
func (p *Process) findSlotByUUIDLocked(u string) *sendSlot {
	if u == "" {
		return nil
	}
	for _, s := range p.slots.pending {
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
func (p *Process) handleReplayEventLocked(ev clievent.Event) {
	if slot := p.findSlotByUUIDLocked(ev.UUID); slot != nil {
		if slot.replayed {
			slog.Debug("passthrough: replay uuid already claimed", "uuid", ev.UUID, "slot_id", slot.id)
			return
		}
		slot.replayed = true
		p.slots.current = append(p.slots.current, slot)
		slog.Debug("passthrough: independent replay matched", "uuid", ev.UUID,
			"slot_id", slot.id, "turn_slots", len(p.slots.current))
		return
	}

	// Merged replay: sweep every unclaimed pending slot.
	claimed := 0
	for _, s := range p.slots.pending {
		if s.replayed {
			continue
		}
		s.replayed = true
		p.slots.current = append(p.slots.current, s)
		claimed++
	}
	slog.Debug("passthrough: merged replay swept", "uuid", ev.UUID,
		"claimed", claimed, "turn_slots", len(p.slots.current),
		"pending_total", len(p.slots.pending))
}

// fanoutTurnResult delivers one CLI result event to every slot the turn
// claimed: the head slot gets the full clievent.SendResult, followers get
// MergedWithHead pointing at it. Called from readLoop after releasing slotsMu
// so channel sends never happen under the lock.
func fanoutTurnResult(owners []*sendSlot, ev clievent.Event) {
	slog.Debug("passthrough: fanout", "owners", len(owners),
		"result_len", len(ev.Result), "session", ev.SessionID)
	if len(owners) == 0 {
		slog.Warn("passthrough: orphan result, no slot claim",
			"session", ev.SessionID, "result_len", len(ev.Result))
		return
	}

	head := owners[0]
	mergedCount := len(owners)

	headRes := resultFromEvent(ev)
	headRes.MergedCount = mergedCount
	deliverSlotResult(head, &headRes)

	if mergedCount == 1 {
		return
	}
	for _, slot := range owners[1:] {
		folRes := &clievent.SendResult{
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
func deliverSlotResult(s *sendSlot, r *clievent.SendResult) {
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
	p.slots.mu.Lock()
	victims := make([]*sendSlot, 0, len(p.slots.pending)+len(p.slots.current))
	victims = append(victims, p.slots.pending...)
	victims = append(victims, p.slots.current...)
	p.slots.pending = nil
	p.slots.current = nil
	p.slots.mu.Unlock()

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
	p.transition(evTurnStarted)
}

// onTurnResult is called when readLoop sees a result event. It snapshots the
// turn's claimed slots, strips them from pendingSlots, and returns them for
// out-of-lock fanout (an aborted turn's victims are handled separately by
// reapAbortedPreempted).
func (p *Process) onTurnResult() []*sendSlot {
	p.slots.mu.Lock()
	owners := p.slots.current
	p.slots.current = nil
	p.removeSlotsLocked(owners)
	pendingLeft := len(p.slots.pending)
	p.slots.mu.Unlock()

	// Mirror Send's State→Ready on the last passthrough turn. Only when
	// owners were consumed: a result with no claim may be a Send-path turn or
	// a reconnect replay, which readLoop handles itself.
	if len(owners) > 0 && pendingLeft == 0 {
		p.transition(evTurnEnded)
	}
	return owners
}

// turnSendOwned reports whether a Send claimed the current turn.
func (p *Process) turnSendOwned() bool {
	p.turn.mu.RLock()
	defer p.turn.mu.RUnlock()
	return p.turn.sendOwned
}

// endUnownedTurn ends a turn the CLI started itself (a background-task
// notification) at its result, which no slot and no Send owns: onSystemInit
// moved the process to Running and nothing moved it back, so Cleanup's stuck
// check killed it later (#3096). It stays Running if passthrough messages are
// queued for the next turn, and hands the result to the session to book now.
// Ownership is decided before deliverEvent and this runs after it: ending the
// turn first would let a Send claim the process and take this result as its
// reply; deciding after would let a Send that just got its own result look
// unowned and have its cost booked here under another run.
func (p *Process) endUnownedTurn(ev clievent.Event) {
	p.slots.mu.Lock()
	pending := len(p.slots.pending)
	p.slots.mu.Unlock()

	p.turn.mu.Lock()
	ended := false
	if pending == 0 {
		_, ended = p.turn.transitionLocked(evTurnEnded)
	}
	onDone, onResult := p.turn.onTurnDone, p.turn.onUnownedResult
	p.turn.mu.Unlock()

	if onResult != nil {
		onResult(resultFromEvent(ev))
	}
	if ended && onDone != nil {
		onDone()
	}
}

// reapAbortedPreempted collects pending slots the CLI discarded when a
// priority:"now" preempted the active turn (result.subtype ==
// "error_during_execution"): slots not yet replayed that are not themselves
// priority:"now" (those proceed into the next turn). Returns the victims
// after removing them from pendingSlots.
func (p *Process) reapAbortedPreempted() []*sendSlot {
	p.slots.mu.Lock()
	defer p.slots.mu.Unlock()
	var victims []*sendSlot
	kept := p.slots.pending[:0]
	for _, s := range p.slots.pending {
		if !s.replayed && s.priority != "now" {
			victims = append(victims, s)
			continue
		}
		kept = append(kept, s)
	}
	for i := len(kept); i < len(p.slots.pending); i++ {
		p.slots.pending[i] = nil
	}
	p.slots.pending = kept
	return victims
}

// fireAbortErrors delivers clierr.ErrAbortedByUrgent to each aborted slot's caller.
// isCanceled() (atomic) is required: slotsMu is already released here.
func fireAbortErrors(victims []*sendSlot) {
	for _, s := range victims {
		if s.isCanceled() {
			continue
		}
		select {
		case s.errCh <- clierr.ErrAbortedByUrgent:
		default:
		}
	}
}

// PassthroughActive returns true when there is at least one pending slot or
// currentTurn slot. Used by callers (dashboard, watchdog) to decide whether
// result events should be routed through fanout vs. the legacy eventCh path.
func (p *Process) PassthroughActive() bool {
	p.slots.mu.Lock()
	defer p.slots.mu.Unlock()
	return len(p.slots.pending) > 0 || len(p.slots.current) > 0
}

// PassthroughDepth returns the current pending slot count. Used by dispatch
// for background pressure signaling.
func (p *Process) PassthroughDepth() int {
	p.slots.mu.Lock()
	defer p.slots.mu.Unlock()
	return len(p.slots.pending)
}

// SupportsPassthrough reports whether this Process's backing protocol can run
// in passthrough mode (replay events are required for slot matching).
func (p *Process) SupportsPassthrough() bool {
	return p.caps.Replay
}
