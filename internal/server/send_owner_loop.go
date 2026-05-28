// Phase 3f-prep / R-send-owner-loop-extract (2026-05-28):
// ownerLoop + handleOwnerLoopPanic (~90 行队列消费驱动) 抽到独立文件。
// 纯物理切分、零行为变化。
//
// 这两个 method 构成"会话拥有者跑完头炮后排空 queue"的完整闭包：
//   - ownerLoop          首炮 + collectTimer 驱动的 drain 循环
//   - handleOwnerLoopPanic 拥有者协程的 panic 恢复 + queue Discard + UI 通知
//
// 与 ws/broadcast 路径无 receiver 关系；调用 runTurn (留在 send.go) 通过
// 同包可见性。Phase 4b（wshub.go *Hub method 锁定）禁动的是 wshub.go 内
// 的 ws fan-out / subscribe / cache 等方法，本文件的 ownerLoop 是 send
// 业务路径的拥有者循环，落在 send.go 而非 wshub.go，可独立抽离。
package server

import (
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/naozhi/naozhi/internal/dispatch"
)

// ownerLoop processes the first send turn and then drains any messages that
// arrived while the turn was running, coalescing them into a single follow-up
// turn. Mirrors dispatch.Dispatcher.ownerLoop but integrates with the hub's
// broadcast + session routing.
//
// gen is the queue generation at enqueue time. If Discard (e.g., /new) bumps
// it mid-flight, DoneOrDrain returns nil and this loop exits cleanly.
// Caller must arrange sendWG accounting via TrackSend — ownerLoop does not
// touch sendWG directly so it can be launched with a defer-release closure.
func (h *Hub) ownerLoop(key string, gen uint64, first dispatch.QueuedMsg, onAsyncError func(string)) {
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
			// Discard clears msgs and resets busy=false + bumps gen so a
			// fresh owner can be spawned by the next Enqueue after restart;
			// without this, the key would remain "busy" forever and queued
			// messages would never be processed.
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
		// 与 dispatch.ownerLoop 对齐：Reset 前 Stop + drain，防止未来循环
		// 形状变化（例如 early-continue）让 timer 的残留 tick 立即 fire，
		// 导致 DoneOrDrain 被多调一次、刚入队的消息被丢弃且无任何提示。
		// R192-SRV-P0-CollectTimerDrain。
		if !collectTimer.Stop() {
			select {
			case <-collectTimer.C:
			default:
			}
		}
		collectTimer.Reset(h.queue.CollectDelay())
	}
}

// handleOwnerLoopPanic is the deferred panic recovery helper for ownerLoop.
// Split out of the defer so the recover path can be unit-tested directly —
// constructing a panicking runTurn in tests would require a real router +
// session, which is out of scope for a targeted recover regression. The
// helper:
//
//  1. Logs the panic with a full stack trace for operator triage.
//  2. Clears the message queue so a stale owner is not left holding the key.
//  3. Signals the dashboard client via onAsyncError so the UI can tell the
//     user the turn was lost. HTTP path passes nil onAsyncError (ack already
//     shipped), so this is a no-op there. RETRY3.
//
// A nested recover around onAsyncError absorbs a cascading panic (e.g., a
// broken WS writer) so the outer defer always completes.
func (h *Hub) handleOwnerLoopPanic(key string, onAsyncError func(string), r any) {
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
			onAsyncError("处理异常，请稍后重试。")
		}()
	}
}
