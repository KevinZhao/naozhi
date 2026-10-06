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
	"github.com/naozhi/naozhi/internal/ctxutil"
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
		return nil, p.exitErr()
	}

	// Shrink oversized inline images once before the write (mirrors Send) so
	// the CLI payload, the dashboard bubble and every replay share the bytes.
	images = downscaleImagesForVision(images)

	slot := &sendSlot{
		id:        p.slots.idGen.Add(1),
		uuid:      newSlotUUID(),
		text:      text,
		priority:  priority,
		runID:     ctxutil.RunID(ctx),
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
	queued, aborts := true, false
	running := priority == "now" && p.State() == StateRunning
	writeErr := p.link.withWriteLock(func() error {
		p.slots.mu.Lock()
		if len(p.slots.pending) >= maxPendingSlots {
			p.slots.mu.Unlock()
			queued = false
			return nil
		}
		if len(p.slots.pending) == 0 {
			// The CLI owes nothing yet, so its silence so far is not a stall:
			// both turn clocks start with this message.
			p.slots.turnStartedAt = slot.enqueueAt
			p.markOutput(slot.enqueueAt)
		}
		// "now" aborts the turn in flight, if any: one is Running or still owed.
		if priority == "now" && (running || len(p.slots.pending) > 0) {
			p.turn.abortRequested.arm()
			aborts = true
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
		// written. Surface the canonical exitErr if the process died
		// between the Alive() check and the write.
		p.removeSlotByID(slot.id)
		if aborts {
			p.turn.abortRequested.disarm()
		}
		if !p.Alive() {
			return nil, p.exitErr()
		}
		return nil, fmt.Errorf("passthrough write: %w", writeErr)
	}

	// Mirror Send's user-entry Append so a later subscribe can re-render the
	// bubble (readLoop filters the CLI's replay echo out of ring.EventLog). After
	// the successful write so a rejected write leaves no ghost entry.
	p.eventLog.Append(buildUserEntry(text, images))

	return p.awaitSlot(ctx, slot)
}

// awaitSlot waits for slot's result. The watchdog applies Send's no-output and
// total budgets to the turn the CLI owes the queue (passthroughWatchdogTick),
// so a stalled CLI is killed and every caller gets the classified timeout.
// The bail timer is the backstop for a watchdog that failed to fire: it only
// trips once that turn is the bail grace past totalTimeout, so a slot
// queued behind long healthy turns keeps waiting.
func (p *Process) awaitSlot(ctx context.Context, slot *sendSlot) (*clievent.SendResult, error) {
	noOutputDur, totalDur := p.turnBudgets()
	checkInterval := p.checkInterval(noOutputDur)
	watchdog := time.NewTimer(checkInterval)
	defer watchdog.Stop()
	bailAfter := totalDur + p.passthroughBailGrace()
	bail := time.NewTimer(bailAfter)
	defer bail.Stop()

	for {
		select {
		case res := <-slot.resultCh:
			return res, nil
		case err := <-slot.errCh:
			return nil, err
		case <-ctx.Done():
			// Tombstone: keep the slot so FIFO positioning survives; fanout
			// sees canceled=true and books the late result instead.
			if res := p.abandonSlot(slot); res != nil && res.MergedWithHead == 0 {
				p.bookAbandoned(*res, slot.runID)
			}
			return nil, ctx.Err()
		case <-watchdog.C:
			if err := p.passthroughWatchdogTick(time.Now(), noOutputDur, totalDur); err != nil {
				// A result that raced the kill still wins; the process dies
				// either way.
				select {
				case res := <-slot.resultCh:
					return res, nil
				case <-slot.errCh:
				case <-p.done:
				}
				return nil, err
			}
			watchdog.Reset(checkInterval)
		case <-bail.C:
			select {
			case res := <-slot.resultCh:
				return res, nil
			case err := <-slot.errCh:
				return nil, err
			default:
			}
			if !p.Alive() {
				return nil, p.exitErr()
			}
			if wait := p.passthroughBailRemaining(time.Now(), bailAfter); wait > 0 {
				bail.Reset(wait)
				continue
			}
			// Mark canceled so a late result is booked, not handed to a gone
			// caller; one delivered since the check above still wins.
			if res := p.abandonSlot(slot); res != nil {
				return res, nil
			}
			slog.WarnContext(ctx, "passthrough: slot orphaned", "slot_id", slot.id, "elapsed", time.Since(slot.enqueueAt))
			return nil, clierr.ErrOrphanedSlot
		}
	}
}

// abandonSlot tombstones slot for a caller that stops waiting: it keeps its
// FIFO place and fan-out books its result from now on. Returns a result
// delivered before the tombstone, which fan-out has therefore not booked.
func (p *Process) abandonSlot(slot *sendSlot) *clievent.SendResult {
	p.slots.mu.Lock()
	defer p.slots.mu.Unlock()
	slot.canceled.Store(true)
	select {
	case res := <-slot.resultCh:
		return res
	default:
		return nil
	}
}

// passthroughBailRemaining is how long the turn the CLI owes the queue has
// left before it is bailAfter old; <= 0 once it is.
func (p *Process) passthroughBailRemaining(now time.Time, bailAfter time.Duration) time.Duration {
	p.slots.mu.Lock()
	turnStart := p.slots.turnStartedAt
	p.slots.mu.Unlock()
	if turnStart.IsZero() {
		return 0
	}
	return bailAfter - now.Sub(turnStart)
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
			slog.Debug("passthrough: replay uuid already claimed", "uuid", ev.UUID, "slot_id", slot.id, "run_id", slot.runID)
			return
		}
		slot.replayed = true
		p.slots.current = append(p.slots.current, slot)
		slog.Debug("passthrough: independent replay matched", "uuid", ev.UUID,
			"slot_id", slot.id, "run_id", slot.runID, "turn_slots", len(p.slots.current))
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
// MergedWithHead pointing at it. A head whose caller left is booked through
// onUnownedResult, as no finishRun will see its cost. Called from readLoop
// after releasing slotsMu.
func (p *Process) fanoutTurnResult(owners []*sendSlot, ev clievent.Event) {
	if len(owners) == 0 {
		slog.Warn("passthrough: orphan result, no slot claim",
			"session", ev.SessionID, "result_len", len(ev.Result))
		return
	}

	head := owners[0]
	// One CLI turn may answer several runs: the head's carries the cost.
	slog.Debug("passthrough: fanout", "owners", len(owners), "run_id", head.runID,
		"merged_run_ids", followerRunIDs(owners), "result_len", len(ev.Result), "session", ev.SessionID)
	mergedCount := len(owners)

	headRes := resultFromEvent(ev)
	headRes.MergedCount = mergedCount
	if !p.deliverSlotResult(head, &headRes) {
		p.bookAbandoned(headRes, head.runID)
	}

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
		p.deliverSlotResult(slot, folRes)
	}
}

// deliverSlotResult writes to slot.resultCh unless the slot was canceled and
// reports whether it did. Checking and sending under slotsMu, like
// abandonSlot's tombstone, means a result is either delivered before the
// cancel (abandonSlot takes it back) or refused after it, never lost between.
// resultCh has cap 1, so the send never blocks; a full channel would mean
// fanout ran twice against the same slot.
func (p *Process) deliverSlotResult(s *sendSlot, r *clievent.SendResult) bool {
	p.slots.mu.Lock()
	delivered, full := false, false
	if !s.isCanceled() {
		select {
		case s.resultCh <- r:
			delivered = true
		default:
			full = true
		}
	}
	p.slots.mu.Unlock()
	if full {
		slog.Warn("passthrough: resultCh full, dropping", "slot_id", s.id, "run_id", s.runID)
	}
	return delivered
}

// followerRunIDs lists the run ids of the slots a merged turn answers after
// its head; nil for a turn with one owner.
func followerRunIDs(owners []*sendSlot) []string {
	if len(owners) < 2 {
		return nil
	}
	ids := make([]string, len(owners)-1)
	for i, s := range owners[1:] {
		ids[i] = s.runID
	}
	return ids
}

// discardAllPending is used when the CLI is known dead or the session is
// reset. All pending + currentTurn slots receive the given error; caller
// should not touch slot state afterwards. currentTurnSlots 必须和 pendingSlots
// 一起被通知，否则已被 replay 认领的 slot 会阻塞到 watchdog / bail timer，IM
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
// out-of-lock fanout. An aborted turn is no different: slots it never claimed
// stay queued for their own turns. Restarts the turn clock for whatever stays
// queued.
func (p *Process) onTurnResult() []*sendSlot {
	p.slots.mu.Lock()
	owners := p.slots.current
	p.slots.current = nil
	p.removeSlotsLocked(owners)
	pendingLeft := len(p.slots.pending)
	// Each turn gets its own total budget: the next one starts now.
	if pendingLeft > 0 {
		p.slots.turnStartedAt = time.Now()
	}
	p.slots.mu.Unlock()

	// Mirror Send's State→Ready on the last passthrough turn. Only when
	// owners were consumed: a result with no claim may be a Send-path turn or
	// a reconnect replay, which readLoop handles itself.
	if len(owners) > 0 && pendingLeft == 0 {
		p.transition(evTurnEnded)
	}
	return owners
}

// settleUnclaimedResult hands a result no Send consumes to onUnownedResult so
// the session books its cost now: no Send's finishRun ever sees it, and the
// next owned result's cumulative difference is lost if the process dies first
// (#3096, #3322). noLiveSend is turnState.noLiveSend read before the result
// was queued, and abandonedRun the run of the Send that gave up on it, if
// one did. A turn the CLI started on its own (system/init with no Send
// behind it) also ends here, since nothing else moves it back to Ready; a
// queued passthrough slot keeps it Running for that slot's own turn. A result
// a live Send owns is left alone.
func (p *Process) settleUnclaimedResult(ev clievent.Event, noLiveSend bool, abandonedRun string) {
	p.slots.mu.Lock()
	pending := len(p.slots.pending)
	p.slots.mu.Unlock()
	p.turn.mu.Lock()
	ended := false
	switch {
	case p.turn.unowned:
		if pending == 0 {
			_, ended = p.turn.transitionLocked(evTurnEnded)
		}
	case !noLiveSend:
		p.turn.mu.Unlock()
		return
	}
	onDone, onResult := p.turn.onTurnDone, p.turn.onUnownedResult
	p.turn.mu.Unlock()
	if onResult != nil {
		res := resultFromEvent(ev)
		if abandonedRun != "" {
			logAbandonedResult(p.slogger(), res, abandonedRun)
		}
		onResult(res)
	}
	if ended && onDone != nil {
		onDone()
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
