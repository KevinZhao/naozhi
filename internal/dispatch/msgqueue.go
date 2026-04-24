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
)

// ParseQueueMode accepts "collect" or "interrupt" (case-insensitive) and
// returns the corresponding QueueMode. Empty / unknown strings map to
// ModeCollect so callers can feed raw YAML values without defensive checks.
func ParseQueueMode(s string) QueueMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "interrupt":
		return ModeInterrupt
	default:
		return ModeCollect
	}
}

// sessionQueue tracks per-session busy state and queued messages.
type sessionQueue struct {
	busy         bool
	gen          uint64 // incremented on Discard to invalidate stale owners
	msgs         []QueuedMsg
	lastNotifyNs int64 // unix nanoseconds of last ShouldNotify call (replaces lastNotify map)
	lastEvictNs  int64 // unix nanoseconds of last eviction Warn log (rate-limit)

	// interruptRequested is true once an interrupt has been triggered for the
	// currently running turn. Cleared by DoneOrDrain when ownership is
	// consumed for the next turn. Prevents multiple follow-up messages on the
	// same turn each firing a separate control_request (redundant and noisy
	// in CLI stderr) while still allowing a fresh interrupt on the next turn.
	interruptRequested bool
}

// MessageQueue replaces SessionGuard with per-session message queuing.
// When a session is busy, incoming messages are queued (up to MaxDepth)
// instead of being dropped. The owner goroutine drains the queue after
// each turn completes.
//
// Thread-safe: all methods acquire mu.
type MessageQueue struct {
	mu           sync.Mutex
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
	dropNotifyLRU   *list.List               // element.Value = *dropNotifyEntry
	dropNotifyIndex map[string]*list.Element // key → list element
}

// dropNotifyEntry is a single LRU entry: key + last notify nanos.
type dropNotifyEntry struct {
	key string
	ts  int64
}

// dropNotifyMaxKeys bounds dropNotifyTimes; oldest entry is evicted on insert
// when at capacity. 1024 covers realistic IM concurrency with minimal memory.
const dropNotifyMaxKeys = 1024

// evictWarnCooldownNs rate-limits the per-key "queue full" eviction Warn log
// so a sustained flood does not drown operator signals. 5s is long enough
// that the first line proves the condition but short enough that a second
// burst after recovery produces a fresh datum.
const evictWarnCooldownNs = int64(5 * time.Second)

// NewMessageQueue creates a MessageQueue in Collect mode.
// maxDepth <= 0 disables queuing (degrades to drop+wait, same as old Guard).
func NewMessageQueue(maxDepth int, collectDelay time.Duration) *MessageQueue {
	return NewMessageQueueWithMode(maxDepth, collectDelay, ModeCollect)
}

// NewMessageQueueWithMode creates a MessageQueue with an explicit queue mode.
// See QueueMode for the semantic difference between Collect and Interrupt.
func NewMessageQueueWithMode(maxDepth int, collectDelay time.Duration, mode QueueMode) *MessageQueue {
	return &MessageQueue{
		queues:          make(map[string]*sessionQueue),
		maxDepth:        maxDepth,
		collectDelay:    collectDelay,
		mode:            mode,
		dropNotifyLRU:   list.New(),
		dropNotifyIndex: make(map[string]*list.Element),
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

	// Evict oldest if at capacity. Shift in place rather than `sq.msgs[1:]`
	// so the underlying array stays bounded at cap=maxDepth instead of
	// drifting forward indefinitely; also zeroes the evicted slot so
	// any held image data can be GC'd.
	if len(sq.msgs) >= q.maxDepth {
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
				"key", key, "depth", len(sq.msgs), "max_depth", q.maxDepth)
			sq.lastEvictNs = now
		}
		copy(sq.msgs, sq.msgs[1:])
		sq.msgs[len(sq.msgs)-1] = QueuedMsg{}
		sq.msgs = sq.msgs[:len(sq.msgs)-1]
	}
	sq.msgs = append(sq.msgs, msg)
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

	if len(sq.msgs) == 0 {
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
		if elem, ok := q.dropNotifyIndex[key]; ok {
			q.dropNotifyLRU.Remove(elem)
			delete(q.dropNotifyIndex, key)
		}
		return nil
	}

	// Drain all; keep ownership. Clearing interruptRequested here is crucial:
	// once the owner takes the drained batch, the NEXT in-flight turn is a
	// fresh target for a future interrupt, so subsequent queued messages
	// during that new turn must be able to trigger another control_request.
	msgs := sq.msgs
	sq.msgs = nil
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
		sq.msgs = nil
		sq.busy = false
		sq.lastNotifyNs = 0
		sq.interruptRequested = false
	}
	// Mirror DoneOrDrain's LRU cleanup: a pre-Discard drop-path cooldown
	// (chat entered via /new after being idle for >3s) would otherwise keep
	// its stale timestamp and silence the first legitimate notify after
	// Discard. Safe to delete even if no entry exists.
	if elem, ok := q.dropNotifyIndex[key]; ok {
		q.dropNotifyLRU.Remove(elem)
		delete(q.dropNotifyIndex, key)
	}
}

// Depth returns the number of queued messages for key (excludes the active one).
func (q *MessageQueue) Depth(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if sq := q.queues[key]; sq != nil {
		return len(sq.msgs)
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
func (q *MessageQueue) ShouldNotify(key string) bool {
	const cooldown = int64(3 * time.Second)
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now().UnixNano()
	if sq, ok := q.queues[key]; ok {
		// Guard against NTP backwards-step: if now < lastNotifyNs the int64
		// subtraction yields a negative value which is < positive cooldown,
		// silencing notifications indefinitely. Treat any non-monotonic
		// jump as "cooldown satisfied" and reset the anchor.
		if delta := now - sq.lastNotifyNs; delta >= 0 && delta < cooldown {
			return false
		}
		sq.lastNotifyNs = now
		return true
	}
	// No queue entry — per-key cooldown via bounded LRU.
	if elem, ok := q.dropNotifyIndex[key]; ok {
		entry := elem.Value.(*dropNotifyEntry)
		if delta := now - entry.ts; delta >= 0 && delta < cooldown {
			return false
		}
		entry.ts = now
		// Refresh LRU ordering — most-recently used to front.
		q.dropNotifyLRU.MoveToFront(elem)
		return true
	}
	// Insert new entry; evict the LRU tail if at capacity.
	if q.dropNotifyLRU.Len() >= dropNotifyMaxKeys {
		if oldest := q.dropNotifyLRU.Back(); oldest != nil {
			entry := oldest.Value.(*dropNotifyEntry)
			delete(q.dropNotifyIndex, entry.key)
			q.dropNotifyLRU.Remove(oldest)
		}
	}
	elem := q.dropNotifyLRU.PushFront(&dropNotifyEntry{key: key, ts: now})
	q.dropNotifyIndex[key] = elem
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

// Release implements SessionGuard. Releases ownership without draining.
func (q *MessageQueue) Release(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if sq := q.queues[key]; sq != nil {
		sq.busy = false
		if len(sq.msgs) == 0 {
			delete(q.queues, key)
		}
	}
}
