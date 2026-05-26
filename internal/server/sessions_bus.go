package server

// SessionsBus is the publish abstraction over Hub.BroadcastSessionsUpdate
// (R246-ARCH-3 / #777). Producers (project rescan, scratch lifecycle,
// node register/deregister, send paths, …) publish a "sessions changed"
// signal here without holding a *Hub reference; the Hub remains the one
// subscriber that materialises the signal into a debounced WebSocket
// frame.
//
// Why an interface, not a func value:
//
//   - Future subscribers — auditing, in-process Prometheus exporters,
//     log streaming — can register without each producer learning a new
//     callback shape.
//   - Tests that don't construct a real Hub can hand a no-op
//     SessionsBus to ProjectHandlers / SessionHandlers without nil-
//     guarding every Publish call.
//   - The Hub-internal debounce + closure-recycling lives untouched in
//     wshub_broadcast.go (#723 owner: F3); this file is purely a
//     publish surface.
//
// Lifecycle: a SessionsBus is created once in buildServer alongside the
// Hub and never replaced. Producers grab a reference at construction
// time (so a nil-Hub test build still gets a valid Publish target) and
// the binding to the Hub is filled in by registerDashboard once the
// Hub exists. The hubSessionsBus adapter holds a *Hub-returning closure
// to avoid the lifecycle ordering hazard the inline `if s.hub != nil`
// pattern repeated at 6+ call sites in server.go used to manage.
//
// Migration plan (out of scope for #777, tracked here so future PRs
// don't reintroduce direct calls):
//
//  1. New code uses SessionsBus.Publish.
//  2. Existing inline `s.hub.BroadcastSessionsUpdate()` call sites are
//     migrated when the surrounding code is touched for other reasons.
//  3. Once direct callers drop to zero (verified via `git grep
//     "\.BroadcastSessionsUpdate()"`), the Hub method is renamed to
//     publishSessionsUpdate (lowercase) and the abstraction owns the
//     public surface.
type SessionsBus interface {
	// Publish coalesces a "sessions changed" notification. The
	// underlying transport may debounce; producers MUST treat publish
	// as fire-and-forget and never expect ordering or per-event
	// delivery.
	Publish()
}

// hubSessionsBus is the production SessionsBus implementation backed by
// a *Hub. The hub field is resolved lazily through getHub so producers
// constructed in buildServer (before the Hub exists) can hold a stable
// SessionsBus reference; once registerDashboard installs the hub the
// closure starts returning a non-nil pointer and Publish becomes a
// debounced broadcast. Publish is a no-op while getHub returns nil,
// matching the legacy `if s.hub != nil` guard repeated at every
// pre-hub call site.
type hubSessionsBus struct {
	getHub func() *Hub
}

// Publish forwards to the Hub's debounced BroadcastSessionsUpdate when
// the Hub is wired; otherwise no-op. The lifecycle window where Hub is
// nil is the buildServer → registerDashboard interval — producers may
// fire signals during that window (e.g. router callbacks installed
// during construction) and the Hub will rebuild fresh state on its
// own first broadcast pass, so dropping pre-Hub publishes is safe.
func (b *hubSessionsBus) Publish() {
	if b == nil || b.getHub == nil {
		return
	}
	if h := b.getHub(); h != nil {
		h.BroadcastSessionsUpdate()
	}
}

// newHubSessionsBus returns a SessionsBus that forwards to the Hub
// resolved by getHub on every Publish. Used in buildServer so the
// SessionsBus reference handed to handlers is stable across the
// pre-/post-registerDashboard lifecycle window.
func newHubSessionsBus(getHub func() *Hub) SessionsBus {
	return &hubSessionsBus{getHub: getHub}
}

// noopSessionsBus is the SessionsBus used by tests that construct a
// partial server without a Hub. Publish is a noop; the test asserts on
// other state.
type noopSessionsBus struct{}

// Publish on a noopSessionsBus is a noop. Returns no error and never
// blocks; safe to call from any goroutine.
func (noopSessionsBus) Publish() {}

// NewNoopSessionsBus returns a SessionsBus whose Publish is a noop. For
// tests that construct partial server state without wiring a Hub.
func NewNoopSessionsBus() SessionsBus { return noopSessionsBus{} }
