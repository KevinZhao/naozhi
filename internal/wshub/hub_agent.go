// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     agent tailer block (tailers / wiredLinkersMu / wiredLinkers)
//	READS:      shared deps block (router for session resolution)
//
// Phase 4a 骨架：方法 placeholder，方法实装留 Phase 4c。
package wshub

// SubscribeAgent attaches a wsClient to the per-task agent tailer for
// streaming agent jsonl events.
//
// Phase 4a placeholder：Phase 4c 起从 server.Hub.handleAgentSubscribe
// 完整搬过来。
func (h *Hub) SubscribeAgent(c *wsClient, taskID string) error {
	_ = c
	_ = taskID
	return nil
}

// UnsubscribeAgent detaches a wsClient from the agent tailer.
func (h *Hub) UnsubscribeAgent(c *wsClient, taskID string) {
	_ = c
	_ = taskID
}
