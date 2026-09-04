package dispatch

import (
	"container/list"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// QueuedMsg holds a single message waiting to be processed.
type QueuedMsg struct {
	Text   string
	Images []cli.Attachment
	// MessageID is the platform-native inbound message ID (optional); used to
	// add/remove the "queued" reaction on the user's original message.
	MessageID string
	EnqueueAt time.Time
}

// QueueMode selects how new messages that arrive while a session is busy are
// handled.
type QueueMode int

const (
	// ModeCollect queues the new messages and waits for the active turn to
	// finish naturally; after a short settle delay the queued messages are
	// coalesced into a single follow-up prompt. Lowest cost, highest latency.
	ModeCollect QueueMode = iota
	// ModeInterrupt queues the new messages AND sends an in-band control_request
	// so the active turn aborts immediately; the queue is then coalesced into the
	// next prompt. Fastest pivot, but burns the aborted turn's tokens.
	ModeInterrupt
	// ModePassthrough writes each user message directly to the CLI and lets its
	// commandQueue merge; every message gets an independent (or merged-group)
	// result. Requires Protocol.SupportsReplay(); otherwise falls back to
	// ModeCollect. See docs/rfc/passthrough-mode.md.
	ModePassthrough
)

// ParseQueueMode accepts "collect" / "interrupt" / "passthrough"
// (case-insensitive). Empty or unknown strings map to ModeCollect so callers
// can feed raw YAML values without defensive checks.
func ParseQueueMode(s string) QueueMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "interrupt":
		return ModeInterrupt
	case "passthrough":
		return ModePassthrough
	default:
		return ModeCollect
	}
}

// sessionQueue tracks per-session busy state and queued messages.
type sessionQueue struct {
	busy bool
	gen  uint64 // incremented on Discard to invalidate stale owners
	// ring holds queued messages in a fixed-capacity FIFO ring buffer (#570).
	ring         msgRing
	lastNotifyNs int64 // unix nanoseconds of last ShouldNotify call
	lastEvictNs  int64 // unix nanoseconds of last eviction Warn log (rate-limit)

	// interruptRequested is set once an interrupt fired for the running turn
	// and cleared by DoneOrDrain, so follow-ups on the same turn don't each
	// send a redundant control_request.
	interruptRequested bool
}

// msgRing is a single-producer / single-consumer FIFO ring buffer; all access
// is serialised under MessageQueue.mu. Capacity is fixed by the first push
// (MessageQueue.maxDepth) and never grows; eviction-on-full is an O(1) head
// advance with the evicted slot zeroed for GC (#570). Layout:
//
//	buf:   [_, A, B, C, _, _]
//	head=1, used=3 → logical view = [A, B, C]
//
// push at full advances head and writes at (head+used)%cap, dropping A.
type msgRing struct {
	buf  []QueuedMsg
	head int // index of the oldest live element when used > 0
	used int // number of live elements; 0 <= used <= cap(buf)

	// scratch is the reusable backing array for drainInto: the owner drains one
	// batch per turn and fully consumes it before the next drain, so one
	// per-ring scratch avoids a per-turn allocation (#1827). Sound because each
	// *sessionQueue owns its ring exclusively under MessageQueue.mu.
	scratch []QueuedMsg
}

// len returns the current number of queued messages.
func (r *msgRing) len() int { return r.used }

// push appends m. When the ring holds capacity (MessageQueue.maxDepth)
// elements the oldest is overwritten and returned as dropped with
// evicted=true so the caller can warn and clear its queued reaction (#1945).
func (r *msgRing) push(m QueuedMsg, capacity int) (evicted bool, dropped QueuedMsg) {
	if cap(r.buf) == 0 {
		r.buf = make([]QueuedMsg, capacity)
	}
	if r.used == capacity {
		// Capture the dropped message before zeroing the slot (frees held image data).
		dropped = r.buf[r.head]
		r.buf[r.head] = QueuedMsg{}
		r.head = (r.head + 1) % capacity
		r.used--
		evicted = true
	}
	idx := (r.head + r.used) % capacity
	r.buf[idx] = m
	r.used++
	return evicted, dropped
}

// drainInto returns the queued messages in FIFO order and resets the ring,
// zeroing consumed slots so retained image data is GC-eligible. Returns nil
// when empty. dst is reused when it has enough capacity, else a fresh slice
// is allocated. The caller MUST fully consume the returned slice before the
// next drainInto/drainAll on the same ring (#1827).
func (r *msgRing) drainInto(dst []QueuedMsg) []QueuedMsg {
	if r.used == 0 {
		return nil
	}
	var out []QueuedMsg
	if cap(dst) >= r.used {
		out = dst[:r.used]
	} else {
		out = make([]QueuedMsg, r.used)
	}
	c := cap(r.buf)
	for i := 0; i < r.used; i++ {
		idx := (r.head + i) % c
		out[i] = r.buf[idx]
		r.buf[idx] = QueuedMsg{}
	}
	r.head = 0
	r.used = 0
	return out
}

// drainAll returns the queued messages in a freshly allocated slice (FIFO
// order) and resets the ring. Equivalent to drainInto(nil).
func (r *msgRing) drainAll() []QueuedMsg {
	return r.drainInto(nil)
}

// reset empties the ring without returning the contents (used by Discard).
// Keeps the backing array allocated for reuse; zeroes live slots for GC.
func (r *msgRing) reset() {
	if r.used == 0 {
		return
	}
	c := cap(r.buf)
	for i := 0; i < r.used; i++ {
		idx := (r.head + i) % c
		r.buf[idx] = QueuedMsg{}
	}
	r.head = 0
	r.used = 0
}

// MessageQueue implements per-session message queuing: when a session is
// busy, incoming messages are queued (up to MaxDepth) instead of dropped and
// the owner goroutine drains the queue after each turn.
//
// Thread-safe: mutating methods take mu.Lock; ShouldNotify's cooldown-active
// fast path takes mu.RLock only (#1358).
type MessageQueue struct {
	mu           sync.RWMutex
	queues       map[string]*sessionQueue
	maxDepth     int
	collectDelay time.Duration
	mode         QueueMode

	// dropNotifyLRU/dropNotifyIndex form a bounded per-key cooldown LRU for
	// notifies when no sessionQueue exists (maxDepth<=0 drop path, or between
	// Discard and a new owner), so one chat's notify never silences another's.
	// The index maps key → *dropNotifyEntry directly so the hot ShouldNotify
	// probe avoids a list.Element.Value assertion (#932).
	dropNotifyLRU   *list.List                  // element.Value = *dropNotifyEntry
	dropNotifyIndex map[string]*dropNotifyEntry // key → entry

	// dropNotifyPool recycles *dropNotifyEntry structs so steady-state cold-key
	// churn (evict tail + insert) does not heap-allocate per key (#1694).
	// Entries are reset before reuse and nil'd on return so a pooled entry
	// doesn't pin a removed list.Element.
	dropNotifyPool sync.Pool

	// onStranded, when non-nil, is invoked by Release (FIFO, outside q.mu) for
	// every message still parked in the ring — otherwise messages enqueued
	// while a Dashboard/WS Guard caller held the key would sit until the next
	// Enqueue, which on a quiet key may never come (#769). nil keeps the
	// park-in-place contract (tests / Guard-only deployments).
	onStranded func(key string, msg QueuedMsg)
}

// SetStrandHandler registers the callback Release uses to recover messages
// parked by a concurrent Enqueue while a SessionGuard caller held the key;
// nil restores park-in-place (#769). The handler runs once per message, FIFO,
// after q.mu is released and the key is idle, so it may re-enter Enqueue.
func (q *MessageQueue) SetStrandHandler(fn func(key string, msg QueuedMsg)) {
	q.mu.Lock()
	q.onStranded = fn
	q.mu.Unlock()
}

// dropNotifyEntry is a single LRU entry: key + last notify nanos. elem links
// back to the *list.Element that boxes this entry in dropNotifyLRU.
type dropNotifyEntry struct {
	key  string
	ts   int64
	elem *list.Element
}

// takePooledEntry returns a reset *dropNotifyEntry: preferred (the entry just
// evicted from the LRU tail) if non-nil, else one from dropNotifyPool, else a
// fresh allocation. Callers must hold q.mu.
func (q *MessageQueue) takePooledEntry(preferred *dropNotifyEntry) *dropNotifyEntry {
	if preferred != nil {
		preferred.key = ""
		preferred.ts = 0
		preferred.elem = nil
		return preferred
	}
	if v := q.dropNotifyPool.Get(); v != nil {
		e := v.(*dropNotifyEntry)
		e.key = ""
		e.ts = 0
		e.elem = nil
		return e
	}
	return &dropNotifyEntry{}
}

// releasePooledEntry returns an already-unlinked entry to dropNotifyPool,
// nil'ing its fields so it pins nothing. Callers must hold q.mu.
func (q *MessageQueue) releasePooledEntry(e *dropNotifyEntry) {
	if e == nil {
		return
	}
	e.key = ""
	e.ts = 0
	e.elem = nil
	q.dropNotifyPool.Put(e)
}

// dropNotifyMaxKeys bounds dropNotifyLRU; the oldest entry is evicted on
// insert when at capacity.
const dropNotifyMaxKeys = 1024

// evictWarnCooldownNs rate-limits the per-key "queue full" eviction Warn so a
// sustained flood does not drown operator signals.
const evictWarnCooldownNs = int64(5 * time.Second)

// NewMessageQueueWithMode creates a MessageQueue with an explicit queue mode.
// See QueueMode for the semantic difference between Collect and Interrupt.
func NewMessageQueueWithMode(maxDepth int, collectDelay time.Duration, mode QueueMode) *MessageQueue {
	return &MessageQueue{
		queues:          make(map[string]*sessionQueue),
		maxDepth:        maxDepth,
		collectDelay:    collectDelay,
		mode:            mode,
		dropNotifyLRU:   list.New(),
		dropNotifyIndex: make(map[string]*dropNotifyEntry),
	}
}

// Mode returns the configured queue mode.
func (q *MessageQueue) Mode() QueueMode {
	return q.mode
}

// getOrCreate returns the sessionQueue for key, creating one if needed.
// Caller must hold mu.
func (q *MessageQueue) getOrCreate(key string) *sessionQueue {
	sq := q.queues[key]
	if sq == nil {
		sq = &sessionQueue{}
		q.queues[key] = sq
	}
	return sq
}

// Enqueue adds a message for key and returns:
//   - isOwner=true: caller becomes the owner goroutine (queue was idle); gen is
//     the generation cookie.
//   - isOwner=false, enqueued=true: appended to the queue; shouldInterrupt is
//     true in ModeInterrupt for the first follow-up of the running turn.
//   - isOwner=false, enqueued=false: queue disabled (maxDepth<=0).
//
// evictedID is the MessageID of the oldest message dropped to make room, or
// "" — the caller clears that message's dangling queued reaction (#1945).
func (q *MessageQueue) Enqueue(key string, msg QueuedMsg) (isOwner, enqueued, shouldInterrupt bool, gen uint64, evictedID string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	sq := q.getOrCreate(key)
	if !sq.busy {
		sq.busy = true
		return true, false, false, sq.gen, ""
	}

	// maxDepth<=0: queue disabled, degrade to drop.
	if q.maxDepth <= 0 {
		return false, false, false, 0, ""
	}

	if evicted, dropped := sq.ring.push(msg, q.maxDepth); evicted {
		evictedID = dropped.MessageID
		// Queue-full eviction is silent data loss for the sender; Warn so
		// operators can observe backpressure, rate-limited per key.
		now := time.Now().UnixNano()
		// delta < 0 means the wall clock stepped backwards; re-anchor so the
		// rate-limit is not defeated (mirrors ShouldNotify).
		if delta := now - sq.lastEvictNs; delta < 0 || delta >= evictWarnCooldownNs {
			slog.Warn("msgqueue: dropping oldest message (queue full)",
				"key", key, "depth", sq.ring.len(), "max_depth", q.maxDepth)
			sq.lastEvictNs = now
		}
	}
	// Only the first queued follow-up of the active turn fires the interrupt;
	// the CLI would ignore a second control_request mid-abort.
	if q.mode == ModeInterrupt && !sq.interruptRequested {
		sq.interruptRequested = true
		return false, true, true, 0, evictedID
	}
	return false, true, false, 0, evictedID
}

// DoneOrDrain is called by the owner goroutine after processing a message.
// gen must match the generation returned by Enqueue; a mismatch means Discard
// ran (e.g. /new) and a new owner may have started — the stale owner must stop.
// If the queue is empty (or gen mismatches) ownership is released and nil is
// returned; otherwise all messages are drained and returned and ownership kept.
// The check-and-release MUST happen under one lock so a message cannot be
// enqueued between check and release and be stranded without an owner.
func (q *MessageQueue) DoneOrDrain(key string, gen uint64) []QueuedMsg {
	q.mu.Lock()
	defer q.mu.Unlock()

	sq := q.queues[key]
	if sq == nil {
		// Entry was discarded while we were processing.
		return nil
	}

	// Stale owner: do NOT release ownership — the new owner holds it.
	if sq.gen != gen {
		return nil
	}

	if sq.ring.len() == 0 {
		// Release ownership. Also purge any stale dropNotify LRU entry so the
		// next ShouldNotify doesn't fall through to a stale LRU timestamp and
		// silence a legitimate notification. interruptRequested is zeroed
		// explicitly in case a future refactor reuses the *sessionQueue.
		sq.interruptRequested = false
		delete(q.queues, key)
		if e, ok := q.dropNotifyIndex[key]; ok {
			q.dropNotifyLRU.Remove(e.elem)
			delete(q.dropNotifyIndex, key)
			q.releasePooledEntry(e)
		}
		return nil
	}

	// Drain all; keep ownership. Clearing interruptRequested makes the next
	// in-flight turn a fresh interrupt target. The ring's scratch is reused
	// across turns — the owner fully consumes each batch before the next
	// DoneOrDrain (#1827).
	msgs := sq.ring.drainInto(sq.ring.scratch)
	sq.ring.scratch = msgs
	sq.interruptRequested = false
	return msgs
}

// Discard clears all queued messages and releases ownership for key, bumping
// the generation so stale ownerLoops stop on their next DoneOrDrain (/new,
// /stop). The bumped gen MUST persist in the map so a concurrent Enqueue that
// becomes the new owner picks up gen+1 rather than colliding with the stale
// owner's check — hence the entry is kept.
func (q *MessageQueue) Discard(key string) {
	q.DiscardAndReturn(key)
}

// DiscardAndReturn is Discard but returns the queued messages (FIFO) instead
// of dropping them, so callers can clear each message's HOURGLASS "queued"
// reaction — otherwise it hangs forever after /new, /clear, panic recovery or
// a restart (#2013). Returns nil when nothing was queued.
func (q *MessageQueue) DiscardAndReturn(key string) []QueuedMsg {
	q.mu.Lock()
	defer q.mu.Unlock()
	var dropped []QueuedMsg
	if sq := q.queues[key]; sq != nil {
		sq.gen++
		dropped = sq.ring.drainAll()
		sq.busy = false
		sq.lastNotifyNs = 0
		sq.interruptRequested = false
	}
	// Mirror DoneOrDrain's LRU cleanup so a pre-Discard drop-path cooldown
	// cannot silence the first notify after Discard.
	if e, ok := q.dropNotifyIndex[key]; ok {
		q.dropNotifyLRU.Remove(e.elem)
		delete(q.dropNotifyIndex, key)
		q.releasePooledEntry(e)
	}
	return dropped
}

// Cleanup UNCONDITIONALLY deletes the map entry for key — the only public
// method allowed to break gen-monotonicity. Callers MUST ensure no in-flight
// owner can arrive on this key afterwards (a stale owner with gen 0 could
// drain a newly-enqueued batch). Intended caller: session.Router on terminal
// removal, after Discard has signalled any racing owner. No-op for unknown keys.
func (q *MessageQueue) Cleanup(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.queues, key)
	if e, ok := q.dropNotifyIndex[key]; ok {
		q.dropNotifyLRU.Remove(e.elem)
		delete(q.dropNotifyIndex, key)
		q.releasePooledEntry(e)
	}
}

// Depth returns the number of queued messages for key (excludes the active one).
func (q *MessageQueue) Depth(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if sq := q.queues[key]; sq != nil {
		return sq.ring.len()
	}
	return 0
}

// CollectDelay returns the configured collect delay.
func (q *MessageQueue) CollectDelay() time.Duration {
	return q.collectDelay
}

// ShouldNotify returns true if the 3s cooldown since the last enqueue
// notification for key has elapsed, so rapid-fire messages don't each get a
// "message received" confirmation. The drop-path cooldown uses the bounded
// list+map LRU so chat A's notify does not silence chat B's. All O(1).
//
// The cooldown-active fast path takes mu.RLock only (#1358); the expired /
// cold-key path re-checks under mu.Lock before mutating, so two goroutines
// racing through the RUnlock→Lock window yield at most one extra notify per
// window — acceptable since the cooldown is "approximately 3s".
func (q *MessageQueue) ShouldNotify(key string) bool {
	const cooldown = int64(3 * time.Second)
	now := time.Now().UnixNano()

	// Fast path: RLock-only probe; return early while the cooldown is active.
	q.mu.RLock()
	if sq, ok := q.queues[key]; ok {
		// delta < 0 means the wall clock stepped backwards; treat as expired so
		// the slow path re-anchors instead of silencing notifications forever.
		if delta := now - sq.lastNotifyNs; delta >= 0 && delta < cooldown {
			q.mu.RUnlock()
			return false
		}
	} else if entry, ok := q.dropNotifyIndex[key]; ok {
		if delta := now - entry.ts; delta >= 0 && delta < cooldown {
			q.mu.RUnlock()
			return false
		}
	}
	q.mu.RUnlock()

	// Slow path: re-check under the write lock — a sibling may have published
	// a fresh timestamp in the RUnlock→Lock window.
	q.mu.Lock()
	defer q.mu.Unlock()
	if sq, ok := q.queues[key]; ok {
		if delta := now - sq.lastNotifyNs; delta >= 0 && delta < cooldown {
			return false
		}
		sq.lastNotifyNs = now
		return true
	}
	// No queue entry — per-key cooldown via bounded LRU.
	if entry, ok := q.dropNotifyIndex[key]; ok {
		if delta := now - entry.ts; delta >= 0 && delta < cooldown {
			return false
		}
		entry.ts = now
		q.dropNotifyLRU.MoveToFront(entry.elem)
		return true
	}
	// Insert new entry; evict the LRU tail if at capacity and recycle the
	// evicted struct for this insert (#1694).
	var reuse *dropNotifyEntry
	if q.dropNotifyLRU.Len() >= dropNotifyMaxKeys {
		if oldest := q.dropNotifyLRU.Back(); oldest != nil {
			evicted := oldest.Value.(*dropNotifyEntry)
			delete(q.dropNotifyIndex, evicted.key)
			q.dropNotifyLRU.Remove(oldest)
			reuse = q.takePooledEntry(evicted)
		}
	}
	if reuse == nil {
		reuse = q.takePooledEntry(nil)
	}
	reuse.key = key
	reuse.ts = now
	reuse.elem = q.dropNotifyLRU.PushFront(reuse)
	q.dropNotifyIndex[key] = reuse
	return true
}

// --- SessionGuard compatibility ---
// The Dashboard/WS path (server/send.go) uses MessageQueue through SessionGuard.

// TryAcquire implements SessionGuard. For the message queue, this checks
// if the session is idle (not busy). Used by Dashboard path only.
func (q *MessageQueue) TryAcquire(key string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	sq := q.getOrCreate(key)
	if sq.busy {
		return false
	}
	sq.busy = true
	return true
}

// ShouldSendWait implements SessionGuard. Delegates to ShouldNotify.
func (q *MessageQueue) ShouldSendWait(key string) bool {
	return q.ShouldNotify(key)
}

// Release implements SessionGuard. Messages enqueued while a Dashboard/WS
// Guard caller held the key would otherwise be parked until the next Enqueue,
// which on a quiet key may never come (#769). With a strand handler
// registered (SetStrandHandler) Release drains them through it; without one
// they stay parked and a Warn is logged.
func (q *MessageQueue) Release(key string) {
	// Snapshot handler + depth under the lock; an Enqueue racing the unlock
	// simply lands on the now-idle key and becomes owner or re-queues.
	q.mu.Lock()
	handler := q.onStranded
	depth := 0
	if sq := q.queues[key]; sq != nil {
		depth = sq.ring.len()
	}
	q.mu.Unlock()

	if handler != nil {
		// ReleaseWithDrain clears the ring + marks the key idle before onDrain,
		// so the handler may re-enter Enqueue.
		q.ReleaseWithDrain(key, func(m QueuedMsg) { handler(key, m) })
		return
	}

	if depth > 0 {
		// No handler: keep park-in-place but make the strand visible.
		slog.Warn("msgqueue release with pending messages and no strand handler, message may be stranded until next Enqueue",
			"key", key, "pending_snapshot", depth)
	}
	q.ReleaseWithDrain(key, nil)
}

// ReleaseWithDrain is the drain-aware variant of Release: queued messages are
// handed to onDrain one at a time, FIFO, AFTER the ring is cleared, the
// session marked idle and q.mu released — so onDrain may re-enter Enqueue.
// A nil onDrain leaves messages parked for a future Enqueue owner.
func (q *MessageQueue) ReleaseWithDrain(key string, onDrain func(QueuedMsg)) {
	q.mu.Lock()
	var drained []QueuedMsg
	if sq := q.queues[key]; sq != nil {
		sq.busy = false
		if sq.ring.len() == 0 {
			delete(q.queues, key)
		} else if onDrain != nil {
			// Hand the batch to the caller and clear the ring so progress is
			// guaranteed even if no further Enqueue arrives. Reusing scratch is
			// safe: the entry is deleted below, so this ring is never reused,
			// and the batch is consumed by the out-of-lock loop before return.
			drained = sq.ring.drainInto(sq.ring.scratch)
			sq.ring.scratch = drained
			delete(q.queues, key)
		}
	}
	q.mu.Unlock()
	for _, m := range drained {
		onDrain(m)
	}
}
