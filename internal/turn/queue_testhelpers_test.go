package turn

import "time"

// newTestQueue mints a queue in ModeCollect, the production default.
func newTestQueue(maxDepth int, collectDelay time.Duration) *queue {
	return newQueue(QueueOptions{MaxDepth: maxDepth, CollectDelay: collectDelay, Mode: ModeCollect})
}

// enqueueTuple is Enqueue with its result spread out, for tests that
// destructure it.
func (q *queue) enqueueTuple(key string, msg Msg) (isOwner, enqueued, shouldInterrupt bool, gen uint64) {
	r := q.Enqueue(key, msg)
	return r.isOwner, r.enqueued, r.shouldInterrupt, r.gen
}

// depth returns the number of queued messages for key (excluding the
// owner's).
func (q *queue) depth(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if sq := q.queues[key]; sq != nil {
		return sq.ring.len()
	}
	return 0
}
