// Package session router cleanup, periodic loops, and shutdown.
//
// This file holds session lifecycle teardown: Remove (per-key delete +
// event-log drop + attachment tracker clear), Cleanup (TTL-based pruning),
// the StartCleanupLoop ticker, periodic saveIfDirty, and graceful Shutdown.
package session

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/osutil"
)

// removeSnapshot captures everything finishRemoveCleanup needs from a session
// AFTER unregisterSessionLocked dropped it from r.ss.sessions, taken under r.mu
// so the post-unlock teardown (possibly a detached RemoveAsync goroutine) never
// reads router state again: finishRemoveCleanup MUST NOT touch r.ss.*.
type removeSnapshot struct {
	proc             processIface
	workspace        string
	retiredSessionID string
}

// unregisterAndSnapshot runs the fast, locked half of a session removal:
// unregister from r.ss.sessions and all secondary indexes, finalise
// active-count and dirty/version bookkeeping, hand back a value snapshot.
// Returns ok=false when the key is absent — lookup+delete is atomic under r.mu,
// so two concurrent Remove/RemoveAsync calls cannot both capture a non-nil proc.
func (r *Router) unregisterAndSnapshot(key string) (removeSnapshot, bool) {
	r.mu.Lock()
	s, ok := r.ss.sessions[key]
	if !ok {
		r.mu.Unlock()
		return removeSnapshot{}, false
	}

	proc := s.loadProcess()
	wasActive := !s.exempt && proc != nil && proc.Alive()
	// Snapshot workspace and session UUID BEFORE unregister: afterwards the
	// session is gone from r.ss.sessions, and OnSessionRemoved needs the root
	// while notifyKeyRetired needs the UUID to stamp retired_at.
	workspaceSnapshot := s.Workspace()
	backend := s.Backend()
	retiredSessionID := s.SessionID()
	r.unregisterSessionLocked(key, s, false)
	if wasActive {
		if r.ss.activeCount.Add(-1) < 0 {
			r.ss.activeCount.Store(0)
		}
		// Per-backend gauge mirror (Multi-Backend RFC §10).
		metrics.RecordSessionActive(backend, -1)
	}
	r.ss.dirty = true
	r.ss.gen.Add(1)
	r.mu.Unlock()

	return removeSnapshot{
		proc:             proc,
		workspace:        workspaceSnapshot,
		retiredSessionID: retiredSessionID,
	}, true
}

// finishRemoveCleanup runs the slow, unlocked half of a session removal: close
// the process, wait for its shim socket to disappear, drop the event log +
// attachment refs, fire lifecycle notifications. Must be called WITHOUT r.mu
// held. Reads only `snap` — never router state — so it is safe in a detached
// goroutine (the session is already gone from every map). Worst case ~15s.
func (r *Router) finishRemoveCleanup(key string, snap removeSnapshot) {
	proc := snap.proc
	if proc != nil && proc.Alive() {
		proc.Close()
		// A dashboard close is often followed by an immediate same-key
		// re-create; wait for the shim socket so the next GetOrCreate does not
		// hit the "refusing to clobber" guard. Deliberately do NOT set
		// shimStuckOnReset[key]: Remove is terminal and unregisterSessionLocked
		// already cleared it; re-inserting leaks an entry per one-shot key (#2261).
		if !waitSocketGoneForKey(key, 2*time.Second) {
			slog.Warn("shim socket still bound after Remove wait — terminal removal, not flagging key (Remove never reuses the key)",
				"key", key)
		}
	}
	// Drop the on-disk event log so a future session reusing the key starts
	// empty. Best-effort: a failed DropKey only leaves stale bytes behind.
	r.dropEventLogForKey(key)
	// Clear the attachment tracker's refs so double-TTL GC reclaims images.
	// Best-effort: stale keyhash entries do not affect correctness.
	r.clearAttachmentTrackerRefs(key, snap.workspace)
	// Free the resident run-history ring (on-disk records stay) so the
	// per-session ring map stays bounded.
	r.sessionRuns.Invalidate(key)
	// Broadcast under r.mu to match every other Broadcast site. Not
	// load-bearing for Shutdown: the session already left r.ss.sessions.
	if r.shutdownCond != nil {
		r.mu.Lock()
		r.shutdownCond.Broadcast()
		r.mu.Unlock()
	}

	logSessionLifecycle("removed", key)
	r.notifyKeyRetired(key, snap.retiredSessionID)
	r.notifyChange()
}

// Remove removes a session from the router and kills its process, blocking
// until the full teardown completes (Shutdown, Cleanup, synchronous tests).
// Dashboard deletes use RemoveAsync to avoid holding the handler ~15s.
func (r *Router) Remove(key string) bool {
	snap, ok := r.unregisterAndSnapshot(key)
	if !ok {
		return false
	}
	r.finishRemoveCleanup(key, snap)
	return true
}

// RemoveAsync removes a session from the router immediately (locked
// unregister) and runs the slow teardown in a detached goroutine. Returns true
// the instant the session is gone from r.ss.sessions.
//
// The teardown goroutine is intentionally NOT tracked by Shutdown (see its
// single-shot + bounded-leak contract); each self-terminates in ≤15s. removeWg
// exists ONLY so tests can wait for the teardown before asserting.
func (r *Router) RemoveAsync(key string) bool {
	snap, ok := r.unregisterAndSnapshot(key)
	if !ok {
		return false
	}
	r.pp.removeWg.Add(1)
	go func() {
		defer r.pp.removeWg.Done()
		// HandleDelete has already returned 200, so a panic in the teardown
		// chain has no caller to recover it. Swallow + count it.
		defer func() {
			if rec := recover(); rec != nil {
				metrics.PanicRecoveredTotal.Add(1)
				slog.Error("async session remove: teardown panicked",
					"key", key, "panic", rec, "stack", string(debug.Stack()))
			}
		}()
		r.finishRemoveCleanup(key, snap)
	}()
	return true
}

// dropEventLogForKey removes a session's persisted event log files (.log +
// .idx). Safe with no persister or never-written keys. The timeout ctx derives
// from r.historyCtx so an in-flight Shutdown cancels DropKey at the next
// syscall boundary instead of blocking Remove for the full 2s; r.historyCtx is
// nil only in tests that bypass NewRouter, which fall back to Background.
func (r *Router) dropEventLogForKey(key string) {
	if r.eventLogPersister == nil {
		return
	}
	parent := r.historyCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	if err := r.eventLogPersister.DropKey(ctx, key); err != nil {
		slog.Warn("event log drop failed", "key", key, "err", err)
	}
}

// clearAttachmentTrackerRefs runs the tracker's OnSessionRemoved sweep so every
// .meta file under `workspace` loses this session's keyhash. Safe with no
// tracker or empty workspace. The short timeout keeps a slow FS from wedging
// Router.Remove (a failure only delays GC by a generation) and is parented on
// r.historyCtx so Shutdown cancels the walk; tests fall back to Background.
func (r *Router) clearAttachmentTrackerRefs(key, workspace string) {
	if r.attachmentTracker == nil || workspace == "" {
		return
	}
	parent := r.historyCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	if err := r.attachmentTracker.OnSessionRemoved(ctx, persist.KeyHash(key), workspace); err != nil {
		slog.Warn("attachment tracker clear failed",
			"key", key, "workspace", workspace, "err", err)
	}
}

// Cleanup closes sessions idle beyond TTL.
// First pass runs under RLock so PID syscalls / process.Alive checks don't
// block message processing (which needs write lock via GetOrCreate).
// Mutations (prune, activeCount recount) still require the write lock.
func (r *Router) Cleanup() {
	type expiredEntry struct {
		s      *ManagedSession
		key    string
		proc   processIface
		reason string // deathReason to stamp; written only after kill re-verify
	}

	now := time.Now()

	// ── Pass 1: snapshot candidate sessions under RLock ────────────
	r.mu.RLock()
	type cand struct {
		key        string
		s          *ManagedSession
		proc       processIface
		lastActive time.Time
		// state is captured once under r.mu RLock so pass-2 avoids re-taking
		// proc.mu.RLock while the hot Send path holds proc.mu.Lock. Staleness
		// is acceptable: Running→Ready just defers stuckKill to the next tick,
		// and death makes Alive() false anyway via the channel-close fast path.
		state cli.ProcessState
	}
	var candidates []cand
	// Collect prune candidates in this same RLock pass so the write-locked
	// prune section is O(expired) instead of ranging the whole map (#1607).
	var pruneCandidates []string
	for key, s := range r.ss.sessions {
		if s.exempt {
			continue // planner sessions are never expired/pruned by TTL
		}
		if r.shouldPrune(s, now) {
			pruneCandidates = append(pruneCandidates, key)
		}
		proc := s.loadProcess()
		if proc == nil {
			continue
		}
		candidates = append(candidates, cand{key, s, proc, s.LastActive(), proc.State()})
	}
	ttl := r.ttl
	totalTimeout := r.totalTimeout
	r.mu.RUnlock()

	if totalTimeout <= 0 {
		totalTimeout = cli.DefaultTotalTimeout
	}
	stuckThreshold := 2 * totalTimeout

	// ── Pass 2: classify outside the lock (may perform PID syscalls) ─
	var expired []expiredEntry
	var stuckKill []expiredEntry
	for _, c := range candidates {
		// Alive() (lock-free select on `done`) is the authoritative liveness
		// gate, so a proc that died between pass-1 and pass-2 is still classified correctly.
		if !c.proc.Alive() {
			continue
		}
		running := c.state == cli.StateRunning

		// Effective activity = max(session.lastActive, process.LastEventAt):
		// lastActive is only refreshed at Send entry, so a single long turn would
		// age past any threshold while the CLI is still streaming events.
		effective := c.lastActive
		if le := c.proc.LastEventAt(); le.After(effective) {
			effective = le
		}

		// Stuck running: watchdog failed, reclaim slot.
		if running {
			if age := now.Sub(effective); age > stuckThreshold {
				slog.Warn("stuck running session detected, force killing",
					"key", c.key, "running_for", age, "threshold", stuckThreshold)
				stuckKill = append(stuckKill, expiredEntry{c.s, c.key, c.proc, "stuck_running"})
			}
			continue
		}

		// PID liveness: shim alive but CLI PID is gone.
		if pid := c.proc.PID(); pid > 0 && !osutil.PidAlive(pid) {
			slog.Warn("CLI process gone but session still alive, force killing",
				"key", c.key, "pid", pid)
			stuckKill = append(stuckKill, expiredEntry{c.s, c.key, c.proc, "pid_gone"})
			continue
		}

		// Normal idle TTL expiry.
		if now.Sub(effective) > ttl {
			logSessionLifecycle("expired", c.key, "idle", now.Sub(effective))
			// Carry the reason and stamp it only after the close-loop re-verify;
			// stamping here (no r.mu) would corrupt the deathReason of a
			// replacement session spawned between this snapshot and the close loop.
			expired = append(expired, expiredEntry{c.s, c.key, c.proc, "idle_timeout"})
		}
	}

	closedCount := 0
	for _, e := range stuckKill {
		// Re-verify the session still holds the proc we classified: pass-2 ran
		// without r.mu, so a concurrent spawnSession / resetLocked may have
		// replaced s.process. Killing the captured proc would target an orphaned
		// shim conn and stamp a bogus deathReason on the fresh session.
		if cur := e.s.loadProcess(); cur != nil && cur != e.proc {
			continue
		}
		// Stamp deathReason only after re-verify confirms this proc is still live.
		if e.reason != "" {
			storeAtomicString(&e.s.deathReason, e.reason)
		}
		e.proc.Kill()
		closedCount++
	}
	// TTL-expired sessions are never re-spawned for the same key by this
	// function, so waitSocketGoneForKey is unnecessary here.
	for _, e := range expired {
		// Same re-verify as stuckKill: skip when the session has moved on so we
		// neither close the replacement's live proc nor stamp idle_timeout on it.
		if cur := e.s.loadProcess(); cur != nil && cur != e.proc {
			continue
		}
		// Stamp deathReason only after re-verify (mirrors stuckKill).
		if e.reason != "" {
			storeAtomicString(&e.s.deathReason, e.reason)
		}
		e.proc.Close()
		closedCount++
	}

	r.mu.Lock()
	// Broadcast under r.mu, after Lock, so Shutdown's cond.Wait predicate
	// (IsRunning check) cannot re-evaluate between Close() and Broadcast.
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}
	// Prune only the candidates snapshotted in pass-1 (#1607), re-checked under
	// the exclusive lock: process/lastActive may have changed (respawn, fresh
	// Send) since the snapshot, and such a candidate must NOT be removed.
	var pruned int
	for _, key := range pruneCandidates {
		s, ok := r.ss.sessions[key]
		if !ok || s.exempt {
			continue // already gone, or became exempt — skip
		}
		if !r.shouldPrune(s, now) {
			continue // state changed since the RLock snapshot; leave it
		}
		// Terminal removal: also frees the backend override.
		r.unregisterSessionLocked(key, s, false)
		pruned++
	}
	// Recompute the per-backend gauge and the alive total in one reconcile
	// walk; skip the O(N) walk when nothing changed.
	var aliveTotal int64
	if closedCount > 0 || pruned > 0 {
		aliveTotal = r.reconcileSessionActiveByBackendLocked()
	} else {
		aliveTotal = r.ss.activeCount.Load()
	}
	r.ss.activeCount.Store(aliveTotal)

	// Snapshot sessions for periodic save (while still holding the lock).
	// Skip save if nothing changed since last Cleanup cycle.
	if closedCount > 0 || pruned > 0 {
		r.ss.dirty = true
		r.ss.gen.Add(1)
	}
	// Snapshot the dirty stores in the smallest shape the save path needs: a
	// []*ManagedSession (#1606) and the ws-overrides map.
	var sessionsCopy []*ManagedSession
	var wsOverridesCopy map[string]string
	storePath := r.storePath
	snapshotGen := r.ss.gen.Load()
	snapshotWsGen := r.wsStore.Gen()
	if r.ss.dirty {
		sessionsCopy = make([]*ManagedSession, 0, len(r.ss.sessions))
		for _, v := range r.ss.sessions {
			sessionsCopy = append(sessionsCopy, v)
		}
	}
	if r.wsStore.Dirty() {
		wsOverridesCopy = r.wsStore.Snapshot()
	}

	r.mu.Unlock()

	// Known IDs live off r.mu. ClaimSave stamps savedAt so a concurrent
	// saveIfDirty tick skips the redundant work (an I/O budget gate, not a
	// file-level race guard: tmp files are unique per WriteFileAtomic call).
	knownIDsCopy, snapshotKnownIDsGen, knownIDsDue, knownIDsMarshalErr := r.kid.ClaimSave(now, knownIDsSaveInterval)

	// Periodic save outside lock to reduce crash-recovery data loss.
	if sessionsCopy != nil {
		if err := saveStoreSlice(storePath, sessionsCopy); err != nil {
			slog.Warn("periodic session save failed", "err", err)
		} else {
			// Only clear dirty flag if no concurrent mutation occurred since snapshot.
			r.mu.Lock()
			if r.ss.gen.Load() == snapshotGen {
				r.ss.dirty = false
			}
			r.mu.Unlock()
		}
	}
	if wsOverridesCopy != nil {
		if err := saveWorkspaceOverrides(storePath, wsOverridesCopy); err != nil {
			slog.Warn("periodic workspace overrides save failed", "err", err)
		} else {
			// Only clear dirty flag if no concurrent SetWorkspace occurred since snapshot.
			r.mu.Lock()
			r.wsStore.MarkSavedIfUnchanged(snapshotWsGen)
			r.mu.Unlock()
		}
	}
	if knownIDsMarshalErr != nil {
		// ClaimSave already released the throttle; dirty stays set.
		slog.Warn("periodic known IDs marshal failed", "err", knownIDsMarshalErr)
	} else if knownIDsDue {
		if err := saveKnownIDsBytes(storePath, knownIDsCopy); err != nil {
			slog.Warn("periodic known IDs save failed", "err", err)
			r.kid.ResetSaveThrottle()
		} else {
			r.kid.MarkSavedIfUnchanged(snapshotKnownIDsGen)
		}
	}

	if len(expired) > 0 || len(stuckKill) > 0 || pruned > 0 {
		r.notifyChange()
	}
}

// shouldPrune returns true if a non-exempt session should be removed from the map.
// Caller must hold r.mu.
//
// Product policy (#2278): a session that owns a Claude SessionID (a resumable
// JSONL on disk) is NEVER auto-pruned — it stays until the user closes it
// (dashboard delete → Remove, or /new → Reset). Only orphans that never held
// a conversation are reaped by TTL: nil-process stubs and dead-but-attached
// processes without a SessionID. The idle TTL still reclaims the CLI *process*.
func (r *Router) shouldPrune(s *ManagedSession, now time.Time) bool {
	if now.Sub(s.LastActive()) <= r.pruneTTL {
		return false
	}
	// A real, resumable conversation: keep it until the user closes it.
	if s.getSessionID() != "" {
		return false
	}
	proc := s.loadProcess()
	if proc == nil {
		return true // nil-process stub that never got a SessionID
	}
	return !proc.Alive() // exited process past pruneTTL, no SessionID
}

// cleanupLoopMaxRestarts caps panic-resurrections of the cleanup loop so a bug
// that panics every tick fails loudly and stops instead of spinning forever.
const cleanupLoopMaxRestarts = 10

// StartCleanupLoop runs Cleanup periodically and saves dirty session state
// on a shorter interval to reduce data loss on crash.
func (r *Router) StartCleanupLoop(ctx context.Context, interval time.Duration) {
	r.startCleanupLoop(ctx, interval, 0)
}

// startCleanupLoop is the panic-restart-aware variant; attempt counts restarts
// so far and the panic handler bails out at cleanupLoopMaxRestarts.
func (r *Router) startCleanupLoop(ctx context.Context, interval time.Duration, attempt int) {
	// time.NewTicker(d) panics for d<=0; the recovery defer would re-schedule
	// and re-panic on the same call, so reject the misconfiguration up front.
	if interval <= 0 {
		slog.Warn("start cleanup loop: non-positive interval, cleanup disabled",
			"interval", interval)
		return
	}
	go func() {
		// A panic inside Cleanup or saveIfDirty would silently kill the loop,
		// letting sessions accumulate past TTL and losing the periodic flush.
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("router cleanup loop panic recovered",
					"panic", rec, "stack", string(debug.Stack()),
					"attempt", attempt, "max_restarts", cleanupLoopMaxRestarts)
				// Do not resurrect after Shutdown. The 5s backoff stops a
				// panic-every-tick bug piling up goroutines; the attempt cap
				// stops the chain spinning — past it, TTL/save coverage degrades
				// and operators restart the process.
				if ctx.Err() != nil {
					return
				}
				if attempt+1 >= cleanupLoopMaxRestarts {
					slog.Error("router cleanup loop exceeded max restarts, giving up",
						"attempts", attempt+1,
						"impact", "TTL pruning and saveIfDirty paused; restart naozhi to recover")
					return
				}
				time.AfterFunc(5*time.Second, func() {
					if ctx.Err() != nil {
						return
					}
					r.startCleanupLoop(ctx, interval, attempt+1)
				})
			}
		}()
		cleanupTicker := time.NewTicker(interval)
		defer cleanupTicker.Stop()
		// Save dirty state on sessionSaveInterval to reduce crash-recovery
		// data loss from ~TTL/2 to one window.
		saveTicker := time.NewTicker(sessionSaveInterval)
		defer saveTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-cleanupTicker.C:
				r.Cleanup()
			case <-saveTicker.C:
				r.saveIfDirty()
			}
		}
	}()
}

// saveIfDirty saves the session store if any mutations have occurred since the last save.
// Also persists knownIDs on the same throttle as Cleanup so crashes between
// Cleanup ticks do not discard newly discovered session IDs.
func (r *Router) saveIfDirty() {
	// The snapshot phase only READS state and dirty flags, so take the RLock
	// and let hot GetOrCreate / Send paths proceed concurrently (#1535).
	r.mu.RLock()
	// Slice snapshot, not a map copy — see the matching note in Cleanup (#1606).
	var sessionsCopy []*ManagedSession
	if r.ss.dirty {
		sessionsCopy = make([]*ManagedSession, 0, len(r.ss.sessions))
		for _, v := range r.ss.sessions {
			sessionsCopy = append(sessionsCopy, v)
		}
	}
	var wsOverridesCopy map[string]string
	if r.wsStore.Dirty() {
		wsOverridesCopy = r.wsStore.Snapshot()
	}
	storePath := r.storePath
	snapshotGen := r.ss.gen.Load()
	snapshotWsGen := r.wsStore.Gen()
	r.mu.RUnlock()

	// Known IDs live off r.mu. ClaimSave checks the throttle and stamps
	// savedAt in one critical section, so two ticks cannot both claim the
	// same save window and double-write the file.
	knownIDsCopy, snapshotKnownIDsGen, knownIDsDue, knownIDsMarshalErr := r.kid.ClaimSave(time.Now(), knownIDsSaveInterval)

	if sessionsCopy != nil {
		if err := saveStoreSlice(storePath, sessionsCopy); err != nil {
			slog.Warn("periodic session save failed", "err", err)
		} else {
			r.mu.Lock()
			if r.ss.gen.Load() == snapshotGen {
				r.ss.dirty = false
			}
			r.mu.Unlock()
		}
	}
	if wsOverridesCopy != nil {
		if err := saveWorkspaceOverrides(storePath, wsOverridesCopy); err != nil {
			slog.Warn("periodic workspace overrides save failed", "err", err)
		} else {
			// Only clear dirty flag if no concurrent SetWorkspace occurred since snapshot.
			r.mu.Lock()
			r.wsStore.MarkSavedIfUnchanged(snapshotWsGen)
			r.mu.Unlock()
		}
	}
	if knownIDsMarshalErr != nil {
		// ClaimSave already released the throttle; dirty stays set.
		slog.Warn("periodic known IDs marshal failed", "err", knownIDsMarshalErr)
	} else if knownIDsDue {
		if err := saveKnownIDsBytes(storePath, knownIDsCopy); err != nil {
			slog.Warn("periodic known IDs save failed", "err", err)
			r.kid.ResetSaveThrottle()
		} else {
			r.kid.MarkSavedIfUnchanged(snapshotKnownIDsGen)
		}
	}
}

// Shutdown gracefully closes all sessions, waiting for running ones to complete.
// Idempotent: subsequent calls return immediately after the first completes.
//
// CONTRACT: Shutdown assumes the naozhi process terminates shortly after it
// returns. Two watcher goroutines (the `r.historyWg.Wait()` wrapper below and
// the shim reconcile ticker in Scheduler.Stop) may outlive Shutdown when
// blocked on hung I/O, relying on OS teardown; a reusable Router would leak one
// per cycle. TestShutdown_SingleShotContract enforces that `shutdownOnce` stays
// in place so any attempt to make Shutdown reversible trips CI.
func (r *Router) Shutdown() {
	r.shutdownOnce.Do(r.shutdown)
}

func (r *Router) shutdown() {
	// Cancel the history ctx so in-flight LoadHistory*Ctx calls abort instead
	// of blocking on slow reads; the bounded Wait below is the hard deadline.
	// historyWgMu makes the cancel atomic vs the "check Err() then Add(1)" pair
	// in runHistoryTask / loadResumeHistoryOnSpawn, so Wait() never races a late Add (#2186).
	if r.historyCancel != nil {
		r.historyWgMu.Lock()
		r.historyCancel()
		r.historyWgMu.Unlock()
	}

	// Wait for history-loading goroutines, but not forever if FS I/O is hung
	// (e.g. NFS); a leaked goroutine on timeout is bounded by the single-shot
	// contract above. Do NOT replace historyWg.Wait() with a ctx-aware
	// pattern: WaitGroup has none; the select IS the bounded wait.
	historyDone := make(chan struct{})
	go func() {
		// Goroutine intentionally left running on timeout; cleaned up on process exit.
		r.historyWg.Wait()
		close(historyDone)
	}()
	historyTimer := time.NewTimer(5 * time.Second)
	select {
	case <-historyDone:
		historyTimer.Stop()
	case <-historyTimer.C:
		slog.Warn("shutdown: history loading timed out after 5s, proceeding")
	}
	// Deadline timer: broadcast to unblock Wait() on timeout. Must hold r.mu
	// across Broadcast so the cond.Wait predicate evaluation below cannot race
	// with the timer firing and silently lose the wakeup (same contract as NotifyIdle).
	timer := time.AfterFunc(ShutdownTimeout, func() {
		if r.shutdownCond != nil {
			r.mu.Lock()
			r.shutdownCond.Broadcast()
			r.mu.Unlock()
		}
	})
	defer timer.Stop()

	r.mu.Lock()

	// Wait for running sessions to complete (up to ShutdownTimeout)
	deadline := time.Now().Add(ShutdownTimeout)
	// Log once per Shutdown when shutdownCond is nil (test shape).
	shutdownCondMissingLogged := false
	for {
		running := false
		for _, s := range r.ss.sessions {
			if p := s.loadProcess(); p != nil && p.IsRunning() {
				running = true
				break
			}
		}
		if !running || time.Now().After(deadline) {
			break
		}
		if r.shutdownCond != nil {
			r.shutdownCond.Wait() // atomically releases and re-acquires r.mu
		} else {
			// Fallback for tests without shutdownCond (production always goes
			// through NewRouter); warn once so a bare `&Router{}` test is not silent.
			if !shutdownCondMissingLogged {
				slog.Warn("shutdown: Router constructed without shutdownCond — falling back to 100ms busy-poll; tests should use NewRouter")
				shutdownCondMissingLogged = true
			}
			r.mu.Unlock()
			time.Sleep(100 * time.Millisecond)
			r.mu.Lock()
		}
	}

	// Publish the stopped gate while still holding r.mu, immediately before the
	// snapshot (#1822): spawnSession checks r.stopped under the same r.mu, so a
	// concurrent spawn either completed (and is in the snapshot) or gets
	// ErrRouterStopped. Do NOT move this out of the held-lock region.
	r.stopped.Store(true)

	// Snapshot sessions for saving outside lock, as a value slice:
	// saveStoreSlice only iterates values, so no map copy is needed.
	sessionsCopy := make([]*ManagedSession, 0, len(r.ss.sessions))
	for _, v := range r.ss.sessions {
		sessionsCopy = append(sessionsCopy, v)
	}
	storePath := r.storePath
	wsOverrides := r.wsStore.Snapshot()

	// Collect processes to close, then release lock to close concurrently
	var procs []processIface
	for key, s := range r.ss.sessions {
		if p := s.loadProcess(); p != nil && p.Alive() {
			slog.Info("shutting down session", "key", key)
			procs = append(procs, p)
		}
	}
	r.mu.Unlock()

	// Known IDs live off r.mu; the final save is unthrottled.
	knownIDsCopy, knownIDsMarshalErr := r.kid.MarshalSnapshot()

	// Save session state outside lock (avoids JSON marshal + file I/O under mutex).
	// disk_full lets monitoring page on ENOSPC separately; each error chain is
	// walked once since the three saves are independent.
	if err := saveStoreSlice(storePath, sessionsCopy); err != nil {
		slog.Error("save session store on shutdown", "err", err, "disk_full", osutil.IsDiskFull(err))
	}
	if knownIDsMarshalErr != nil {
		slog.Error("marshal known session IDs on shutdown", "err", knownIDsMarshalErr)
	} else if err := saveKnownIDsBytes(storePath, knownIDsCopy); err != nil {
		slog.Error("save known session IDs on shutdown", "err", err, "disk_full", osutil.IsDiskFull(err))
	}
	if err := saveWorkspaceOverrides(storePath, wsOverrides); err != nil {
		slog.Error("save workspace overrides on shutdown", "err", err, "disk_full", osutil.IsDiskFull(err))
	}

	// Detach shim processes (keep them alive for reconnect after restart)
	// instead of Close (which would kill the CLI).
	var wg sync.WaitGroup
	for _, proc := range procs {
		wg.Add(1)
		go func(p processIface) {
			defer wg.Done()
			// Shutdown is last in the graceful-stop sequence, so a panic inside
			// Detach/Close would bring down the process and skip remaining
			// cleanup in main. Swallow so naozhi still exits cleanly.
			defer func() {
				if r := recover(); r != nil {
					metrics.PanicRecoveredTotal.Add(1)
					slog.Error("session shutdown: detach panicked",
						"panic", r, "stack", string(debug.Stack()))
				}
			}()
			if dp, ok := p.(interface{ Detach() }); ok {
				dp.Detach()
			} else {
				p.Close()
			}
		}(proc)
	}
	wg.Wait()

	// Flush & stop the event-log persister last so batches still in the
	// in-channel reach disk. The ctx parent is context.Background, NOT
	// r.historyCtx: that was cancelled at the top of shutdown, so a child would
	// see ctx.Err() immediately and the persister would skip flushing.
	if r.eventLogPersister != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.eventLogPersister.Stop(ctx); err != nil {
			slog.Warn("event log persister stop timed out",
				"err", err, "stats", r.eventLogPersister.Stats())
		}
	}

	// Stop the attachment tracker AFTER the persister so no OnPersistedEntry
	// bumps arrive during its drain (a bump after Stop would silently drop).
	r.stopAttachmentTracker()

	// Flush the session-run-history write worker so records from the final
	// turns reach disk. Close blocks on the bounded queue draining; nil store is a no-op.
	r.sessionRuns.Close()
}
