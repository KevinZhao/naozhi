package session

// router_adopt.go — the session half of cron run adoption (#2712 PR B).
//
// A cron run that was mid-turn when naozhi restarted has, after reconnect, a
// live *cli.Process whose adopted-turn latch (cli/adopted_turn.go) holds — or
// will hold — how that turn ended. Nobody in this process issued the Send, so
// cron's startup reconciler is the caller that wants the answer. It cannot
// import session or cli; wireup bridges the capability (cron.InFlightAdopter).

import (
	"sync"

	"github.com/naozhi/naozhi/internal/cli"
)

// AdoptState classifies what the router knows about a key a cron reconciler is
// trying to adopt.
type AdoptState int

const (
	// AdoptNone: no live adoptable turn — the shim died with the old process,
	// or the session was never reconnected mid-turn. The caller records the
	// interrupted run exactly as before adoption existed.
	AdoptNone AdoptState = iota
	// AdoptLive: the session holds a process whose adopted-turn latch is armed;
	// AwaitAdopted on it answers how the in-flight turn ended.
	AdoptLive
	// AdoptDriftShutdown: startup shut this key's surviving shim down because
	// its argv no longer matched config (#2749). The run did not fail and was
	// not interrupted by the restart itself — the operator's config edit ended
	// it, and the caller's record should say so.
	AdoptDriftShutdown
)

// driftShutdownKeys records the keys whose shims reconnectShims shut down for
// argv drift, so a cron reconciler asking moments later can attribute the
// death correctly. Startup-only state: written during the reconnect pass,
// read during cron's reconcile, never cleaned — a handful of keys per process
// lifetime, and "was ever drift-shut" stays true for late askers.
type driftShutdowns struct {
	mu   sync.Mutex
	keys map[string]struct{}
}

func (d *driftShutdowns) mark(key string) {
	d.mu.Lock()
	if d.keys == nil {
		d.keys = make(map[string]struct{}, 2)
	}
	d.keys[key] = struct{}{}
	d.mu.Unlock()
}

func (d *driftShutdowns) has(key string) bool {
	d.mu.Lock()
	_, ok := d.keys[key]
	d.mu.Unlock()
	return ok
}

// AdoptInFlight reports whether key holds an adoptable in-flight turn, and the
// live process when it does. The verdict order matters: a live latch wins over
// a recorded drift shutdown, because a key could in principle be drift-shut
// and then respawned mid-turn — the live turn is the newer fact.
func (r *Router) AdoptInFlight(key string) (*cli.Process, AdoptState) {
	r.mu.RLock()
	sess := r.ss.sessions[key]
	r.mu.RUnlock()
	if sess != nil {
		// loadProcess returns the processIface tests stub; the adopted-turn
		// latch lives on the concrete *cli.Process only, so a stubbed process
		// simply reports nothing to adopt.
		if p, ok := sess.loadProcess().(*cli.Process); ok && p != nil && p.AdoptedTurnPending() {
			return p, AdoptLive
		}
	}
	if r.drift.has(key) {
		return nil, AdoptDriftShutdown
	}
	return nil, AdoptNone
}
