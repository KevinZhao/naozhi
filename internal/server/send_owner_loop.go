// ownerLoop + handleOwnerLoopPanic：会话拥有者跑完头炮后排空 queue 的完整闭包
// （首炮 + collectTimer 驱动的 drain 循环；panic 恢复 + queue Discard + UI 通知）。
// runTurn 留在 send.go。
package server

import (
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/naozhi/naozhi/internal/dispatch"
)

// ownerLoop processes the first send turn and then drains any messages that
// arrived meanwhile, coalescing them into a single follow-up turn. Mirrors
// dispatch.Dispatcher.ownerLoop with the hub's broadcast + session routing.
//
// gen is the queue generation at enqueue time; if Discard (e.g. /new) bumps
// it mid-flight, DoneOrDrain returns nil and the loop exits. Caller must
// arrange sendWG accounting via TrackSend — ownerLoop never touches sendWG.
func (h *Hub) ownerLoop(key string, gen uint64, first dispatch.QueuedMsg, onAsyncError asyncErrorFn) {
	defer func() {
		if r := recover(); r != nil {
			h.handleOwnerLoopPanic(key, onAsyncError, r)
		}
	}()
	defer h.router.NotifyIdle()

	h.runTurn(key, first.Text, first.Images, onAsyncError)

	// Drain loop: after each turn, wait collectDelay then drain.
	collectTimer := time.NewTimer(h.queue.CollectDelay())
	defer collectTimer.Stop()
	for {
		select {
		case <-h.ctx.Done():
			// Discard resets busy + bumps gen so the next Enqueue can spawn a
			// fresh owner; otherwise the key stays "busy" forever.
			h.queue.Discard(key)
			return
		case <-collectTimer.C:
		}

		queued := h.queue.DoneOrDrain(key, gen)
		if queued == nil {
			return // empty or generation mismatch — stop.
		}

		text, images := dispatch.CoalesceMessages(queued)
		slog.Debug("send: processing queued messages", "key", key, "count", len(queued), "merged_len", len(text))
		// onAsyncError only applies to the first turn (one ack per request);
		// subsequent coalesced turns log failures without a back-channel.
		h.runTurn(key, text, images, nil)
		// Reset 前 Stop + drain（与 dispatch.ownerLoop 对齐）：残留 tick 会让
		// DoneOrDrain 多调一次、刚入队的消息被静默丢弃。
		if !collectTimer.Stop() {
			select {
			case <-collectTimer.C:
			default:
			}
		}
		collectTimer.Reset(h.queue.CollectDelay())
	}
}

// handleOwnerLoopPanic is the deferred panic recovery helper for ownerLoop
// (split out to be unit-testable): logs the stack, Discards the queue so a
// stale owner does not hold the key, and notifies the client via onAsyncError.
// A nested recover absorbs a cascading panic (e.g. a broken WS writer).
func (h *Hub) handleOwnerLoopPanic(key string, onAsyncError asyncErrorFn, r any) {
	slog.Error("ownerLoop panic", "key", key, "panic", r, "stack", string(debug.Stack()))
	if h.queue != nil {
		h.queue.Discard(key)
	}
	if onAsyncError != nil {
		func() {
			defer func() {
				if rr := recover(); rr != nil {
					slog.Error("ownerLoop onAsyncError panic recovered", "key", key, "panic", rr)
				}
			}()
			onAsyncError(nil, "处理异常，请稍后重试。")
		}()
	}
}
