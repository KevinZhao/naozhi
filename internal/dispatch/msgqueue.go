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
	Images []cli.ImageData
	// MessageID is the platform-native inbound message ID (optional).
	// Dispatch uses it to add/remove a reaction on the user's original
	// message as a non-intrusive "queued" acknowledgement. Empty when the
	// platform doesn't report an ID or isn't Reactor-capable.
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
	// ModeInterrupt queues the new messages AND asks the dispatcher to send
	// an in-band control_request to the CLI so the active turn aborts
	// immediately. The queued messages are then coalesced and sent as the
	// next prompt on the same live process. Fastest user-facing pivot, but
	// burns the tokens already spent on the aborted turn.
	ModeInterrupt
	// ModePassthrough writes each user message directly to the CLI and lets
	// the CLI's own commandQueue handle merging. Every message gets an
	// independent result (or a merged-group result with head/follower
	// semantics). Requires Protocol.SupportsReplay()==true; sessions whose
	// protocol can't provide replay events silently fall back to ModeCollect.
	// See docs/rfc/passthrough-mode.md.
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
	// ring holds the queued messages in a fixed-capacity FIFO ring buffer.
	// O(1) push (with O(1) head-advance eviction when full) replaces the
	// O(N) slice-memmove that #570 R247-PERF-23 flagged at maxDepth=16
	// every Enqueue under sustained backpressure. See msgRing for layout.
	ring         msgRing
	lastNotifyNs int64 // unix nanoseconds of last ShouldNotify call (replaces lastNotify map)
	lastEvictNs  int64 // unix nanoseconds of last eviction Warn log (rate-limit)

	// interruptRequested is true once an interrupt has been triggered for the
	// currently running turn. Cleared by DoneOrDrain when ownership is
	// consumed for the next turn. Prevents multiple follow-up messages on the
	// same turn each firing a separate control_request (redundant and noisy
	// in CLI stderr) while still allowing a fresh interrupt on the next turn.
	interruptRequested bool
}

// msgRing is a single-producer / single-consumer FIFO ring buffer used by
// sessionQueue.msgs. All access is serialised under MessageQueue.mu — the
// ring carries no internal locking. The capacity is fixed by the first
// push (set to MessageQueue.maxDepth via push's `cap` argument) so once
// allocated the backing array never grows; eviction-on-full is an O(1)
// head advance (with the evicted slot zeroed for GC) instead of the
// previous slice-memmove. R247-PERF-23 (#570).
//
// Empty / one-shot semantics:
//   - len()==0 and the underlying buf is nil → ring has never been used.
//     Callers that treat the ring as logically absent (Depth -> 0,
//     DoneOrDrain "no messages" branch) can detect this via len()==0.
//   - drainAll resets the ring to the empty-and-cleared state but keeps
//     the backing array, so subsequent pushes reuse it without realloc.
//
// The buffer is laid out as:
//
//	buf:   [_, A, B, C, _, _]
//	head=1, used=3 → logical view = [A, B, C]
//
// push at full advances head and writes at (head+used)%cap, dropping A.
type msgRing struct {
	buf  []QueuedMsg
	head int // index of the oldest live element when used > 0
	used int // number of live elements; 0 <= used <= cap(buf)
}

// len returns the current number of queued messages.
func (r *msgRing) len() int { return r.used }

// push appends m. When the ring is at the supplied capacity (cap, the
// MessageQueue's maxDepth), the head is advanced and the oldest element
// is overwritten — returns true to signal an eviction so the caller can
// emit the rate-limited warn log. The first push allocates the backing
// array at the requested capacity; subsequent pushes reuse it.
func (r *msgRing) push(m QueuedMsg, capacity int) (evicted bool) {
	if cap(r.buf) == 0 {
		r.buf = make([]QueuedMsg, capacity)
	}
	if r.used == capacity {
		// Full: drop oldest. Zero the slot first so any held image data
		// becomes GC-eligible immediately.
		r.buf[r.head] = QueuedMsg{}
		r.head = (r.head + 1) % capacity
		r.used--
		evicted = true
	}
	idx := (r.head + r.used) % capacity
	r.buf[idx] = m
	r.used++
	return evicted
}

// drainAll returns the queued messages in FIFO order and resets the ring
// to empty (head=used=0). The backing array is retained for reuse but
// each consumed slot is zeroed so retained image data becomes
// GC-eligible. Returns nil when empty (mirrors the previous nil-vs-slice
// distinction the queue exposed via sq.msgs == nil).
func (r *msgRing) drainAll() []QueuedMsg {
	if r.used == 0 {
		return nil
	}
	out := make([]QueuedMsg, r.used)
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

// MessageQueue replaces SessionGuard with per-session message queuing.
// When a session is busy, incoming messages are queued (up to MaxDepth)
// instead of being dropped. The owner goroutine drains the queue after
// each turn completes.
//
// Thread-safe: all mutating methods acquire mu (write lock); ShouldNotify's
// fast cooldown-active path takes mu.RLock() only — see R20260528-PERF-19
// (#1358). Switching from sync.Mutex to sync.RWMutex is non-breaking
// because RWMutex.Lock/Unlock match Mutex's surface; existing call sites
// that use Lock continue to serialise correctly.
type MessageQueue struct {
	mu           sync.RWMutex
	queues       map[string]*sessionQueue
	maxDepth     int
	collectDelay time.Duration
	mode         QueueMode

	// dropNotifyTimes is a bounded per-key cooldown map for notifies that
	// happen when no sessionQueue exists (the drop path with maxDepth<=0
	// or the window between Discard and a new owner). Keeping per-key state
	// avoids cross-user interference where one chat's notify silences
	// another's; the map is capped to dropNotifyMaxKeys via LRU eviction.
	//
	// Implementation: a classic list+map LRU. List front holds the most
	// recent insertion/update; back holds the least recent. This yields O(1)
	// insert, refresh, and evict — critical since ShouldNotify runs under
	// mu on the IM hot path.
	// dropNotifyLRU orders entries by recency (front = most recent) purely for
	// O(1) eviction. dropNotifyIndex maps key → *dropNotifyEntry directly so the
	// hot ShouldNotify probe reads the timestamp off a concrete struct instead
	// of paying a `list.Element.Value.(*dropNotifyEntry)` interface assertion on
	// every IM message (R249-PERF-12, #932). Each entry keeps a back-pointer to
	// its list element so refresh/evict can splice the list without a second
	// map lookup.
	dropNotifyLRU   *list.List                  // element.Value = *dropNotifyEntry
	dropNotifyIndex map[string]*dropNotifyEntry // key → entry
}

// dropNotifyEntry is a single LRU entry: key + last notify nanos. elem links
// back to the *list.Element that boxes this entry in dropNotifyLRU.
type dropNotifyEntry struct {
	key  string
	ts   int64
	elem *list.Element
}

// dropNotifyMaxKeys bounds dropNotifyTimes; oldest entry is evicted on insert
// when at capacity. 1024 covers realistic IM concurrency with minimal memory.
const dropNotifyMaxKeys = 1024

// evictWarnCooldownNs rate-limits the per-key "queue full" eviction Warn log
// so a sustained flood does not drown operator signals. 5s is long enough
// that the first line proves the condition but short enough that a second
// burst after recovery produces a fresh datum.
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

// Enqueue adds a message for key.
//
// Returns:
//   - isOwner=true:  caller becomes the owner goroutine, should process the
//     message directly (queue was idle). gen is the generation cookie.
//   - isOwner=false, enqueued=true: message was appended to the queue.
//     shouldInterrupt=true when mode is ModeInterrupt and this is the first
//     follow-up for the currently running turn — the caller should trigger
//     an in-band CLI interrupt so the active turn aborts promptly.
//   - isOwner=false, enqueued=false: queue is disabled (maxDepth<=0).
//     Caller should reply "please wait".
func (q *MessageQueue) Enqueue(key string, msg QueuedMsg) (isOwner, enqueued, shouldInterrupt bool, gen uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()

	sq := q.getOrCreate(key)
	if !sq.busy {
		sq.busy = true
		return true, false, false, sq.gen
	}

	// maxDepth<=0: degrade to drop (backward-compatible with old Guard behavior).
	if q.maxDepth <= 0 {
		return false, false, false, 0
	}

	// Push into the ring buffer. Eviction on full is O(1) (head advance);
	// the previous slice-memmove was O(N) on every push at full depth.
	// R247-PERF-23 (#570).
	if evicted := sq.ring.push(msg, q.maxDepth); evicted {
		// Queue-full eviction is silent data loss: the user that sent the
		// evicted message gets no feedback. Log at Warn so operators can
		// observe backpressure (single chat overwhelmed, or CLI hung).
		// Rate-limit per key to 1/5s so a sustained flood does not drown
		// the log; a single log line is enough to prove the condition, and
		// repeated lines add no operator signal once the alert fires.
		now := time.Now().UnixNano()
		// delta < 0 means NTP stepped backwards (wall-clock moved into the
		// past). Without re-anchoring lastEvictNs to `now`, the next check
		// would again see delta < 0 and log, defeating the rate-limit; we
		// also need to update the anchor in the fire path below. Mirrors
		// the pattern in ShouldNotify.
		if delta := now - sq.lastEvictNs; delta < 0 || delta >= evictWarnCooldownNs {
			slog.Warn("msgqueue: dropping oldest message (queue full)",
				"key", key, "depth", sq.ring.len(), "max_depth", q.maxDepth)
			sq.lastEvictNs = now
		}
	}
	// In Interrupt mode the first queued follow-up for the active turn flips
	// interruptRequested. Subsequent queued messages for the same turn skip
	// the interrupt — the first control_request already cancels the turn,
	// and the CLI would ignore a second one mid-abort.
	if q.mode == ModeInterrupt && !sq.interruptRequested {
		sq.interruptRequested = true
		return false, true, true, 0
	}
	return false, true, false, 0
}

// DoneOrDrain is called by the owner goroutine after processing a message.
//
// gen must match the generation returned by Enqueue; a mismatch means
// Discard was called (e.g., /new) and a new owner may have started.
// The stale owner should stop its loop.
//
// If the queue is empty (or gen mismatches), ownership is released and nil is returned.
// If the queue has messages, all are drained and returned; ownership is kept.
//
// Atomicity is critical: the check-and-release must happen under the same
// lock to prevent a message from being enqueued between check and release,
// which would leave it stranded (no owner to process it).
func (q *MessageQueue) DoneOrDrain(key string, gen uint64) []QueuedMsg {
	q.mu.Lock()
	defer q.mu.Unlock()

	sq := q.queues[key]
	if sq == nil {
		// Entry was discarded while we were processing.
		return nil
	}

	// Generation mismatch: Discard was called and possibly a new owner started.
	// Stale owner must stop. Do NOT release ownership — the new owner holds it.
	if sq.gen != gen {
		return nil
	}

	if sq.ring.len() == 0 {
		// Release ownership. Also purge any stale dropNotify LRU entry so
		// the next notification goes through a consistent cooldown path:
		// otherwise ShouldNotify would fall from the queue branch (which
		// uses sq.lastNotifyNs) to the LRU branch (which still has a stale
		// timestamp from before this session was queued), silencing a
		// legitimate notification.
		//
		// Note: deleting the map entry implicitly drops interruptRequested
		// (getOrCreate allocates a fresh sessionQueue on the next Enqueue).
		// We zero the field explicitly anyway so any future refactor that
		// reuses the *sessionQueue instance cannot silently suppress the
		// first interrupt of the next turn.
		sq.interruptRequested = false
		delete(q.queues, key)
		if e, ok := q.dropNotifyIndex[key]; ok {
			q.dropNotifyLRU.Remove(e.elem)
			delete(q.dropNotifyIndex, key)
		}
		return nil
	}

	// Drain all; keep ownership. Clearing interruptRequested here is crucial:
	// once the owner takes the drained batch, the NEXT in-flight turn is a
	// fresh target for a future interrupt, so subsequent queued messages
	// during that new turn must be able to trigger another control_request.
	msgs := sq.ring.drainAll()
	sq.interruptRequested = false
	return msgs
}

// Discard clears all queued messages and releases ownership for key.
// Bumps the generation so stale ownerLoops stop on their next DoneOrDrain.
// Used when the user sends /new or /stop.
//
// The generation bump MUST persist in the map so a concurrent Enqueue that
// becomes the new owner picks up gen+1 rather than starting from gen=0 and
// colliding with the stale owner's check. We therefore keep the entry
// around; panic-recovery callers do not leave orphaned entries in practice
// because a subsequent Enqueue reuses this same sessionQueue.
func (q *MessageQueue) Discard(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if sq := q.queues[key]; sq != nil {
		sq.gen++
		sq.ring.reset()
		sq.busy = false
		sq.lastNotifyNs = 0
		sq.interruptRequested = false
	}
	// Mirror DoneOrDrain's LRU cleanup: a pre-Discard drop-path cooldown
	// (chat entered via /new after being idle for >3s) would otherwise keep
	// its stale timestamp and silence the first legitimate notify after
	// Discard. Safe to delete even if no entry exists.
	if e, ok := q.dropNotifyIndex[key]; ok {
		q.dropNotifyLRU.Remove(e.elem)
		delete(q.dropNotifyIndex, key)
	}
}

// Cleanup UNCONDITIONALLY deletes the map entry for key — the only public
// method allowed to break gen-monotonicity. Callers MUST ensure no in-flight
// owner can arrive on this key afterwards (otherwise a stale owner whose gen
// equals the from-scratch 0 could drain a newly-enqueued batch). Intended
// caller: session.Router on user-initiated terminal removal (Reset/Remove),
// where the preceding Discard already signalled any racing owner to stop.
// No-op for unknown keys. Also clears the dropNotifyLRU entry for key.
func (q *MessageQueue) Cleanup(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.queues, key)
	if e, ok := q.dropNotifyIndex[key]; ok {
		q.dropNotifyLRU.Remove(e.elem)
		delete(q.dropNotifyIndex, key)
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

// ShouldNotify returns true if enough time (3s) has passed since the last
// enqueue notification for key. Prevents spamming users with "message received"
// confirmations when they send many messages in quick succession.
//
// Must not leak map entries: the drop-path cooldown uses a bounded list+map
// LRU so chat A's notify does not silence chat B's without unbounded growth
// on the maxDepth<=0 code path. All operations here are O(1).
//
// R20260528-PERF-19 (#1358): the cooldown-active fast path takes mu.RLock
// only. The vast majority of busy-chat ShouldNotify calls land in the
// "still inside the 3s cooldown" branch — that observation has no side
// effects (no timestamp update, no LRU mutation), so a read lock is
// sufficient and concurrent IM messages on different chats no longer
// serialise on the write lock. Cooldown-expired or cold-key paths fall
// through to the original Lock+Unlock branch where mutation happens.
// Read-then-write is racy with another goroutine racing to Lock between
// our RUnlock and Lock: the second goroutine may also see the cooldown
// expired under RLock and both will try to fire the notify. The Lock
// branch re-checks the cooldown under the write lock, so the second
// goroutine observes the just-published timestamp and silently returns
// false — at most one extra observation per cooldown window per key,
// which the user-facing semantics already tolerate (the cooldown is
// "approximately 3s", not strictly monotonic).
func (q *MessageQueue) ShouldNotify(key string) bool {
	const cooldown = int64(3 * time.Second)
	now := time.Now().UnixNano()

	// Fast path: RLock-only probe. Returns false (no notify) when the
	// cooldown is clearly active. Returns from the read-lock window so
	// we do not promote to the write lock for the common busy-chat case.
	q.mu.RLock()
	if sq, ok := q.queues[key]; ok {
		// Guard against NTP backwards-step: if now < lastNotifyNs the int64
		// subtraction yields a negative value which is < positive cooldown,
		// silencing notifications indefinitely. Treat any non-monotonic
		// jump as "cooldown satisfied" and fall through to the slow path
		// where the anchor gets reset under the write lock.
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

	// Slow path: the cooldown is expired (or the key is cold). Acquire
	// the write lock and re-check under it before mutating; a sibling
	// goroutine may have raced through the same RUnlock→Lock window and
	// already published a fresh timestamp, in which case we observe the
	// new value and return false. At-most-one-extra-fire-per-window is
	// the bounded race outcome — see method godoc above.
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
		// Refresh LRU ordering — most-recently used to front.
		q.dropNotifyLRU.MoveToFront(entry.elem)
		return true
	}
	// Insert new entry; evict the LRU tail if at capacity.
	if q.dropNotifyLRU.Len() >= dropNotifyMaxKeys {
		if oldest := q.dropNotifyLRU.Back(); oldest != nil {
			delete(q.dropNotifyIndex, oldest.Value.(*dropNotifyEntry).key)
			q.dropNotifyLRU.Remove(oldest)
		}
	}
	entry := &dropNotifyEntry{key: key, ts: now}
	entry.elem = q.dropNotifyLRU.PushFront(entry)
	q.dropNotifyIndex[key] = entry
	return true
}

// --- SessionGuard compatibility ---
// These methods implement the SessionGuard interface so the Dashboard/WS
// path (server/send.go) can continue using Guard without changes.

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

// Release implements SessionGuard. Releases ownership without draining
// — internally it calls ReleaseWithDrain(key, nil), which clears the
// busy flag but leaves any queued messages parked in the sessionQueue
// for a future Enqueue owner to consume via DoneOrDrain.  This is the
// SessionGuard-compatible path; it is *not* a drain failure.
//
// R37-REL1: if messages landed during the busy window (concurrent
// Enqueue while Dashboard/WS Guard held the session), they would
// otherwise be stuck until the next Enqueue re-entered the queue.
// Callers that can process the drained batch should use
// ReleaseWithDrain instead.
func (q *MessageQueue) Release(key string) {
	// Peek depth under the lock so we can warn callers about stranded messages
	// without changing Release's no-drain contract. Without this log the only
	// signal is a silent "queue appears to lose messages" user report.
	q.mu.Lock()
	depth := 0
	if sq := q.queues[key]; sq != nil {
		depth = sq.ring.len()
	}
	q.mu.Unlock()
	if depth > 0 {
		// `pending` is a lock-release snapshot — Enqueue callers racing this
		// unlock can shift the real depth. Accurate enough for "a caller
		// stranded N+ messages" triage.
		slog.Warn("msgqueue release with pending messages, use ReleaseWithDrain to avoid strand",
			"key", key, "pending_snapshot", depth)
	}
	q.ReleaseWithDrain(key, nil)
}

// ReleaseWithDrain is the drain-aware variant of Release. If messages are
// queued when ownership is released, onDrain is invoked once per message in
// FIFO order while the internal queue state has already been cleared and the
// session marked idle — so the callback can safely re-enter Enqueue or
// otherwise process each message without re-acquiring q.mu re-entrantly.
//
// onDrain may be nil; in that case behaviour matches the legacy Release
// (messages stay in sq.msgs waiting for a future Enqueue owner to sweep
// them via DoneOrDrain).
//
// Callback invocation happens AFTER the queue state is cleared and the lock
// released, mirroring DoneOrDrain's out-of-lock delivery contract.
func (q *MessageQueue) ReleaseWithDrain(key string, onDrain func(QueuedMsg)) {
	q.mu.Lock()
	var drained []QueuedMsg
	if sq := q.queues[key]; sq != nil {
		sq.busy = false
		if sq.ring.len() == 0 {
			delete(q.queues, key)
		} else if onDrain != nil {
			// Transfer the queued batch to the caller and clear the
			// internal ring so a later Enqueue starts fresh. Ownership
			// is released (busy=false) so the next Enqueue becomes
			// owner; if we kept the msgs in place, that owner would
			// still receive them via DoneOrDrain — but nothing
			// guarantees a next Enqueue arrives. Draining here ensures
			// progress even on a quiet session.
			drained = sq.ring.drainAll()
			// Entry becomes eligible for deletion now that it carries no
			// queued state; mirroring the empty branch above keeps the map
			// from accumulating idle sessionQueue instances.
			delete(q.queues, key)
		}
	}
	q.mu.Unlock()
	for _, m := range drained {
		onDrain(m)
	}
}
