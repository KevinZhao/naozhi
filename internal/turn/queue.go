// Package turn runs a session key's turns: the per-session message queue,
// the merge of queued messages into one prompt, and the Orchestrator that
// owns the drain loop and delivers each turn's outcome to the entry points
// whose messages it carried. The IM dispatcher and the dashboard send engine
// submit to one Orchestrator, which owns the queue, built once in the server
// composition root (#3004).
package turn

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// Msg holds a single message waiting to be processed.
type Msg struct {
	Text      string
	Images    []clievent.Attachment
	EnqueueAt time.Time
	// Origin is the submitting entry point; a nil Origin is silent.
	Origin Origin
	// TraceID is the request's trace id (Request.TraceID, resolved).
	TraceID string
}

// Mode selects how new messages that arrive while a session is busy are
// handled.
type Mode int

const (
	// ModeCollect queues the new messages and waits for the active turn to
	// finish naturally; after a short settle delay the queued messages are
	// coalesced into a single follow-up prompt. Lowest cost, highest latency.
	ModeCollect Mode = iota
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

// ParseMode accepts "collect" / "interrupt" / "passthrough"
// (case-insensitive). Empty or unknown strings map to ModeCollect so callers
// can feed raw YAML values without defensive checks.
func ParseMode(s string) Mode {
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
	// gen is drawn from queue.genSeq, so it is unique across every entry the
	// queue ever creates: an old owner's gen never matches a later entry.
	gen          uint64
	msgs         []Msg // queued messages, FIFO; at most queue.maxDepth
	lastNotifyNs int64 // unix nanoseconds of last ShouldNotify call
	lastEvictNs  int64 // unix nanoseconds of last eviction Warn log (rate-limit)

	// interruptRequested is set once an interrupt fired for the running turn
	// and cleared by DoneOrDrain, so follow-ups on the same turn don't each
	// send a redundant control_request.
	interruptRequested bool
}

// queue implements per-session message queuing: when a session is busy,
// incoming messages are queued (up to MaxDepth) instead of dropped and the
// owner goroutine drains the queue after each turn. Only the Orchestrator
// that New built it for holds one.
//
// Thread-safe: queues, genSeq and dropNotify are guarded by mu.
type queue struct {
	mu           sync.Mutex
	queues       map[string]*sessionQueue
	maxDepth     int
	collectDelay time.Duration
	mode         Mode
	// genSeq is the last generation handed to a sessionQueue.
	genSeq uint64

	// dropNotify holds key → last notify nanos, the per-key cooldown for
	// notifies when no sessionQueue exists (maxDepth<=0 drop path, or between
	// a discard and a new owner), so one chat's notify never silences another's.
	dropNotify map[string]int64
}

// dropNotifyMaxKeys bounds dropNotify; inserting past it first drops the
// entries whose cooldown expired, then everything if none had. A full table
// still all in cooldown therefore costs at most one extra receipt per key.
const dropNotifyMaxKeys = 1024

// evictWarnCooldownNs rate-limits the per-key "queue full" eviction Warn so a
// sustained flood does not drown operator signals.
const evictWarnCooldownNs = int64(5 * time.Second)

// QueueOptions configures the queue New builds for its Orchestrator.
type QueueOptions struct {
	// MaxDepth caps the messages queued per key; <=0 disables queuing, so a
	// request for a busy key is answered AckDropped.
	MaxDepth int
	// CollectDelay is how long the owner loop waits after a turn for more
	// messages before it drains the queue.
	CollectDelay time.Duration
	Mode         Mode
}

func newQueue(o QueueOptions) *queue {
	return &queue{
		queues:       make(map[string]*sessionQueue),
		maxDepth:     o.MaxDepth,
		collectDelay: o.CollectDelay,
		mode:         o.Mode,
		dropNotify:   make(map[string]int64),
	}
}

// Mode returns the configured queue mode.
func (q *queue) Mode() Mode {
	return q.mode
}

// getOrCreate returns the sessionQueue for key, creating one with a fresh
// generation if needed. Caller must hold mu.
func (q *queue) getOrCreate(key string) *sessionQueue {
	sq := q.queues[key]
	if sq == nil {
		sq = &sessionQueue{gen: q.nextGen()}
		q.queues[key] = sq
	}
	return sq
}

// detachedGen is the gen of a turn that holds no key: nextGen never returns it.
const detachedGen uint64 = 0

// nextGen returns a generation no sessionQueue has held. Caller must hold mu.
func (q *queue) nextGen() uint64 {
	q.genSeq++
	return q.genSeq
}

// enqueueResult is Enqueue's answer:
//   - isOwner: the key was idle and the caller now owns it; gen is the
//     generation cookie its DoneOrDrain calls must pass.
//   - enqueued: appended behind the owner; shouldInterrupt is set in
//     ModeInterrupt for the running turn's first follow-up.
//   - neither: the queue is disabled (maxDepth<=0).
//
// evicted reports that the oldest queued message, dropped, was pushed out to
// make room; its Origin is told (#1945).
type enqueueResult struct {
	isOwner, enqueued, shouldInterrupt bool
	gen                                uint64
	evicted                            bool
	dropped                            Msg
}

// Enqueue adds msg for key; see enqueueResult.
func (q *queue) Enqueue(key string, msg Msg) enqueueResult {
	q.mu.Lock()
	defer q.mu.Unlock()

	sq := q.getOrCreate(key)
	if !sq.busy {
		sq.busy = true
		return enqueueResult{isOwner: true, gen: sq.gen}
	}

	// maxDepth<=0: queue disabled, degrade to drop.
	if q.maxDepth <= 0 {
		return enqueueResult{}
	}

	r := enqueueResult{enqueued: true}
	if len(sq.msgs) >= q.maxDepth {
		// Drop the oldest, zeroing its slot so held image data is freed (#1945).
		r.evicted, r.dropped = true, sq.msgs[0]
		sq.msgs[0] = Msg{}
		sq.msgs = sq.msgs[1:]
	}
	sq.msgs = append(sq.msgs, msg)
	if r.evicted {
		// Queue-full eviction is silent data loss for the sender; Warn so
		// operators can observe backpressure, rate-limited per key.
		now := time.Now().UnixNano()
		// delta < 0 means the wall clock stepped backwards; re-anchor so the
		// rate-limit is not defeated (mirrors ShouldNotify).
		if delta := now - sq.lastEvictNs; delta < 0 || delta >= evictWarnCooldownNs {
			slog.Warn("msgqueue: dropping oldest message (queue full)",
				"key", key, "depth", len(sq.msgs), "max_depth", q.maxDepth)
			sq.lastEvictNs = now
		}
	}
	// Only the first queued follow-up of the active turn fires the interrupt;
	// the CLI would ignore a second control_request mid-abort.
	if q.mode == ModeInterrupt && !sq.interruptRequested {
		sq.interruptRequested = true
		r.shouldInterrupt = true
	}
	return r
}

// DoneOrDrain is called by the owner goroutine after processing a message.
// gen must match the generation returned by Enqueue; a missing entry or a
// mismatch means the entry was discarded or recreated since, and a later owner
// may hold it — the stale owner must stop. nil is returned then, and on an
// empty queue (which releases ownership); otherwise all messages are drained
// and returned and ownership kept.
// The check-and-release MUST happen under one lock so a message cannot be
// enqueued between check and release and be stranded without an owner.
func (q *queue) DoneOrDrain(key string, gen uint64) []Msg {
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

	if len(sq.msgs) == 0 {
		// Release ownership. Also purge any stale dropNotify entry so the
		// next ShouldNotify doesn't fall through to a stale timestamp and
		// silence a legitimate notification. interruptRequested is zeroed
		// explicitly in case a future refactor reuses the *sessionQueue.
		sq.interruptRequested = false
		delete(q.queues, key)
		delete(q.dropNotify, key)
		return nil
	}

	// Drain all; keep ownership. Clearing interruptRequested makes the next
	// in-flight turn a fresh interrupt target.
	sq.interruptRequested = false
	return takeMsgs(sq)
}

// takeMsgs returns sq's queued messages FIFO and empties the queue; nil when
// none were queued (the owner loop stops on a nil batch).
func takeMsgs(sq *sessionQueue) []Msg {
	if len(sq.msgs) == 0 {
		return nil
	}
	out := sq.msgs
	sq.msgs = nil
	return out
}

// DiscardAndReturn clears key's queued messages and releases ownership,
// giving the entry a fresh generation so a stale owner loop stops on its next
// DoneOrDrain (/new, /clear, a detached turn's panic, Submit's shutdown
// drop when Admit fails). The entry is kept; deleting it would be equally
// safe. The discarded messages come back FIFO so each origin can be told
// (#2013); nil when nothing was queued.
func (q *queue) DiscardAndReturn(key string) []Msg {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.discardLocked(key, q.queues[key])
}

// DiscardOwned is DiscardAndReturn on behalf of the owner holding gen. An
// entry with another gen belongs to a later owner, whose queue a stale owner
// must not discard: it is left alone. With no entry only the drop-path
// cooldown is purged, as DiscardAndReturn would.
func (q *queue) DiscardOwned(key string, gen uint64) []Msg {
	q.mu.Lock()
	defer q.mu.Unlock()
	sq := q.queues[key]
	if sq != nil && sq.gen != gen {
		return nil
	}
	return q.discardLocked(key, sq)
}

// discardLocked is DiscardAndReturn's body; sq is key's entry, nil if
// none. Caller must hold mu.
func (q *queue) discardLocked(key string, sq *sessionQueue) []Msg {
	var dropped []Msg
	if sq != nil {
		sq.gen = q.nextGen()
		dropped = takeMsgs(sq)
		sq.busy = false
		sq.lastNotifyNs = 0
		sq.interruptRequested = false
	}
	// Mirror DoneOrDrain's cleanup so a pre-discard drop-path cooldown
	// cannot silence the first notify after a discard.
	delete(q.dropNotify, key)
	return dropped
}

// Cleanup deletes the map entry for key and returns its queued messages FIFO
// (nil when none) so each origin can be told. An owner still running on key
// finds no entry, or a later entry with a different gen, on its next
// DoneOrDrain and stops. Its one caller is Orchestrator.Retire, on a key the
// router retired.
func (q *queue) Cleanup(key string) []Msg {
	q.mu.Lock()
	defer q.mu.Unlock()
	var dropped []Msg
	if sq := q.queues[key]; sq != nil {
		dropped = takeMsgs(sq)
		delete(q.queues, key)
	}
	delete(q.dropNotify, key)
	return dropped
}

// CollectDelay returns the configured collect delay.
func (q *queue) CollectDelay() time.Duration {
	return q.collectDelay
}

// ShouldNotify returns true if the 3s cooldown since the last enqueue
// notification for key has elapsed, so rapid-fire messages don't each get a
// "message received" confirmation. The drop-path cooldown is per key so chat
// A's notify does not silence chat B's.
func (q *queue) ShouldNotify(key string) bool {
	const cooldown = int64(3 * time.Second)
	now := time.Now().UnixNano()
	// delta < 0 means the wall clock stepped backwards; treat as expired so
	// the timestamp re-anchors instead of silencing notifications forever.
	cooling := func(ts int64) bool {
		delta := now - ts
		return delta >= 0 && delta < cooldown
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if sq, ok := q.queues[key]; ok {
		if cooling(sq.lastNotifyNs) {
			return false
		}
		sq.lastNotifyNs = now
		return true
	}
	// No queue entry — per-key cooldown via the bounded dropNotify map.
	ts, ok := q.dropNotify[key]
	if ok && cooling(ts) {
		return false
	}
	if !ok && len(q.dropNotify) >= dropNotifyMaxKeys {
		for k, ts := range q.dropNotify {
			if !cooling(ts) {
				delete(q.dropNotify, k)
			}
		}
		if len(q.dropNotify) >= dropNotifyMaxKeys {
			clear(q.dropNotify)
		}
	}
	q.dropNotify[key] = now
	return true
}
