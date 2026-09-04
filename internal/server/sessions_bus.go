package server

// SessionsBus is the publish abstraction over Hub.BroadcastSessionsUpdate
// (#777). Producers publish a "sessions changed" signal without holding a
// *Hub; the Hub is the one subscriber that debounces it into a WebSocket
// frame (wshub_broadcast.go). Created once in buildServer and never
// replaced; the Hub binding is filled in by registerDashboard, so producers
// may hold a reference before the Hub exists. New code should call Publish
// rather than s.hub.BroadcastSessionsUpdate() directly.
type SessionsBus interface {
	// Publish coalesces a "sessions changed" notification. Fire-and-forget:
	// the transport may debounce, so producers must never expect ordering
	// or per-event delivery.
	Publish()
}

// hubSessionsBus is the production SessionsBus backed by a *Hub. The hub is
// resolved lazily through getHub so producers built before the Hub exists
// hold a stable reference; Publish is a no-op while getHub returns nil.
type hubSessionsBus struct {
	getHub func() *Hub
}

// Publish forwards to Hub.BroadcastSessionsUpdate when the Hub is wired;
// otherwise no-op. Dropping pre-Hub publishes is safe: the Hub rebuilds
// fresh state on its own first broadcast pass.
func (b *hubSessionsBus) Publish() {
	if b == nil || b.getHub == nil {
		return
	}
	if h := b.getHub(); h != nil {
		h.BroadcastSessionsUpdate()
	}
}

// newHubSessionsBus returns a SessionsBus that forwards to the Hub
// resolved by getHub on every Publish.
func newHubSessionsBus(getHub func() *Hub) SessionsBus {
	return &hubSessionsBus{getHub: getHub}
}

// noopSessionsBus is the SessionsBus for tests that construct a partial
// server without a Hub.
type noopSessionsBus struct{}

// Publish on a noopSessionsBus is a noop.
func (noopSessionsBus) Publish() {}

// NewNoopSessionsBus returns a SessionsBus whose Publish is a noop.
func NewNoopSessionsBus() SessionsBus { return noopSessionsBus{} }
