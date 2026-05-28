// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     rate-limit/cache block (historyMarshalCache for replay cache)
//	READS:      shared deps block (read-only after ctor) + subscriber block
//	            (clients for fanout) + lifecycle block (ctx for cancel)
//
// Phase 4a 骨架：方法 placeholder，方法实装留 Phase 4c（agent_tailer +
// eventpush + hub_agent 收尾）。
package wshub

// startEventPushLoop is the placeholder for the per-session event push
// loop that fans EventLog updates to all subscribed wsClients. Phase 4c
// 起从 server.Hub 的 eventPushLoop 完整搬过来。
func (h *Hub) startEventPushLoop(sessionKey string) {
	_ = sessionKey
}
