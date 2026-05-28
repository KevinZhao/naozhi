// File-block contract (server-split-phase4-design v0.6.1 §五):
//
//	WRITES:     send block (queue / sendWG / sendTrackMu / sendClosed /
//	            droppedTotal / legacySendInvokes) +
//	            rate-limit/cache block (userSendLimitersMu / userSendLimiters)
//	READS:      shared deps block (read-only after ctor)
//	READS-ALSO: subscriber block (clients) for per-subscription routing
//
// Phase 4a 骨架：方法 placeholder，方法实装留 Phase 4b（Phase 4 三刀
// 里风险最高的一刀，含 send 路径与 broadcast 协调）。
package wshub

// TrackSend registers a background send goroutine with the Hub's sendWG.
//
// CONTRACT (R218B-ARCH-1): every code path that registers a goroutine on
// sendWG MUST go through TrackSend(). 直接 h.sendWG.Add(1) 会绕过
// sendClosed gate，与 Shutdown.Wait 竞速——Shutdown 可能 returned 后
// goroutine 还活着，dereference 已 teardown 的 router/nodes/clients。
//
// Phase 4a placeholder：Phase 4b 起从 server.Hub.TrackSend 完整搬过来。
func (h *Hub) TrackSend() (allowed bool) {
	h.sendTrackMu.Lock()
	defer h.sendTrackMu.Unlock()
	if h.sendClosed {
		return false
	}
	h.sendWG.Add(1)
	return true
}

// DoneSend releases a sendWG slot acquired via TrackSend. Must be paired
// 1:1 with TrackSend(true) returns.
func (h *Hub) DoneSend() {
	h.sendWG.Done()
}
