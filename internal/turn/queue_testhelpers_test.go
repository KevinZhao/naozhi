package turn

import "time"

// NewQueue is a test-only convenience constructor that mints a Queue in
// ModeCollect — the production default. Production code must construct via
// NewQueueWithMode so the queue mode is explicit at every call site
// (Collect / Interrupt / Passthrough each have very different latency /
// cost profiles, and a wrapper that hides the mode makes mode-related
// regressions invisible at review time).
//
// Lives in a *_test.go file so the linker excludes it from the production
// binary (#1205). Tests in other packages cannot see it and use the
// explicit turn.NewQueueWithMode form.
func NewQueue(maxDepth int, collectDelay time.Duration) *Queue {
	return NewQueueWithMode(maxDepth, collectDelay, ModeCollect)
}

// depth returns the number of queued messages for key (excluding the
// owner's). Test-only and unexported, so it is not part of Queue's
// production surface (TestQueueSurface_Ratchet).
func (q *Queue) depth(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if sq := q.queues[key]; sq != nil {
		return sq.ring.len()
	}
	return 0
}
