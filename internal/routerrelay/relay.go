// Package routerrelay breaks the construction cycles around session.Router.
//
// The router is built first, in cmd/naozhi, but the things its notifications
// reach are built later and need the router to exist: the dashboard's WS hub
// and session handler (server), and the cron scheduler that owns cost runs
// (wireup, which takes the router's ledger). A Relay is handed to the router at
// construction as its observer and cost-run owner, and each consumer binds its
// half once it exists. Binding is once per slot — a second bind panics — so
// the router's wiring cannot be swapped out from under it later.
//
// Until a slot is bound its events are dropped and OwnsCostRun answers false,
// which is what the router did before those consumers existed.
package routerrelay

import "sync/atomic"

// Relay is the router's observer and cost-run owner. The zero value is ready
// to use, with every slot unbound.
type Relay struct {
	sessionsChanged atomic.Pointer[func()]
	keyRetired      atomic.Pointer[func(key, sessionID string)]
	costRunOwner    atomic.Pointer[func(key string) bool]
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

// OwnsCostRun reports whether a turn on key belongs to a run that writes its
// own ledger entry; false until an owner is bound.
func (r *Relay) OwnsCostRun(key string) bool {
	if fn := r.costRunOwner.Load(); fn != nil {
		return (*fn)(key)
	}
	return false
}

// BindSessionsChanged installs the session-list change consumer.
func (r *Relay) BindSessionsChanged(fn func()) { bindOnce(&r.sessionsChanged, fn, "SessionsChanged") }

// BindKeyRetired installs the key-retirement consumer.
func (r *Relay) BindKeyRetired(fn func(key, sessionID string)) {
	bindOnce(&r.keyRetired, fn, "KeyRetired")
}

// BindCostRunOwner installs the cost-run ownership gate.
func (r *Relay) BindCostRunOwner(fn func(key string) bool) {
	bindOnce(&r.costRunOwner, fn, "CostRunOwner")
}

func bindOnce[F any](slot *atomic.Pointer[F], fn F, name string) {
	if !slot.CompareAndSwap(nil, &fn) {
		panic("routerrelay: " + name + " bound twice")
	}
}
