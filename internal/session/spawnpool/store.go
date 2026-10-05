// Package spawnpool holds the spawn-concurrency bookkeeping owned by
// session.Router (the `pp` facet): the pending-spawn counter that feeds the
// maxProcs capacity check, the per-key in-flight spawn done-channels that
// coalesce concurrent GetOrCreate callers and stop ReconnectShims from
// treating a half-spawned shim as an orphan, the per-key shim-stuck flags
// that classify the next spawn failure as ErrShimStuck, and the per-key runs
// of failed Init handshakes the startup breaker reads. Fields are private so
// the compiler enforces access through the method surface (#2495).
//
// Lock contract: Store carries NO lock. The router keeps it in its session
// table's extension state, so every method runs inside a table transaction:
// the pending count is added to the live-session count inside the capacity
// check, in-flight keys are checked against the live session index by
// reconnect and by GetOrCreate's wait loop, the stuck flag is consumed in
// the critical section that enters spawnSession, and a failed spawn's record
// is written before its done-channel closes, so a woken waiter sees it.
package spawnpool

import (
	"time"

	"github.com/naozhi/naozhi/internal/cli/clierr"
)

// Store is the spawn-concurrency container; the zero value is ready to use.
type Store struct {
	// pending counts spawnSession calls between slot acquire and release; the
	// capacity check adds it to the live count because r.mu is dropped
	// around the actual Spawn.
	pending int
	// spawning maps a key with a spawnSession in flight to the done-channel
	// its defer closes; waiters select on the channel, reconnect reads the key.
	spawning map[string]chan struct{}
	// shimStuck marks keys whose last Reset / ResetAndRecreate saw the shim
	// socket outlive its wait; consumed by the next spawn for that key.
	shimStuck map[string]bool
	// failures maps a key whose last spawns failed their Init handshake to
	// that run; a spawn that gets past Init and every reset clear it.
	failures map[string]StartupFailure
}

// StartupFailure is a key's run of spawns whose CLI failed the Init
// handshake: Streak in a row, the last at At, Class and Detail naming its
// cause.
type StartupFailure struct {
	Streak int32
	Class  clierr.ExitClass
	At     time.Time
	Detail string
}

// PendingSpawns returns the number of spawns holding a slot.
func (s *Store) PendingSpawns() int { return s.pending }

// AcquireSpawnSlot counts one spawn in progress. Balance is the caller's
// RAII token's job; there is no floor.
func (s *Store) AcquireSpawnSlot() { s.pending++ }

// ReleaseSpawnSlot undoes one AcquireSpawnSlot.
func (s *Store) ReleaseSpawnSlot() { s.pending-- }

// BeginSpawn marks key as spawning and returns its done-channel. owned is
// false when a spawn is already in flight for key: ch is then that spawn's
// channel, to wait on, and the caller must not end it. Only the owner — the
// caller that got owned=true — ends a spawn, which is what makes the close
// happen exactly once.
func (s *Store) BeginSpawn(key string) (ch chan struct{}, owned bool) {
	if s.spawning == nil {
		s.spawning = make(map[string]chan struct{})
	}
	if ch, ok := s.spawning[key]; ok {
		return ch, false
	}
	ch = make(chan struct{})
	s.spawning[key] = ch
	return ch, true
}

// EndSpawn closes ch, waking every waiter parked on it, and removes key —
// if ch is key's current marker. Otherwise it does nothing and returns
// false: a second end, or an end by a caller that never owned the spawn,
// must not close a channel twice or retire someone else's marker.
func (s *Store) EndSpawn(key string, ch chan struct{}) bool {
	if cur, ok := s.spawning[key]; !ok || cur != ch {
		return false
	}
	close(ch)
	delete(s.spawning, key)
	return true
}

// SpawnInFlight returns the done-channel of the spawn in flight for key.
func (s *Store) SpawnInFlight(key string) (chan struct{}, bool) {
	ch, ok := s.spawning[key]
	return ch, ok
}

// SpawningCount returns the number of keys with a spawn in flight.
func (s *Store) SpawningCount() int { return len(s.spawning) }

// MarkShimStuck flags key so its next spawn failure is wrapped as ErrShimStuck.
func (s *Store) MarkShimStuck(key string) {
	if s.shimStuck == nil {
		s.shimStuck = make(map[string]bool)
	}
	s.shimStuck[key] = true
}

// ConsumeShimStuck reports whether key is flagged and clears the flag, so a
// retry after the consuming spawn gets a clean classification.
func (s *Store) ConsumeShimStuck(key string) bool {
	if !s.shimStuck[key] {
		return false
	}
	delete(s.shimStuck, key)
	return true
}

// ShimStuck reports whether key is flagged without consuming the flag.
func (s *Store) ShimStuck(key string) bool { return s.shimStuck[key] }

// ClearShimStuck drops the flag for key; terminal removals call it so a
// never-respawned key cannot pin an entry for the process lifetime.
func (s *Store) ClearShimStuck(key string) { delete(s.shimStuck, key) }

// NoteStartupFailure stores f as key's run of failed Init handshakes.
func (s *Store) NoteStartupFailure(key string, f StartupFailure) {
	if s.failures == nil {
		s.failures = make(map[string]StartupFailure)
	}
	s.failures[key] = f
}

// StartupFailure returns key's run of failed Init handshakes.
func (s *Store) StartupFailure(key string) (StartupFailure, bool) {
	f, ok := s.failures[key]
	return f, ok
}

// ClearStartupFailure drops key's run.
func (s *Store) ClearStartupFailure(key string) { delete(s.failures, key) }

// PruneStartupFailures drops the runs whose last failure is before cutoff,
// so a key that is never retried cannot pin an entry, and returns their keys.
func (s *Store) PruneStartupFailures(cutoff time.Time) (pruned []string) {
	for key, f := range s.failures {
		if f.At.Before(cutoff) {
			delete(s.failures, key)
			pruned = append(pruned, key)
		}
	}
	return pruned
}
