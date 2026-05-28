// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     broadcast block (debounceMu / debounceTimer / debounceFirst /
//	            debounceClosed / debounceClosedFast / debounceFire) +
//	            subscriber block (clients) for SendRaw fanout
//	READS:      shared deps block (read-only after ctor) + send block
//	            (queue / droppedTotal for broadcast-aware enqueue)
//
// Phase 4a 骨架：方法 placeholder，方法实装留 Phase 4b。
package wshub

// BroadcastSessionsUpdate signals that the sessions list changed and
// fanouts a "sessions:update" event to all connected clients.
//
// Phase 4a placeholder：Phase 4b 起从 server.Hub.BroadcastSessionsUpdate
// 完整搬过来（含 debounce 节流逻辑）。
func (h *Hub) BroadcastSessionsUpdate() {
	if h.debounceClosedFast.Load() {
		return
	}
	// Phase 4b 实装真正的 debounce 调度。
}
