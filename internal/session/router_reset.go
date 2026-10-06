package session

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"

	"github.com/naozhi/naozhi/internal/metrics"
)

// The reset family. Every variant removes the key(s) from the table in one
// transaction and then, outside it, releases them (releaseKeys: close the
// process, wait for the shim socket, flag shim-stuck). They differ in scope,
// in which picks survive, and in whether the key is retired:
//
//   - Reset / ResetAndDiscardOverride: one key; every pick is dropped; the
//     key is retired before the release (notifyKeyRetired → the queue's
//     Retire, the history cache, retired_at). Retire tells the queued
//     origins DropRemoved, which is why callers that reset on the user's
//     request discard the queue first (DropReset: dispatch /new, server
//     /clear).
//   - ResetChatAndSetWorkspace: every agent key of a chat; only the backend
//     pick is dropped (pendingPicks.dropBackend); the keys are NOT retired —
//     /cd does not discard their queues, and queued messages are meant to run
//     in the new workspace. Like /new, it lifts the chat's startup-failure
//     pauses, on keys with no entry too.
//   - ResetAndRecreate: one key, re-spawned in the same transaction; picks
//     are kept for that spawn; not retired, since a Retire would drop the
//     messages queued for the recreated session.

// ResetChatAndSetWorkspace atomically resets all sessions belonging to a chat
// (all agents) and installs a new workspace override for it in one
// transaction. Two separate transactions would let a concurrent GetOrCreate
// see the key idle with the override deleted and spawn in the OLD workspace
// (#2342).
func (r *Router) ResetChatAndSetWorkspace(chatKeyPrefix, path string) {
	var released []releasedKey
	r.ss.Update(func(tx sessTx) {
		var closedActive int
		// A copy of the chat's keys: resetChatEntry deletes as it goes.
		for _, key := range tx.KeysOfChat(chatKeyPrefix) {
			released = r.resetChatEntry(tx, key, released, &closedActive)
		}
		tx.Ext().spawns.ClearStartupFailuresOfChat(chatKeyPrefix, chatKeyFor)
		if closedActive > 0 {
			if tx.AddActive(-int64(closedActive)) < 0 {
				tx.SetActive(0)
			}
			// Reconcile the per-backend labeled gauge by batched recount;
			// O(n) but only on the rare chat-prefix reset.
			r.reconcileActiveByBackend(tx.View)
		}
		// Delete marks the store dirty so the removal survives a crash before
		// any other path flips the flag.
		tx.Ext().workspaces.Delete(chatKeyPrefix)
		if chatKeyPrefix != "" {
			// Same transaction as the reset so no concurrent GetOrCreate sees
			// the chat reset with the override gone (#2342). The override was
			// just deleted, so this is always a fresh insert.
			r.putWorkspaceOverride(tx, chatKeyPrefix, path)
		}
		tx.MarkChanged()
	})
	r.releaseKeys(released)
	r.notifyChange()
}

// resetChatEntry removes one session of a chat reset: drops the session's
// record, its session-ID mapping and its backend pick, appends it to released,
// and bumps closedActive when the session counted toward maxProcs.
func (r *Router) resetChatEntry(tx sessTx, key string, released []releasedKey, closedActive *int) []releasedKey {
	s := tx.Get(key)
	if s == nil {
		return released
	}
	p := s.loadProcess()
	if p != nil && p.Alive() && !s.exempt {
		*closedActive++
	}
	if id := s.getSessionID(); id != "" {
		tx.ClearID(id)
	}
	tx.Delete(key)
	// Backend pick only: the chat returns to the default backend, while the
	// two consumed-on-spawn picks still apply to this key. dropBackend's doc
	// records that the omission is deliberate.
	tx.Ext().picks.dropBackend(key)
	return append(released, releasedKey{key: key, proc: p})
}

// releasedKey is a key a reset removed from the table, with the process it
// had (nil when none).
type releasedKey struct {
	key  string
	proc processIface
}

// releaseKeys finishes resets outside any transaction. It closes each live
// process and waits for its key's shim socket to go, so a same-key StartShim
// does not hit the dial-first "refusing to clobber" guard. A key whose proc is
// nil or dead may still have its shim bound (CLI crash, stale pointer): that
// shim is offered a dead-CLI retire, and the wait runs unless it took effect.
// A socket still bound after the bounded wait flags its key shim-stuck, and
// the next GetOrCreate wraps its spawn error with ErrShimStuck (#1324). Keys
// are released concurrently, so a chat reset waits one window rather than one
// per key. The Broadcast at the end wakes a Shutdown waiting on a process.
func (r *Router) releaseKeys(keys []releasedKey) {
	if len(keys) == 0 {
		return
	}
	stuck := make([]bool, len(keys))
	release := func(i int) {
		k := keys[i]
		if k.proc != nil && k.proc.Alive() {
			k.proc.Close()
		} else if r.backends.retireDeadShim(k.key) {
			return
		}
		stuck[i] = !waitSocketGoneForKey(k.key)
	}
	if len(keys) == 1 {
		release(0)
	} else {
		var wg sync.WaitGroup
		for i := range keys {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if rec := recover(); rec != nil {
						metrics.PanicRecoveredTotal.Add(1)
						slog.Error("reset: releasing a key panicked",
							"key", keys[i].key, "panic", rec, "stack", string(debug.Stack()))
					}
				}()
				release(i)
			}()
		}
		wg.Wait()
	}
	r.ss.Update(func(tx sessTx) {
		for i, k := range keys {
			if stuck[i] {
				tx.Ext().spawns.MarkShimStuck(k.key)
			}
		}
		tx.Broadcast()
	})
	for i, k := range keys {
		if stuck[i] {
			slog.Warn("shim socket still bound after reset wait — flagging key for ErrShimStuck wrap on next GetOrCreate",
				"key", k.key)
		}
		logSessionLifecycle("reset", k.key)
	}
}

// resetEntry performs the in-lock teardown shared by Reset and
// ResetAndDiscardOverride. Caller must run the finishResetUnlocked
// sequence after releasing the lock.
//
// Returns the live process (for Close after lock release), the session
// UUID captured before teardown (for the retired-session notification —
// the key's table entry is unregistered here, so callers cannot recover the
// UUID once the transaction ends), and the success flag.
func (r *Router) resetEntry(tx sessTx, key string) (processIface, string, bool) {
	// /new lifts a startup-failure pause at once, on a key with no entry too.
	tx.Ext().spawns.ClearStartupFailure(key)
	s, ok := tx.Lookup(key)
	if !ok {
		return nil, "", false
	}
	proc := s.loadProcess()
	wasActive := !s.exempt && proc != nil && proc.Alive()
	backend := s.Backend()
	sessionID := s.SessionID()
	r.unregisterSession(tx, key, s, false)
	if wasActive {
		if tx.AddActive(-1) < 0 {
			tx.SetActive(0)
		}
		metrics.RecordSessionActive(backend, -1)
	}
	tx.MarkChanged()
	return proc, sessionID, true
}

// Reset discards the session for the given key (user sent /new).
func (r *Router) Reset(key string) {
	var proc processIface
	var sessionID string
	var ok bool
	r.ss.Update(func(tx sessTx) { proc, sessionID, ok = r.resetEntry(tx, key) })
	if !ok {
		return
	}
	r.finishResetUnlocked(key, sessionID, proc)
}

// ResetAndDiscardOverride atomically resets the session AND deletes its
// chat's workspace override, so a concurrent SetWorkspace cannot survive a
// bare Reset+delete pair and leak into the next session. Overrides are keyed
// by the chat key, not the session key.
func (r *Router) ResetAndDiscardOverride(key string) {
	var proc processIface
	var sessionID string
	var hadSession bool
	r.ss.Update(func(tx sessTx) {
		proc, sessionID, hadSession = r.resetEntry(tx, key)
		tx.Ext().workspaces.Delete(chatKeyFor(key))
	})
	if !hadSession {
		return
	}
	r.finishResetUnlocked(key, sessionID, proc)
}

// finishResetUnlocked runs the post-unlock teardown shared by Reset and
// ResetAndDiscardOverride. Must be called without the table lock held. sessionID
// is the UUID captured by resetEntry before unregister removed the key's
// table entry; pass through as-is to notifyKeyRetired so the
// dashboard history-sort hook can stamp retired_at. The key is retired before
// releaseKeys, as Remove does, so a session admitted during it keeps its queue.
func (r *Router) finishResetUnlocked(key, sessionID string, proc processIface) {
	r.notifyKeyRetired(key, sessionID)
	r.releaseKeys([]releasedKey{{key: key, proc: proc}})
	r.notifyChange()
}

// ResetAndRecreate atomically resets a session and spawns a new one for the
// same key, so no concurrent message can create a session with other opts. A
// guard channel is installed with BeginSpawn before the transaction releases
// the lock for proc.Close(); reserveSpawn reuses it, so the in-flight marker
// is continuous from that first release until completeSpawn ends it (#775).
//
// One spawn per key at a time: a spawn already in flight for key — another
// ResetAndRecreate mid-close, a GetOrCreate, a takeover — is waited out first,
// then the reset runs against whatever it installed. Joining it instead would
// run two spawns for one key and end its guard twice.
func (r *Router) ResetAndRecreate(ctx context.Context, key string, opts AgentOpts) (*ManagedSession, error) {
	for {
		s, wait, err := r.resetAndRecreateOnce(ctx, key, opts)
		if wait == nil {
			return s, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait:
		}
	}
}

// resetAndRecreateOnce is one ResetAndRecreate attempt; a non-nil wait is the
// in-flight spawn it found and did not touch.
func (r *Router) resetAndRecreateOnce(ctx context.Context, key string, opts AgentOpts) (*ManagedSession, chan struct{}, error) {
	var (
		hadOld, stuck, stuckWarn bool
		res                      spawnReservation
		err                      error
		wait                     chan struct{}
	)
	r.ss.Update(func(tx sessTx) {
		if ch, inflight := tx.Ext().spawns.SpawnInFlight(key); inflight {
			wait = ch
			return
		}
		tx.Ext().spawns.ClearStartupFailure(key)
		// Delete old session if present
		if s, ok := tx.Lookup(key); ok {
			hadOld = true
			proc := s.loadProcess()
			wasActive := !s.exempt && proc != nil && proc.Alive()
			oldBackend := s.Backend()
			// keepBackendOverride=true: the new opts may carry its own
			// backend, and the spawn below consumes and clears the override.
			r.unregisterSession(tx, key, s, true)
			if wasActive {
				if tx.AddActive(-1) < 0 {
					tx.SetActive(0)
				}
				// The spawn below Incs the gauge for the (possibly
				// different) new backend.
				metrics.RecordSessionActive(oldBackend, -1)
			}
			tx.MarkChanged()

			if proc != nil && proc.Alive() {
				// Install the guard before releasing the lock so a concurrent
				// GetOrCreate parks instead of spawning with different opts;
				// the spawn reuses it and ends it (#775). Owned: nothing was in
				// flight at the top of this transaction.
				res.guard, _ = tx.Ext().spawns.BeginSpawn(key)
				var gone bool
				tx.Unlocked(func() {
					proc.Close()
					// As in Reset: the shim socket must be gone before the
					// spawn's StartShim dials it, or the re-bind fails with
					// "refusing to clobber".
					gone = waitSocketGoneForKey(key)
				})
				if !gone {
					// Flag for the ErrShimStuck wrap on the spawn failure path
					// below (#1324); consumed inline here, not by GetOrCreate.
					tx.Ext().spawns.MarkShimStuck(key)
					stuckWarn = true
				}
				// Broadcast inside the transaction (see evictOldest).
				tx.Broadcast()
			}
		}
		stuck = tx.Ext().spawns.ConsumeShimStuck(key)
		err = r.reserveSpawn(tx, &res, key, "", opts)
	})
	if wait != nil {
		return nil, wait, nil
	}
	if stuckWarn {
		slog.Warn("shim socket still bound after ResetAndRecreate wait — flagging key for ErrShimStuck wrap on spawn failure",
			"key", key)
	}
	var s *ManagedSession
	if err == nil {
		s, err = r.completeSpawn(ctx, &res)
	}
	if err != nil {
		if hadOld {
			r.notifyChange()
		}
		if stuck {
			return nil, nil, fmt.Errorf("%w: %w", ErrShimStuck, err)
		}
		return nil, nil, err
	}
	// completeSpawn can return a concurrently-spawned session with err==nil;
	// the stuck flag must not be silently swallowed (#1702).
	warnShimStuckReuse(stuck, key)
	// completeSpawn already called notifyChange on success
	return s, nil, nil
}

// warnShimStuckReuse logs the shim-stuck diagnostic when ResetAndRecreate set
// shim-stuck but the spawn returned a usable session without error
// (TOCTOU guard reused a concurrently-spawned session), so the signal is not
// lost on the success path (#1702).
func warnShimStuckReuse(stuck bool, key string) {
	if !stuck {
		return
	}
	slog.Warn("shim socket was still bound after ResetAndRecreate wait, but spawnSession reused an existing session (TOCTOU race); ErrShimStuck not wrapped, surfacing stuck diagnostic via log",
		"key", key)
}
