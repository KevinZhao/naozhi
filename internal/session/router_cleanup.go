// Package session router cleanup, periodic loops, and shutdown.
//
// Extracted from router.go on 2026-05-19 as part of the router-split
// refactor (docs/design/router-split-design.md). For history prior to
// commit 77b2c7a4d85bcf03fa6258e5dacb77140a8bc9fb, see:
//
//	git log --follow internal/session/router.go
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

// Remove removes a session from the router and kills its process.
// Used by the dashboard to hide sessions from the list.
func (r *Router) Remove(key string) bool {
	r.mu.Lock()
	s, ok := r.sessions[key]
	if !ok {
		r.mu.Unlock()
		return false
	}

	// Kill process if alive
	proc := s.loadProcess()
	wasActive := !s.exempt && proc != nil && proc.Alive()
	// Snapshot the workspace BEFORE unregister so the attachment
	// tracker's OnSessionRemoved walk has the right root. After
	// unregisterSessionLocked the session is gone from r.sessions
	// and the workspace lookup would fail.
	workspaceSnapshot := s.Workspace()
	backend := s.Backend()
	// Snapshot the session UUID before unregister for the same reason
	// — by the time notifyKeyRetired runs r.sessions[key] is gone, and
	// the history-drawer subscriber needs the UUID to stamp retired_at
	// on the corresponding RecentSession.
	retiredSessionID := s.SessionID()
	r.unregisterSessionLocked(key, s, false)
	if wasActive {
		if r.activeCount.Add(-1) < 0 {
			r.activeCount.Store(0)
		}
		// Multi-Backend RFC §10 (Sprint 6a): per-backend gauge mirror.
		metrics.RecordSessionActive(backend, -1)
	}
	r.storeDirty = true
	r.storeGen.Add(1)
	r.mu.Unlock()

	if proc != nil && proc.Alive() {
		// Remove is a pure delete, not a re-spawn, so we intentionally do
		// not call waitSocketGoneForKey. If a caller ever chains Remove
		// → GetOrCreate for the same key (e.g., a "restart session" UI
		// button), add the wait there — see Reset/ResetAndRecreate for
		// the UCCLEP-2026-04-26 pattern.
		proc.Close()
	}
	// Drop the on-disk event log so a future session reusing the same
	// key starts with an empty history. Best-effort: a DropKey failure
	// leaves the file behind; the next spawnSession's Recover pass
	// will tolerate stale bytes but operators will see larger disk
	// usage than expected.
	r.dropEventLogForKey(key)
	// Clear the attachment tracker's refs for this session so the
	// double-TTL GC will reclaim images once LastReferencedAt
	// elapses. Best-effort — a failure leaves stale keyhash entries
	// behind which do not affect correctness (GC still collects on
	// uploadTTL expiry).
	r.clearAttachmentTrackerRefs(key, workspaceSnapshot)
	// R191-CONC-H1-c: Broadcast under r.mu (see evictOldest comment).
	if r.shutdownCond != nil {
		r.mu.Lock()
		r.shutdownCond.Broadcast()
		r.mu.Unlock()
	}

	logSessionLifecycle("removed", key)
	r.notifyKeyRetired(key, retiredSessionID)
	r.notifyChange()
	return true
}

// dropEventLogForKey removes a session's persisted event log files
// (.log + .idx). Safe to call with no persister configured or for
// keys that were never written to — the Persister's DropKey path
// tolerates missing files.
//
// R229-GO-6: derive the timeout ctx from r.historyCtx so a Shutdown
// in flight cancels DropKey at the next syscall boundary instead of
// letting Remove block for the full 2s budget per call. r.historyCtx
// is cancelled as the very first step of shutdown(); DropKey will see
// ctx.Err() == context.Canceled and bail early. r.historyCtx is nil
// only in tests that bypass NewRouter; fall back to Background there
// to preserve the previous behaviour.
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

// clearAttachmentTrackerRefs runs the tracker's OnSessionRemoved
// sweep so every .meta file under `workspace` loses this session's
// keyhash. Safe to call with no tracker configured or an empty
// workspace snapshot.
//
// We use a short ctx timeout so a permission-denied subtree or
// slow FS cannot wedge Router.Remove. A failure only delays
// attachment GC by a generation; correctness is unaffected.
//
// R229-GO-6: parent the timeout on r.historyCtx so Shutdown propagates
// cancellation to OnSessionRemoved (FS walks can otherwise tie up the
// shutdown deadline for the full 5s budget per Remove). Tests without
// NewRouter wiring keep the previous Background behaviour via fallback.
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
		s    *ManagedSession
		key  string
		proc processIface
	}

	// ── Pass 1: snapshot candidate sessions under RLock ────────────
	// Single-pass build with a conservative capacity hint (half the map —
	// planner/exempt and suspended sessions are typically the majority on
	// idle deployments, so over-allocating to len(r.sessions) wastes cap on
	// every 5-minute tick). A prior two-loop version pre-counted candidates
	// to size the slice exactly, but loadProcess() is an atomic read whose
	// result can change between the two passes, and the doubled map walk
	// paid O(2n) for no correctness benefit. R59-GO-M1.
	//
	// Also closes R57-ARCH-001 (the historical "double pass loadProcess() in
	// non-exempt majority deployments is slower" concern): the single-pass
	// candidate build above means there is exactly one loadProcess() call
	// per non-exempt session per tick, regardless of exempt density. The
	// idle-plan deployment shape (5-minute tick, mostly-suspended sessions)
	// shows zero observed cost difference vs the older two-pass form.
	r.mu.RLock()
	type cand struct {
		key        string
		s          *ManagedSession
		proc       processIface
		lastActive time.Time
		// state is captured once under r.mu RLock (R220-PERF-4) so pass-2
		// avoids re-taking proc.mu.RLock for IsRunning/Alive while message
		// processing on the hot Send path holds proc.mu.Lock. The state
		// can be stale by pass-2 (proc may transition Running→Ready or
		// die before classification), but staleness is acceptable: a
		// transition to Ready makes us skip stuckKill (we'll catch it
		// next tick on idle TTL), and a transition to Dead makes Alive()
		// already-false anyway via the channel-close fast path used
		// below.
		state cli.ProcessState
	}
	candidates := make([]cand, 0, len(r.sessions)/2+1)
	for key, s := range r.sessions {
		if s.exempt {
			continue // planner sessions are never expired by TTL
		}
		proc := s.loadProcess()
		if proc == nil {
			continue
		}
		candidates = append(candidates, cand{key, s, proc, s.LastActive(), proc.GetState()})
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
	now := time.Now()
	for _, c := range candidates {
		// R220-PERF-4: derive alive/running from the state captured in
		// pass-1. StateDead is set lazily by Process.markExited via the
		// done channel; Alive() still consults that channel as a
		// fast-path so a proc that died strictly between pass-1 and
		// pass-2 is still classified correctly. We keep the Alive()
		// check (lock-free select on `done`) as the authoritative
		// liveness gate.
		if !c.proc.Alive() {
			continue
		}
		running := c.state == cli.StateRunning

		// Effective activity = max(session.lastActive, process.LastEventAt).
		// lastActive is only refreshed at Send entry, so a single long-
		// running turn (e.g. 20 min code analysis) would age past any
		// threshold even while the CLI is actively streaming tool_use /
		// thinking events. Folding in LastEventAt turns "a live event
		// landed recently" into a first-class progress signal and kills
		// the stuck-running false positive that used to vaporise running
		// sessions from the dashboard.
		effective := c.lastActive
		if le := c.proc.LastEventAt(); le.After(effective) {
			effective = le
		}

		// Stuck running: watchdog failed, reclaim slot.
		if running {
			if age := now.Sub(effective); age > stuckThreshold {
				slog.Warn("stuck running session detected, force killing",
					"key", c.key, "running_for", age, "threshold", stuckThreshold)
				storeAtomicString(&c.s.deathReason, "stuck_running")
				stuckKill = append(stuckKill, expiredEntry{c.s, c.key, c.proc})
			}
			continue
		}

		// PID liveness: shim alive but CLI PID is gone.
		if pid := c.proc.PID(); pid > 0 && !osutil.PidAlive(pid) {
			slog.Warn("CLI process gone but session still alive, force killing",
				"key", c.key, "pid", pid)
			storeAtomicString(&c.s.deathReason, "pid_gone")
			stuckKill = append(stuckKill, expiredEntry{c.s, c.key, c.proc})
			continue
		}

		// Normal idle TTL expiry.
		if now.Sub(effective) > ttl {
			logSessionLifecycle("expired", c.key, "idle", now.Sub(effective))
			storeAtomicString(&c.s.deathReason, "idle_timeout")
			expired = append(expired, expiredEntry{c.s, c.key, c.proc})
		}
	}

	closedCount := 0
	for _, e := range stuckKill {
		// R217-CR-3: re-verify the session still holds the proc we
		// classified as stuck. Pass-2 ran without r.mu (PID syscalls), so
		// a concurrent spawnSession / resetLocked could have replaced
		// s.process between the snapshot and now. Killing the originally-
		// captured proc would still target a now-orphaned shim conn — a
		// no-op in the steady state but it pollutes the deathReason
		// (already stamped to "stuck_running" / "pid_gone" above) on the
		// fresh ManagedSession the user is actively using. Skip when the
		// session has moved on; the new proc gets its own chance next
		// tick. shouldPrune already handles the orphan-process side; this
		// just stops the false-positive kill bookkeeping.
		if cur := e.s.loadProcess(); cur != nil && cur != e.proc {
			continue
		}
		e.proc.Kill()
		closedCount++
	}
	// TTL-expired sessions are closed but never re-spawned for the same
	// key by this function, so waitSocketGoneForKey is unnecessary here.
	// The next unrelated GetOrCreate will hash to a different socket.
	for _, e := range expired {
		e.proc.Close()
		closedCount++
	}

	r.mu.Lock()
	// R191-CONC-H1-d: Broadcast under r.mu (see evictOldest comment). Moved
	// from before Lock to after Lock so Shutdown's cond.Wait predicate
	// (IsRunning check) cannot re-evaluate between Close() and Broadcast.
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}
	// Prune orphaned sessions: nil process, no session ID, past prune TTL.
	// Maintain a running newActive counter so we avoid a separate countActive() O(n) pass.
	var pruned int
	var newActive int64
	for key, s := range r.sessions {
		if s.exempt {
			continue // planner sessions are never pruned
		}
		if r.shouldPrune(s, now) {
			// Terminal removal: free the backend override too (previous versions
			// leaked it; see MED-5 in 2026-04-26 architecture review).
			r.unregisterSessionLocked(key, s, false)
			pruned++
			continue
		}
		if s.isAlive() {
			newActive++
		}
	}
	prevActive := r.activeCount.Swap(newActive)

	// Snapshot sessions for periodic save (while still holding the lock).
	// Skip save if nothing changed since last Cleanup cycle.
	if closedCount > 0 || pruned > 0 {
		r.storeDirty = true
		r.storeGen.Add(1)
	}
	// Multi-Backend RFC §10 (Sprint 6a): same reconciliation rationale
	// as countActive — bulk path, recompute the labeled gauge in one
	// pass instead of plumbing per-key Dec calls through the prune loop.
	//
	// R20260602-PERF-1 (#1627): gate the reconcile so the second O(N)
	// write-locked scan + map alloc + expvar sweep only runs when the live
	// set could actually have changed this tick. We trigger on a close /
	// prune OR on a change in the alive count vs the previous tick — the
	// latter catches a session whose process exited naturally (isAlive()
	// flips) without being closed/pruned, which still shifts the per-backend
	// gauge. On the common steady-state no-op tick all three are false and
	// the reconcile is skipped entirely.
	if closedCount > 0 || pruned > 0 || newActive != prevActive {
		r.reconcileSessionActiveByBackendLocked()
	}
	var sessionsCopy map[string]*ManagedSession
	var knownIDsCopy map[string]bool
	var wsOverridesCopy map[string]string
	storePath := r.storePath
	snapshotGen := r.storeGen.Load()
	snapshotWsGen := r.wsOverridesGen.Load()
	if r.storeDirty {
		sessionsCopy = make(map[string]*ManagedSession, len(r.sessions))
		for k, v := range r.sessions {
			sessionsCopy[k] = v
		}
	}
	if r.wsOverridesDirty {
		wsOverridesCopy = make(map[string]string, len(r.workspaceOverrides))
		for k, v := range r.workspaceOverrides {
			wsOverridesCopy[k] = v
		}
	}
	// knownIDs is append-only and relatively stable. Throttle its fsync to
	// bound disk I/O (see knownIDsSaveInterval constant). Commit
	// knownIDsSavedAt optimistically here — still under r.mu — so a
	// concurrent saveIfDirty tick on the neighboring interval boundary
	// sees the updated timestamp and skips the redundant work. (The
	// underlying tmp file is unique per WriteFileAtomic call via
	// os.CreateTemp, so this throttle is an I/O budget gate, not a
	// file-level race guard.)
	var snapshotKnownIDsGen uint64
	if r.knownIDsDirty && now.Sub(r.knownIDsSavedAt) >= knownIDsSaveInterval {
		knownIDsCopy = make(map[string]bool, len(r.knownIDs))
		for id := range r.knownIDs {
			knownIDsCopy[id] = true
		}
		snapshotKnownIDsGen = r.knownIDsGen
		r.knownIDsSavedAt = now
	}

	r.mu.Unlock()

	// Periodic save outside lock to reduce crash-recovery data loss.
	if sessionsCopy != nil {
		if err := saveStore(storePath, sessionsCopy); err != nil {
			slog.Warn("periodic session save failed", "err", err)
		} else {
			// Only clear dirty flag if no concurrent mutation occurred since snapshot.
			r.mu.Lock()
			if r.storeGen.Load() == snapshotGen {
				r.storeDirty = false
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
			if r.wsOverridesGen.Load() == snapshotWsGen {
				r.wsOverridesDirty = false
			}
			r.mu.Unlock()
		}
	}
	if knownIDsCopy != nil {
		// knownIDsSavedAt was committed under r.mu above (pre-save) to
		// gate concurrent saveIfDirty. On success we only clear the dirty
		// flag; on failure we leave it set so the next tick retries,
		// accepting one extra interval of delay in exchange for no
		// torn-write race.
		if err := saveKnownIDs(storePath, knownIDsCopy); err != nil {
			slog.Warn("periodic known IDs save failed", "err", err)
		} else {
			// Generation counter matches the (sessions | ws-overrides) pattern:
			// if a concurrent trackSessionID fired between snapshot and re-lock,
			// the gen will differ and we leave the dirty flag set so the next
			// tick retries. len()-equality alone is insufficient because an
			// add + evict pair produces identical lengths with different content.
			r.mu.Lock()
			if r.knownIDsGen == snapshotKnownIDsGen {
				r.knownIDsDirty = false
			}
			r.mu.Unlock()
		}
	}

	if len(expired) > 0 || len(stuckKill) > 0 || pruned > 0 {
		r.notifyChange()
	}
}

// shouldPrune returns true if a non-exempt session should be removed from the map.
// Covers: nil-process stubs, dead processes past pruneTTL. Caller must hold r.mu.
func (r *Router) shouldPrune(s *ManagedSession, now time.Time) bool {
	if now.Sub(s.LastActive()) <= r.pruneTTL {
		return false
	}
	proc := s.loadProcess()
	if proc == nil {
		return true // nil-process stub (with or without session ID)
	}
	return !proc.Alive() // exited process past pruneTTL
}

// cleanupLoopMaxRestarts caps how many times the cleanup loop may be
// resurrected after a panic before giving up. A genuine bug that panics
// every tick would otherwise spin forever — silently log-spamming and
// burning a goroutine budget — when the right move is to surface the
// failure loudly and stop. R229-GO-9.
const cleanupLoopMaxRestarts = 10

// StartCleanupLoop runs Cleanup periodically and saves dirty session state
// on a shorter interval to reduce data loss on crash.
func (r *Router) StartCleanupLoop(ctx context.Context, interval time.Duration) {
	r.startCleanupLoop(ctx, interval, 0)
}

// startCleanupLoop is the panic-restart-aware variant. attempt counts how
// many restarts have happened so far; the panic handler bails out once it
// reaches cleanupLoopMaxRestarts so a tick that panics deterministically
// cannot trap the process in an unbounded restart cycle.
func (r *Router) startCleanupLoop(ctx context.Context, interval time.Duration, attempt int) {
	// time.NewTicker(d) panics for d<=0; the panic-recovery defer would then
	// schedule another StartCleanupLoop via AfterFunc, which would re-panic on
	// the same NewTicker call, producing an unbounded retry chain. Reject the
	// misconfiguration up front.
	if interval <= 0 {
		slog.Warn("start cleanup loop: non-positive interval, cleanup disabled",
			"interval", interval)
		return
	}
	go func() {
		// Panic recovery: a bug inside Cleanup or saveIfDirty would silently
		// kill the loop, allowing sessions to accumulate indefinitely past
		// their TTL and losing the periodic sessions.json flush. Log with
		// stack so ops can diagnose, then re-enter the loop via a tail call.
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("router cleanup loop panic recovered",
					"panic", rec, "stack", string(debug.Stack()),
					"attempt", attempt, "max_restarts", cleanupLoopMaxRestarts)
				// Restart the loop so TTL expiry and saveIfDirty continue.
				// Guard against ctx already cancelled so we do not resurrect
				// after Shutdown. Brief backoff before relaunch so a bug that
				// panics on every tick cannot pile up a cloud of short-lived
				// restart goroutines; 5s bounds the recovery latency at the
				// same order as the cleanup tick.
				//
				// Cap restart attempts: a deterministic panic on every tick
				// would otherwise spin the recovery chain indefinitely. After
				// cleanupLoopMaxRestarts we stop and let TTL/save coverage
				// degrade — operators see the loud Error log line and can
				// restart the process to retry. R229-GO-9.
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
	// R20260531070014-PERF-9 (#1535): the snapshot phase only READS r.sessions /
	// r.workspaceOverrides / r.knownIDs and the dirty flags, so take the cheaper
	// RLock here instead of the exclusive Lock. The hot GetOrCreate / Send paths
	// that contend on r.mu can now proceed concurrently with the O(N) map copy
	// (they only need the write lock briefly to register a session); previously
	// the whole copy serialised against them. The single mutation in this path —
	// committing knownIDsSavedAt — is promoted to a short write-locked section
	// below, only when a knownIDs save is actually due.
	r.mu.RLock()
	knownIDsDue := r.knownIDsDirty && time.Since(r.knownIDsSavedAt) >= knownIDsSaveInterval
	if !r.storeDirty && !r.wsOverridesDirty && !knownIDsDue {
		r.mu.RUnlock()
		return
	}
	var sessionsCopy map[string]*ManagedSession
	if r.storeDirty {
		sessionsCopy = make(map[string]*ManagedSession, len(r.sessions))
		for k, v := range r.sessions {
			sessionsCopy[k] = v
		}
	}
	var wsOverridesCopy map[string]string
	if r.wsOverridesDirty {
		wsOverridesCopy = make(map[string]string, len(r.workspaceOverrides))
		for k, v := range r.workspaceOverrides {
			wsOverridesCopy[k] = v
		}
	}
	var knownIDsCopy map[string]bool
	var snapshotKnownIDsGen uint64
	if knownIDsDue {
		knownIDsCopy = make(map[string]bool, len(r.knownIDs))
		for id := range r.knownIDs {
			knownIDsCopy[id] = true
		}
		snapshotKnownIDsGen = r.knownIDsGen
	}
	storePath := r.storePath
	snapshotGen := r.storeGen.Load()
	snapshotWsGen := r.wsOverridesGen.Load()
	r.mu.RUnlock()

	if knownIDsDue {
		// Commit savedAt under the write lock so a concurrent Cleanup tick
		// re-checking the throttle skips — both paths share the same .tmp
		// target file and torn writes cannot be recovered. Re-verify the
		// throttle under the exclusive lock so two saveIfDirty/Cleanup ticks
		// racing past the RLock check do not both stamp + double-write.
		r.mu.Lock()
		if r.knownIDsDirty && time.Since(r.knownIDsSavedAt) >= knownIDsSaveInterval {
			r.knownIDsSavedAt = time.Now()
		} else {
			// Lost the race; another tick already claimed this save window.
			knownIDsCopy = nil
		}
		r.mu.Unlock()
	}

	if sessionsCopy != nil {
		if err := saveStore(storePath, sessionsCopy); err != nil {
			slog.Warn("periodic session save failed", "err", err)
		} else {
			r.mu.Lock()
			if r.storeGen.Load() == snapshotGen {
				r.storeDirty = false
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
			if r.wsOverridesGen.Load() == snapshotWsGen {
				r.wsOverridesDirty = false
			}
			r.mu.Unlock()
		}
	}
	if knownIDsCopy != nil {
		// savedAt committed pre-save; only toggle dirty on success.
		if err := saveKnownIDs(storePath, knownIDsCopy); err != nil {
			slog.Warn("periodic known IDs save failed", "err", err)
		} else {
			// Match the storeGen/wsOverridesGen pattern: only clear dirty if
			// no concurrent trackSessionID fired since the snapshot.
			r.mu.Lock()
			if r.knownIDsGen == snapshotKnownIDsGen {
				r.knownIDsDirty = false
			}
			r.mu.Unlock()
		}
	}
}

// Shutdown gracefully closes all sessions, waiting for running ones to complete.
// Idempotent: subsequent calls return immediately after the first completes.
//
// CONTRACT: Shutdown assumes the naozhi process terminates shortly after it
// returns. Two watcher goroutines (the one below that wraps
// `r.historyWg.Wait()` + the shim reconcile ticker in Scheduler.Stop) are
// allowed to outlive Shutdown when their work is blocked on hung I/O —
// relying on OS teardown for cleanup. If future code ever makes Router
// reusable after Shutdown (tests that spin a router up and down, hot
// reloads, etc.), those watchers would accumulate one-per-cycle. The
// R44-REL-HIST-GOROUTINE / R44-REL-TRIGGER-GOROUTINE audit items pin this
// assumption; a `TestShutdown_SingleShotContract` source-level test
// enforces `shutdownOnce` stays in place so any attempt to make Shutdown
// reversible trips CI and forces a re-audit.
func (r *Router) Shutdown() {
	r.shutdownOnce.Do(r.shutdown)
}

func (r *Router) shutdown() {
	// Cancel the history ctx so in-flight LoadHistory*Ctx calls (both startup
	// preloaders and reconnect-time chain walkers) abort instead of blocking
	// behind slow filesystem reads. The bounded Wait below provides a hard
	// deadline on top of cancellation in case a syscall is stuck past the
	// ctx check point.
	if r.historyCancel != nil {
		r.historyCancel()
	}

	// Wait for startup history-loading goroutines to finish first,
	// but don't block forever if filesystem I/O is hung (e.g. NFS).
	// Reduced from 15s to 5s now that cancellation short-circuits the
	// loaders at the next chunk/line boundary; the remaining budget is
	// for goroutines mid-syscall.
	//
	// Goroutine leak on timeout is intentional and bounded by the
	// "Shutdown is single-shot, process terminates next" contract above.
	// The wrapper goroutine exits the moment historyWg reaches zero —
	// either naturally (loaders finish) or after the CLI process hosting
	// the hung syscall is reaped by the kernel on OS teardown. Do NOT
	// replace historyWg.Wait() with a ctx-aware pattern here: the only
	// reason we spawn a goroutine at all is that WaitGroup has no
	// ctx-aware Wait; the select below IS the bounded-wait primitive.
	historyDone := make(chan struct{})
	go func() {
		// Goroutine intentionally left running on timeout; cleaned up on process exit.
		// See Shutdown godoc for the single-shot lifecycle contract that
		// makes this acceptable. R44-REL-HIST-GOROUTINE.
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
	// Deadline timer: broadcast to unblock Wait() when timeout expires.
	// R192-CONC-H1: must hold r.mu across Broadcast so the cond.Wait predicate
	// evaluation window below (lines referencing `running`) cannot race with
	// the timer firing and silently lose the wakeup. This mirrors the
	// contract documented on NotifyIdle (R183-REL-H1) and the sibling
	// Broadcast call-sites fixed in R191-CONC-H1.
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
	// R227-GO-4: log once per Shutdown when shutdownCond is nil (test
	// shape) so the busy-poll fallback isn't silent.
	shutdownCondMissingLogged := false
	for {
		running := false
		for _, s := range r.sessions {
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
			// Fallback for tests without shutdownCond. R227-GO-4: warn once
			// per Shutdown so a bare `&Router{}` test construction surfaces
			// the missing field instead of silently busy-polling for up to
			// ShutdownTimeout/100ms iterations. Production callers always
			// route through NewRouter (which initialises shutdownCond), so
			// this branch is purely a test-shape sentinel.
			if !shutdownCondMissingLogged {
				slog.Warn("shutdown: Router constructed without shutdownCond — falling back to 100ms busy-poll; tests should use NewRouter")
				shutdownCondMissingLogged = true
			}
			r.mu.Unlock()
			time.Sleep(100 * time.Millisecond)
			r.mu.Lock()
		}
	}

	// Snapshot sessions for saving outside lock
	sessionsCopy := make(map[string]*ManagedSession, len(r.sessions))
	for k, v := range r.sessions {
		sessionsCopy[k] = v
	}
	storePath := r.storePath
	knownIDsCopy := make(map[string]bool, len(r.knownIDs))
	for id := range r.knownIDs {
		knownIDsCopy[id] = true
	}
	wsOverrides := make(map[string]string, len(r.workspaceOverrides))
	for k, v := range r.workspaceOverrides {
		wsOverrides[k] = v
	}

	// Collect processes to close, then release lock to close concurrently
	var procs []processIface
	for key, s := range r.sessions {
		if p := s.loadProcess(); p != nil && p.Alive() {
			slog.Info("shutting down session", "key", key)
			procs = append(procs, p)
		}
	}
	r.mu.Unlock()

	// Save session state outside lock (avoids JSON marshal + file I/O under mutex).
	// disk_full is surfaced as a distinct structured field so monitoring can
	// page on ENOSPC separately from transient write failures; shutdown loses
	// all un-persisted state silently otherwise. Each error chain is walked
	// once — the three save paths are independent, so sharing a single flag
	// would mis-attribute a disk-full on saveStore to saveKnownIDs.
	if err := saveStore(storePath, sessionsCopy); err != nil {
		slog.Error("save session store on shutdown", "err", err, "disk_full", osutil.IsDiskFull(err))
	}
	if err := saveKnownIDs(storePath, knownIDsCopy); err != nil {
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
			// Shutdown happens last in the graceful-stop sequence, so a panic
			// inside Detach/Close (e.g. a nil shim conn from a late race)
			// would bring down the whole process and skip any remaining
			// cleanup in main. Swallow so the rest of the goroutines still
			// finish and naozhi exits cleanly.
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

	// Flush & stop the event-log persister last so any batches still in
	// the in-channel (e.g. emitted while CLIs were detaching) reach
	// disk. 5s matches the historyWg budget above — ample for the
	// typical 200 ms debounce plus a final fsync, but bounded so a
	// wedged disk doesn't hold Shutdown open.
	//
	// R230B-GO-5: ctx parent is context.Background, NOT r.historyCtx.
	// historyCtx is cancelled at the very top of Shutdown (line ~700),
	// so a child of it would observe ctx.Err() immediately and the
	// persister would skip flushing — losing the in-flight batch the
	// 5s window is meant to drain. Decoupling via Background keeps the
	// 5s budget usable; the bounded WithTimeout still prevents a
	// wedged FS from holding shutdown open.
	if r.eventLogPersister != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.eventLogPersister.Stop(ctx); err != nil {
			slog.Warn("event log persister stop timed out",
				"err", err, "stats", r.eventLogPersister.Stats())
		}
	}

	// Stop the attachment tracker AFTER the persister so no more
	// OnPersistedEntry bumps arrive during the tracker's drain.
	// Ordering matters: a bump after Stop would silently drop.
	r.stopAttachmentTracker()
}
