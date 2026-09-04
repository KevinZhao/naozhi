// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     none (interface declarations + var _ binding only)
//	READS:      none
package server

import (
	"time"

	"github.com/naozhi/naozhi/internal/dispatch"
)

// MessageEnqueuer is the *dispatch.MessageQueue subset Hub depends on for
// the dashboard-side write path.
//
// Contract:
//   - Enqueue returning isOwner=true: the caller becomes owner goroutine and
//     MUST eventually invoke DoneOrDrain(key, gen), or the queue slot for that
//     key leaks (subsequent Enqueue blocks / returns "please wait").
//   - DoneOrDrain returns the next batch; empty means the key is idle.
//   - Discard drops queued messages for key without invoking handlers.
//   - CollectDelay is static config; Mode drives broadcast-debounce decisions.
type MessageEnqueuer interface {
	Enqueue(key string, msg dispatch.QueuedMsg) (isOwner, enqueued, shouldInterrupt bool, gen uint64, evictedID string)
	DoneOrDrain(key string, gen uint64) []dispatch.QueuedMsg
	Discard(key string)
	Mode() dispatch.QueueMode
	CollectDelay() time.Duration
}

// Compile-time guarantee: *dispatch.MessageQueue satisfies MessageEnqueuer.
// This is the editing barrier — adding a method to MessageEnqueuer that
// MessageQueue does not implement breaks the build immediately.
var _ MessageEnqueuer = (*dispatch.MessageQueue)(nil)
