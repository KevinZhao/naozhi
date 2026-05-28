// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     subscriber block (clients / connCount / subscriberCount /
//	            clientWG / wsAuthLimiter / wsUpgradeLimiter / upgrader /
//	            dashTokenHash / cookieMAC / trustedProxy)
//	READS:      shared deps block (read-only after ctor)
//	READS-ALSO: send block (sendClosed only — close client must drain
//	            pending sends; lifecycle-coordinated)
//
// Phase 4a 骨架：方法 placeholder，方法实装留 Phase 4b。
package wshub

// Register adds a wsClient to the Hub's subscriber registry.
//
// Phase 4a placeholder：Phase 4b 起从 server.Hub.Register 完整搬过来。
func (h *Hub) Register(c *wsClient) error {
	// Phase 4b 实装：
	//   h.mu.Lock()
	//   h.clients[c] = struct{}{}
	//   h.connCount.Add(1)
	//   h.mu.Unlock()
	//   h.clientWG.Add(1)
	_ = c
	return nil
}

// Unregister removes a wsClient from the Hub's subscriber registry.
//
// Phase 4a placeholder：Phase 4b 起从 server.Hub.Unregister 完整搬过来。
func (h *Hub) Unregister(c *wsClient) {
	_ = c
}
