// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     none (interface declarations + var _ binding only)
//	READS:      none
//
// Phase 4 抽包后这文件搬到 internal/wshub/types.go；其内容是接口定义而
// 非方法——rule 3a/3b 的字段块对账对它不适用，但 marker 仍需声明
// 以满足 Phase 0b 的 marker-existence gate。
package server

import (
	"time"

	"github.com/naozhi/naozhi/internal/dispatch"
)

// MessageEnqueuer is the *dispatch.MessageQueue subset Hub depends on for
// the dashboard-side write path.
//
// satisfies-by: *dispatch.MessageQueue (internal/dispatch/msgqueue.go)
//
// Method set: derived from grep `h\.queue\.[A-Z]` over wshub*.go on
// 2026-05-25; see docs/design/server-split-phase4-design.md §四.4 and
// docs/design/server-consumer-contracts.md (TODO Phase 4) for the
// cross-method ordering contract (Enqueue → DoneOrDrain).
//
// Phase 4 抽包 to internal/wshub/ keeps this interface co-located with
// Hub; dispatch then imports wshub for the var _ binding (no cycle since
// dispatch already depends on cli / session / platform / cron / routing
// but not server). For now the interface lives in server package; the
// var _ binding is therefore inverted into this file too (server imports
// dispatch, the natural direction).
//
// CONTRACT (Phase 0 stub; expand in server-consumer-contracts.md):
//
//   - Enqueue returning isOwner=true: caller becomes owner goroutine and
//     MUST eventually invoke DoneOrDrain(key, gen). Failure to do so
//     leaks the queue slot for that key — subsequent Enqueue blocks /
//     returns "please wait".
//   - DoneOrDrain returns the next batch to process; an empty return
//     means the queue is idle for that key (caller may release ownership).
//   - Discard drops queued messages for key without invoking handlers;
//     used during session reset / shutdown.
//   - CollectDelay is a static config value; cached read OK.
//   - Mode reflects the configured queue strategy (ModeInterrupt / etc.);
//     drives broadcast-debounce decisions in wshub_broadcast.go.
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
