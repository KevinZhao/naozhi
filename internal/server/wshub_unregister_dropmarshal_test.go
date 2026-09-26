package server

import (
	"testing"
)

// TestUnregister_DropsMarshalCacheOnLastSubscriber pins R20260610-085718-LB-5
// (#2010): the abrupt-disconnect path (Hub.unregister, driven by readPump's
// defer) must drop the historyMarshalCache slot for any key whose subscriber
// count hits zero — exactly like the explicit handleUnsubscribe path. Before
// the fix, unregister only decremented the counter, pinning the cache slot to
// Shutdown for every multi-tab session that bypassed the singleSubscriber
// fast path.
func TestUnregister_DropsMarshalCacheOnLastSubscriber(t *testing.T) {
	hub, _ := newTestHub("")
	t.Cleanup(hub.Shutdown)

	const lastKey = "feishu:p2p:last"
	const sharedKey = "feishu:p2p:shared"

	// Seed cache slots for both keys (as getOrMarshal would on first push).
	hub.historyMarshalCache.slot(lastKey)
	hub.historyMarshalCache.slot(sharedKey)

	// The disconnecting client subscribes to both keys; a second tab still
	// subscribes to sharedKey, so only lastKey's slot may go.
	c := &wsClient{done: make(chan struct{})}
	registerSub(hub, c, lastKey)
	subscribeTest(hub, c, sharedKey, func() {})
	registerSub(hub, &wsClient{done: make(chan struct{})}, sharedKey)

	hub.unregister(c)

	if _, ok := hub.historyMarshalCache.entries.Load(lastKey); ok {
		t.Errorf("cache slot for %q should be dropped after last subscriber disconnects", lastKey)
	}
	if _, ok := hub.historyMarshalCache.entries.Load(sharedKey); !ok {
		t.Errorf("cache slot for %q must survive while another tab still subscribes", sharedKey)
	}
}
