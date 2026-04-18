package dispatch

import (
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
	busy bool
	gen  uint64 // incremented on Discard to invalidate stale owners
	msgs []QueuedMsg
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

	// Rate-limit "enqueued" notifications (same semantics as Guard.ShouldSendWait).
	lastNotify map[string]time.Time
}

// NewMessageQueue creates a MessageQueue.
// maxDepth <= 0 disables queuing (degrades to drop+wait, same as old Guard).
func NewMessageQueue(maxDepth int, collectDelay time.Duration) *MessageQueue {
	return &MessageQueue{
		queues:       make(map[string]*sessionQueue),
		maxDepth:     maxDepth,
		collectDelay: collectDelay,
		lastNotify:   make(map[string]time.Time),
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

	// Evict oldest if at capacity.
	if len(sq.msgs) >= q.maxDepth {
		sq.msgs = sq.msgs[1:]
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
		sq.busy = false
		delete(q.queues, key)
		delete(q.lastNotify, key)
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
	}
	delete(q.lastNotify, key)
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
func (q *MessageQueue) ShouldNotify(key string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if time.Since(q.lastNotify[key]) < 3*time.Second {
		return false
	}
	q.lastNotify[key] = time.Now()
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
	delete(q.lastNotify, key)
}
