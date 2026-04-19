package dispatch

import (
	"container/list"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// QueuedMsg holds a single message waiting to be processed.
type QueuedMsg struct {
	Text      string
	Images    []cli.ImageData
	EnqueueAt time.Time
}

// sessionQueue tracks per-session busy state and queued messages.
type sessionQueue struct {
	busy         bool
	gen          uint64 // incremented on Discard to invalidate stale owners
	msgs         []QueuedMsg
	lastNotifyNs int64 // unix nanoseconds of last ShouldNotify call (replaces lastNotify map)
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

// NewMessageQueue creates a MessageQueue.
// maxDepth <= 0 disables queuing (degrades to drop+wait, same as old Guard).
func NewMessageQueue(maxDepth int, collectDelay time.Duration) *MessageQueue {
	return &MessageQueue{
		queues:          make(map[string]*sessionQueue),
		maxDepth:        maxDepth,
		collectDelay:    collectDelay,
		dropNotifyLRU:   list.New(),
		dropNotifyIndex: make(map[string]*list.Element),
	}
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
//   - isOwner=false, enqueued=false: queue is disabled (maxDepth<=0).
//     Caller should reply "please wait".
func (q *MessageQueue) Enqueue(key string, msg QueuedMsg) (isOwner, enqueued bool, gen uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()

	sq := q.getOrCreate(key)
	if !sq.busy {
		sq.busy = true
		return true, false, sq.gen
	}

	// maxDepth<=0: degrade to drop (backward-compatible with old Guard behavior).
	if q.maxDepth <= 0 {
		return false, false, 0
	}

	// Evict oldest if at capacity. Shift in place rather than `sq.msgs[1:]`
	// so the underlying array stays bounded at cap=maxDepth instead of
	// drifting forward indefinitely; also zeroes the evicted slot so
	// any held image data can be GC'd.
	if len(sq.msgs) >= q.maxDepth {
		copy(sq.msgs, sq.msgs[1:])
		sq.msgs[len(sq.msgs)-1] = QueuedMsg{}
		sq.msgs = sq.msgs[:len(sq.msgs)-1]
	}
	sq.msgs = append(sq.msgs, msg)
	return false, true, 0
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
		// Release ownership.
		delete(q.queues, key)
		return nil
	}

	// Drain all; keep ownership.
	msgs := sq.msgs
	sq.msgs = nil
	return msgs
}

// Discard clears all queued messages and releases ownership for key.
// Bumps the generation so stale ownerLoops stop on their next DoneOrDrain.
// Used when the user sends /new or /stop.
func (q *MessageQueue) Discard(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if sq := q.queues[key]; sq != nil {
		sq.gen++
		sq.msgs = nil
		sq.busy = false
		sq.lastNotifyNs = 0
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
		if now-sq.lastNotifyNs < cooldown {
			return false
		}
		sq.lastNotifyNs = now
		return true
	}
	// No queue entry — per-key cooldown via bounded LRU.
	if elem, ok := q.dropNotifyIndex[key]; ok {
		entry := elem.Value.(*dropNotifyEntry)
		if now-entry.ts < cooldown {
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
