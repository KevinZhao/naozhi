// Package routerrelay breaks the construction cycles around session.Router.
//
// The router is built first, in cmd/naozhi, but the things its notifications
// reach are built later and need the router to exist: the dashboard's WS hub
// and session handler (server). A Relay is handed to the router at
// construction as its observer, and each consumer binds its slot once it
// exists. Binding is once per slot — a second bind panics — so the router's
// wiring cannot be swapped out from under it later.
//
// Until a slot is bound its events are dropped, which is what the router did
// before those consumers existed.
package routerrelay

import "sync/atomic"

// Relay is the router's observer. The zero value is ready to use, with every
// slot unbound.
type Relay struct {
	sessionsChanged atomic.Pointer[func()]
	keyRetired      atomic.Pointer[func(key, sessionID string)]
}

// SessionsChanged forwards the router's session-list change notification.
func (r *Relay) SessionsChanged() {
	if fn := r.sessionsChanged.Load(); fn != nil {
		(*fn)()
	}
}

// KeyRetired forwards the router's key-retirement notification.
func (r *Relay) KeyRetired(key, sessionID string) {
	if fn := r.keyRetired.Load(); fn != nil {
		(*fn)(key, sessionID)
	}
}

// BindSessionsChanged installs the session-list change consumer.
func (r *Relay) BindSessionsChanged(fn func()) { bindOnce(&r.sessionsChanged, fn, "SessionsChanged") }

// BindKeyRetired installs the key-retirement consumer.
func (r *Relay) BindKeyRetired(fn func(key, sessionID string)) {
	bindOnce(&r.keyRetired, fn, "KeyRetired")
}

func bindOnce[F any](slot *atomic.Pointer[F], fn F, name string) {
	if !slot.CompareAndSwap(nil, &fn) {
		panic("routerrelay: " + name + " bound twice")
	}
}
