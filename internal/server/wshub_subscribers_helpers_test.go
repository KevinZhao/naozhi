package server

import "fmt"

// registerSub registers c as an authenticated client and, when key is set,
// subscribes it to key with a no-op unsubscribe.
func registerSub(h *Hub, c *wsClient, key string) {
	c.authenticated.Store(true)
	h.subs.add(c)
	if key != "" {
		subscribeTest(h, c, key, func() {})
	}
}

// subscribeTest subscribes the registered client c to key with unsub, the way
// a completed handleSubscribe does, and returns the new generation.
func subscribeTest(h *Hub, c *wsClient, key string, unsub func()) uint64 {
	if res := h.subs.reserve(c, key); res != reserveOK {
		panic(fmt.Sprintf("subscribeTest: reserve(%q) = %v", key, res))
	}
	gen, ok := h.subs.install(c, key, unsub, func() bool { return true })
	if !ok {
		panic(fmt.Sprintf("subscribeTest: install(%q) declined", key))
	}
	return gen
}

// isSubscribed reports whether c holds a subscription (or a reservation) for key.
func isSubscribed(h *Hub, c *wsClient, key string) bool {
	h.subs.mu.RLock()
	defer h.subs.mu.RUnlock()
	cs, ok := h.subs.clients[c]
	if !ok {
		return false
	}
	_, ok = cs.unsubs[key]
	return ok
}

// subscriptionCount is the number of keys c is subscribed to.
func subscriptionCount(h *Hub, c *wsClient) int {
	h.subs.mu.RLock()
	defer h.subs.mu.RUnlock()
	if cs, ok := h.subs.clients[c]; ok {
		return len(cs.unsubs)
	}
	return 0
}

// subscriberCountOf is key's subscriber count from the registry's own set,
// not the lock-free mirror.
func subscriberCountOf(h *Hub, key string) int {
	h.subs.mu.RLock()
	defer h.subs.mu.RUnlock()
	return len(h.subs.byKey[key])
}

// subscribedKeyCount is the number of keys with at least one subscriber.
func subscribedKeyCount(h *Hub) int {
	h.subs.mu.RLock()
	defer h.subs.mu.RUnlock()
	return len(h.subs.byKey)
}

// isRegistered reports whether c is a registered client.
func isRegistered(h *Hub, c *wsClient) bool {
	h.subs.mu.RLock()
	defer h.subs.mu.RUnlock()
	_, ok := h.subs.clients[c]
	return ok
}

// registeredClients is the number of registered clients.
func registeredClients(h *Hub) int {
	h.subs.mu.RLock()
	defer h.subs.mu.RUnlock()
	return len(h.subs.clients)
}

// authCount is the size of the authenticated set.
func authCount(h *Hub) int {
	h.subs.authMu.RLock()
	defer h.subs.authMu.RUnlock()
	return len(h.subs.authSlice)
}
