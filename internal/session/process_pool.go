// Package session — the process-pool / shim-reconciler facet (#805).
package session

import "sync"

// processPool groups the spawn-concurrency fields. It is a value field on
// Router, carries NO lock of its own, and is read/written ONLY under Router.mu.
//
// CRITICAL — removeWg is a sync.WaitGroup and therefore NON-COPYABLE. Embedding
// by value is sound ONLY because Router is always heap-allocated via `&Router{}`
// and never copied (go vet copylocks enforces this). Do NOT add any method/func
// that takes a Router by value or returns processPool by value.
//
// The lint (tools/check-router-fields) recurses one level, so each inner field
// carries its own per-domain access annotation.
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
	// from its defer; GetOrCreate's wait loop selects on it instead of polling.
	// ReconnectShims reads only the key set.
	// 读写: core (init), lifecycle (spawnSession write/close), shim (reconnect read)
	spawningKeys map[string]chan struct{}

	// shimStuckOnReset records keys whose most recent Reset / ResetAndRecreate
	// saw waitSocketGoneForKey time out. The next GetOrCreate for the key
	// consults it and, on spawn failure, wraps the error with ErrShimStuck; the
	// flag is consumed (deleted) on that GetOrCreate — success or failure — so a
	// subsequent retry gets a clean classification.
	// 读写: lifecycle (Reset / ResetAndRecreate write; GetOrCreate read+delete), cleanup (finishRemoveCleanup write)
	// (#1324)
	shimStuckOnReset map[string]bool

	// removeWg tracks in-flight RemoveAsync teardown goroutines, ONLY for test
	// observability (tests call removeWg.Wait()). Production never waits on it;
	// Shutdown deliberately does NOT join it (single-shot + bounded-leak
	// contract documented on Shutdown). Each goroutine self-terminates in ≤15s.
	// 读写: cleanup (RemoveAsync Add/Done), test helpers (Wait)
	removeWg sync.WaitGroup
}
