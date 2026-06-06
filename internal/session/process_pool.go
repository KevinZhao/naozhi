// Package session — the process-pool / shim-reconciler facet (Router P5, #805).
package session

import "sync"

// processPool groups the four correlated spawn-concurrency fields (Router P5
// facet, #805): the in-flight Spawn() counter (pendingSpawns), the in-flight
// spawn-key → done-channel set (spawningKeys), the reset-stuck-shim flag set
// (shimStuckOnReset), and the RemoveAsync teardown WaitGroup (removeWg). It is
// a value field on Router, carries NO lock of its own, and is read/written
// ONLY under Router.mu — the lock topology is unchanged (RFC §3 candidate A:
// single r.mu retained, NO atomic promotion, NO new lock).
//
// CRITICAL — removeWg is a sync.WaitGroup and therefore NON-COPYABLE. This
// facet is embedded on Router as a value, which is sound ONLY because Router
// is always heap-allocated via `&Router{}` and never copied by value (go vet
// copylocks enforces this at build time). Do NOT add any method/func that
// takes a Router by value or returns processPool by value.
//
// done-channel pairing (R243-ARCH-4 / R248-ARCH-10): spawningKeys[key]=doneCh
// install ↔ markSpawnDoneLocked close+delete ↔ GetOrCreate select-on-ch ↔
// ReconnectShims presence read all reference this one spawningKeys map under
// r.mu. pendingSpawns RAII balance (acquire ++ / releaseLocked+release -- /
// panicSafeSpawn recover) all reference this one pendingSpawns. The annotation
// below covers the UNION of all accessing domains; the lint recurses one level
// so each inner field ALSO carries its own per-domain annotation.
type processPool struct {
	// pendingSpawns tracks Spawn() calls in progress (lock released during spawn)
	// 读写: lifecycle (spawnSession), core (acquire/release RAII helpers)
	pendingSpawns int

	// spawningKeys records keys whose spawnSession is in flight. ReconnectShims
	// consults this set before declaring a discovered shim "orphan": a shim may
	// have written its state file after we dropped r.mu for wrapper.Spawn() but
	// before the new ManagedSession is installed, and without this set a
	// concurrent reconcile would shut the fresh shim down as an orphan.
	//
	// The map value is a per-spawn done-channel that spawnSession close()s
	// from its defer. GetOrCreate's wait loop selects on this channel
	// instead of polling, so the second caller wakes the instant the
	// winner finishes (success or failure) rather than after the next
	// 20ms tick. ReconnectShims still reads only the key set, so its
	// presence check is unaffected by the value type. R243-ARCH-4.
	// 读写: core (init), lifecycle (spawnSession write/close), shim (reconnect read)
	spawningKeys map[string]chan struct{}

	// shimStuckOnReset records keys whose most recent Reset /
	// ResetAndRecreate observed waitSocketGoneForKey timing out (the shim
	// socket was still bound after the 2s grace). The next GetOrCreate
	// for the same key consults this flag and, on spawn failure, wraps
	// the returned error with ErrShimStuck so the cron / dashboard caller
	// can surface a distinct actionable error class to the operator
	// instead of the generic ErrClassSessionError. The flag is consumed
	// (deleted) on the very next GetOrCreate for the key — success or
	// failure — so a subsequent retry gets a clean classification.
	// 读写: lifecycle (Reset / ResetAndRecreate write; GetOrCreate read+delete), cleanup (finishRemoveCleanup write)
	// (#1324 — R20260527122801-CR-12)
	shimStuckOnReset map[string]bool

	// removeWg tracks in-flight RemoveAsync teardown goroutines. It exists
	// ONLY for test observability (tests call removeWg.Wait() directly) —
	// production teardown never waits on it, and in particular Shutdown
	// deliberately does NOT join it (the detached teardown follows the
	// single-shot + bounded-leak contract documented on Shutdown). Each
	// tracked goroutine self-terminates in ≤15s.
	// 读写: cleanup (RemoveAsync Add/Done), test helpers (Wait)
	removeWg sync.WaitGroup
}
