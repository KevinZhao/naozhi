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

// removeSnapshot captures everything finishRemoveCleanup needs from a
// session AFTER unregisterSessionLocked has dropped it from r.sessions.
// Every field is a value/pointer snapshot taken under r.mu so the
// post-unlock teardown (which may run in a detached goroutine via
// RemoveAsync) never reads router state again — see M3 in the
// async-remove review: finishRemoveCleanup MUST NOT touch r.sessions,
// r.activeCount, r.storeDirty or r.storeGen (all finalised in the
// locked section below).
type removeSnapshot struct {
	proc             processIface
	workspace        string
	retiredSessionID string
}

// unregisterAndSnapshot runs the fast, locked half of a session removal:
// look the session up, unregister it from r.sessions (and all secondary
// indexes), finalise the active-count and dirty/version bookkeeping, then
// hand back a value snapshot for the slow teardown. Returns ok=false when
// the key is absent — a concurrent Remove/RemoveAsync for the same key
// loses the lookup race here and never proceeds to finishRemoveCleanup
// (review M1: the lookup+delete is atomic under r.mu so the two callers
// cannot both capture a non-nil proc).
func (r *Router) unregisterAndSnapshot(key string) (removeSnapshot, bool) {
	r.mu.Lock()
	s, ok := r.sessions[key]
	if !ok {
		r.mu.Unlock()
		return removeSnapshot{}, false
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

	return removeSnapshot{
		proc:             proc,
		workspace:        workspaceSnapshot,
		retiredSessionID: retiredSessionID,
	}, true
}

// finishRemoveCleanup runs the slow, unlocked half of a session removal:
// close the process, wait for its shim socket to disappear, drop the
// event log + attachment refs, then fire the lifecycle notifications.
// Must be called WITHOUT r.mu held. Reads only `snap` — never router
// state — so it is safe to run in a detached goroutine concurrently with
// any number of other router operations (the session is already gone
// from every map by the time this runs).
//
// Worst case ~15s: proc.Close (8s) + waitSocketGoneForKey (2s) +
// dropEventLogForKey (2s) + clearAttachmentTrackerRefs (5s). When invoked
// async via RemoveAsync the HTTP handler has long since returned, so this
// latency no longer blocks the dashboard.
func (r *Router) finishRemoveCleanup(key string, snap removeSnapshot) {
	proc := snap.proc
	if proc != nil && proc.Alive() {
		proc.Close()
		// Async-remove review H2: the dashboard "close session" gesture is
		// frequently followed by an immediate same-key re-create. With the
		// teardown now potentially running in a detached goroutine, proc.Close
		// no longer completes before HandleDelete returns, so the shim socket
		// can still be bound when the next GetOrCreate dials it — hitting the
		// "refusing to clobber" dial-first guard. Mirror finishResetUnlocked:
		// wait up to 2s for the socket to vanish and, on timeout, flag the key
		// so the next GetOrCreate wraps its spawn error with ErrShimStuck for
		// an actionable diagnosis instead of the generic session error.
		if !waitSocketGoneForKey(key, 2*time.Second) {
			r.mu.Lock()
			if r.shimStuckOnReset == nil {
				r.shimStuckOnReset = make(map[string]bool)
			}
			r.shimStuckOnReset[key] = true
			r.mu.Unlock()
			slog.Warn("shim socket still bound after Remove wait — flagging key for ErrShimStuck wrap on next GetOrCreate",
				"key", key)
		}
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
	r.clearAttachmentTrackerRefs(key, snap.workspace)
	// R191-CONC-H1-c: Broadcast under r.mu (see evictOldest comment).
	// Async-remove review H1: this Broadcast is NOT load-bearing for
	// Shutdown — the session left r.sessions in unregisterAndSnapshot, so
	// Shutdown's running-detection loop can never see this proc and never
	// waits on it. Kept under r.mu solely to match the shape of every
	// other Broadcast site (a bare Broadcast would confuse the race
	// detector and future readers).
	if r.shutdownCond != nil {
		r.mu.Lock()
		r.shutdownCond.Broadcast()
		r.mu.Unlock()
	}

	logSessionLifecycle("removed", key)
	r.notifyKeyRetired(key, snap.retiredSessionID)
	r.notifyChange()
}

// Remove removes a session from the router and kills its process,
// blocking until the full teardown completes. Used by Shutdown, Cleanup,
// and tests that assert post-teardown state synchronously (e.g. the
// attachment-tracker integration test). Dashboard deletes use RemoveAsync
// instead so the HTTP handler is not held for the worst-case ~15s
// teardown.
func (r *Router) Remove(key string) bool {
	snap, ok := r.unregisterAndSnapshot(key)
	if !ok {
		return false
	}
	r.finishRemoveCleanup(key, snap)
	return true
}

// RemoveAsync removes a session from the router immediately (synchronous,
// locked unregister) and runs the slow teardown — proc.Close, socket
// wait, event-log drop, attachment clear, notifications — in a detached
// goroutine. Returns true the instant the session is gone from r.sessions
// so the dashboard DELETE handler can reply 200 without waiting on the
// worst-case ~15s teardown.
//
// The teardown goroutine is intentionally NOT tracked by Shutdown (per
// the single-shot + bounded-leak contract on Shutdown's godoc). Each
// goroutine self-terminates in ≤15s — it is bounded, not a permanent
// leak — so a long-running process accumulates at most a handful of them
// during a burst of deletes. removeWg exists ONLY so tests can wait for
// the teardown to finish before asserting; it is never consulted by
// production teardown paths.
func (r *Router) RemoveAsync(key string) bool {
	snap, ok := r.unregisterAndSnapshot(key)
	if !ok {
		return false
	}
	r.removeWg.Add(1)
	go func() {
		defer r.removeWg.Done()
		// HandleDelete has already returned 200, so a panic in the
		// teardown chain (Close → Kill → shimSendLocked on a wedged
		// socket) has no caller to recover it. Swallow + count it, like
		// the Shutdown detach goroutine (R44).
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
		s      *ManagedSession
		key    string
		proc   processIface
		reason string // deathReason to stamp; written only after kill re-verify
	}

	now := time.Now()

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
	// R20260602190132-PERF-5 (#1607): collect prune candidates in this same
	// RLock pass so the later write-locked prune section only has to re-verify
	// and delete a small known set (O(expired)) instead of re-ranging the whole
	// map under the exclusive lock (O(N)), which serialised every concurrent
	// GetOrCreate/Send for the duration of the scan.
	var pruneCandidates []string
	for key, s := range r.sessions {
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
			// Carry the reason on the entry and stamp it only after the
			// close-loop re-verify confirms the proc is still current,
			// mirroring stuckKill. Stamping here (pass-2, no r.mu) would
			// corrupt the deathReason of a replacement session spawned
			// between this snapshot and the close loop. R20260605-GO.
			expired = append(expired, expiredEntry{c.s, c.key, c.proc, "idle_timeout"})
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
		// (stamped to "stuck_running" / "pid_gone" below, after re-verify) on the
		// fresh ManagedSession the user is actively using. Skip when the
		// session has moved on; the new proc gets its own chance next
		// tick. shouldPrune already handles the orphan-process side; this
		// just stops the false-positive kill bookkeeping.
		if cur := e.s.loadProcess(); cur != nil && cur != e.proc {
			continue
		}
		// Stamp deathReason only after re-verify confirms this proc is still
		// the live process on the session. Premature stamping (before re-verify)
		// would corrupt the deathReason of a freshly-spawned replacement
		// session visible on the dashboard. R20260603-GO-8.
		if e.reason != "" {
			storeAtomicString(&e.s.deathReason, e.reason)
		}
		e.proc.Kill()
		closedCount++
	}
	// TTL-expired sessions are closed but never re-spawned for the same
	// key by this function, so waitSocketGoneForKey is unnecessary here.
	// The next unrelated GetOrCreate will hash to a different socket.
	for _, e := range expired {
		// Same re-verify as stuckKill above: pass-2 ran without r.mu, so a
		// concurrent spawnSession / resetLocked may have replaced s.process
		// between the snapshot and now. Skip when the session has moved on so
		// we neither close the replacement's live proc nor stamp idle_timeout
		// onto the fresh ManagedSession the user is actively using. R217-CR-3.
		if cur := e.s.loadProcess(); cur != nil && cur != e.proc {
			continue
		}
		// Stamp deathReason only after re-verify confirms this proc is still
		// the live process on the session (mirrors stuckKill). R20260605-GO.
		if e.reason != "" {
			storeAtomicString(&e.s.deathReason, e.reason)
		}
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
	// R20260602190132-PERF-5 (#1607): only re-verify and delete the candidate
	// keys snapshotted under RLock in pass-1, not the whole map. The write
	// lock is now held for O(expired) prune work instead of an O(N) range,
	// so concurrent GetOrCreate/Send stall for far less time on large
	// deployments. Each candidate is re-checked under the exclusive lock
	// because the session's process/lastActive may have changed (respawn,
	// a fresh Send) between the RLock snapshot and here — a stale candidate
	// that no longer satisfies shouldPrune must NOT be removed.
	var pruned int
	for _, key := range pruneCandidates {
		s, ok := r.sessions[key]
		if !ok || s.exempt {
			continue // already gone, or became exempt — skip
		}
		if !r.shouldPrune(s, now) {
			continue // state changed since the RLock snapshot; leave it
		}
		// Terminal removal: free the backend override too (previous versions
		// leaked it; see MED-5 in 2026-04-26 architecture review).
		r.unregisterSessionLocked(key, s, false)
		pruned++
	}
	// Multi-Backend RFC §10 (Sprint 6a): recompute the labeled gauge and the
	// authoritative alive total in one pass. reconcile already walks the map
	// to drive the per-backend gauge; reuse its alive total to set activeCount
	// instead of maintaining a second counting loop. R20260602190132-PERF-5.
	// R20260603-PERF-7: skip the O(N) reconcile walk when nothing changed;
	// reuse the already-accurate activeCount instead.
	var aliveTotal int64
	if closedCount > 0 || pruned > 0 {
		aliveTotal = r.reconcileSessionActiveByBackendLocked()
	} else {
		aliveTotal = r.activeCount.Load()
	}
	r.activeCount.Store(aliveTotal)

	// Snapshot sessions for periodic save (while still holding the lock).
	// Skip save if nothing changed since last Cleanup cycle.
	if closedCount > 0 || pruned > 0 {
		r.storeDirty = true
		r.storeGen.Add(1)
	}
	// R20260602190132-PERF-4 (#1606): snapshot the dirty maps into the
	// smallest shape the save path needs. saveStoreSlice only iterates session
	// values, so a []*ManagedSession slice avoids re-allocating a whole
	// map[string]*ManagedSession (hashmap buckets + load-factor slack) on every
	// tick. The ws-overrides copy stays a map because its save path keys by
	// string; knownIDs uses the gen-memoised sorted slice (R220123-PERF-19).
	var sessionsCopy []*ManagedSession
	var knownIDsCopy []string
	var wsOverridesCopy map[string]string
	storePath := r.storePath
	snapshotGen := r.storeGen.Load()
	snapshotWsGen := r.wsOverridesGen.Load()
	if r.storeDirty {
		sessionsCopy = make([]*ManagedSession, 0, len(r.sessions))
		for _, v := range r.sessions {
			sessionsCopy = append(sessionsCopy, v)
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
		// R220123-PERF-19 (#1638): sorted snapshot is memoised by gen, so
		// the O(N log N) sort is skipped when the set is unchanged.
		knownIDsCopy = r.snapshotKnownIDsSortedLocked()
		snapshotKnownIDsGen = r.knownIDsGen
		r.knownIDsSavedAt = now
	}

	r.mu.Unlock()

	// Periodic save outside lock to reduce crash-recovery data loss.
	if sessionsCopy != nil {
		if err := saveStoreSlice(storePath, sessionsCopy); err != nil {
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
		// flag; on failure reset savedAt to zero so the throttle gate
		// re-opens on the next tick (R20260603-CODE-4).
		if err := saveKnownIDs(storePath, knownIDsCopy); err != nil {
			slog.Warn("periodic known IDs save failed", "err", err)
			r.mu.Lock()
			r.knownIDsSavedAt = time.Time{}
			r.mu.Unlock()
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
	// R20260602190132-PERF-4 (#1606): slice snapshot, not a map copy — see the
	// matching note in Cleanup. saveStoreSlice only needs session values.
	var sessionsCopy []*ManagedSession
	if r.storeDirty {
		sessionsCopy = make([]*ManagedSession, 0, len(r.sessions))
		for _, v := range r.sessions {
			sessionsCopy = append(sessionsCopy, v)
		}
	}
	var wsOverridesCopy map[string]string
	if r.wsOverridesDirty {
		wsOverridesCopy = make(map[string]string, len(r.workspaceOverrides))
		for k, v := range r.workspaceOverrides {
			wsOverridesCopy[k] = v
		}
	}
	var knownIDsCopy []string
	var snapshotKnownIDsGen uint64
	if knownIDsDue {
		// R220123-PERF-19 (#1638): memoised sorted snapshot.
		knownIDsCopy = r.snapshotKnownIDsSortedLocked()
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
		if err := saveStoreSlice(storePath, sessionsCopy); err != nil {
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
		// R20260603-CODE-4: reset savedAt on failure so the throttle gate
		// re-opens on the next tick rather than blocking for a full interval.
		if err := saveKnownIDs(storePath, knownIDsCopy); err != nil {
			slog.Warn("periodic known IDs save failed", "err", err)
			r.mu.Lock()
			r.knownIDsSavedAt = time.Time{}
			r.mu.Unlock()
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

	// Snapshot sessions for saving outside lock.
	// R20260603-PERF-1: use a value slice instead of a map copy so we avoid
	// allocating hashmap buckets + load-factor slack; saveStoreSlice only
	// iterates values, so the key is never needed here.
	sessionsCopy := make([]*ManagedSession, 0, len(r.sessions))
	for _, v := range r.sessions {
		sessionsCopy = append(sessionsCopy, v)
	}
	storePath := r.storePath
	// R220123-PERF-19 (#1638): sorted snapshot for the final flush too, so
	// saveKnownIDs receives the deterministic ordering it now requires.
	knownIDsCopy := r.snapshotKnownIDsSortedLocked()
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
	if err := saveStoreSlice(storePath, sessionsCopy); err != nil {
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
