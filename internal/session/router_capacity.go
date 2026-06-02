package session

import (
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// countActive recounts alive processes (corrects drift from undetected exits).
// Exempt sessions are not counted toward max_procs capacity. Caller must
// hold r.mu.
func (r *Router) countActive() {
	count := int64(0)
	for _, s := range r.sessions {
		if s.exempt {
			continue
		}
		if s.isAlive() {
			count++
		}
	}
	r.activeCount.Store(count)
	// Multi-Backend RFC §10 (Sprint 6a): keep the per-backend labeled
	// gauge in sync with the freshly recounted r.sessions snapshot.
	// Done in the same critical section as activeCount.Store so the
	// two bookkeeping totals never diverge from each other.
	r.reconcileSessionActiveByBackendLocked()
}

// reconcileSessionActiveByBackendLocked rebuilds the metrics.SessionActive
// pair (legacy unlabeled mirror + per-backend labeled gauge) from r.sessions.
//
// Used by bulk teardown paths (countActive / cleanupSessionsByChatPrefix /
// Cleanup prune) where per-key Inc/Dec bookkeeping in the loop would
// require threading each session's backend through the batched-close
// machinery. Single-session sites (Reset / Remove / evictOldest) call
// metrics.RecordSessionActive(backend, -1) directly for lower overhead.
//
// Backends that previously had sessions but no longer do are explicitly
// driven to zero — without ForEachKey the bucket would stay stuck at
// its last non-zero value.
//
// Returns the freshly counted alive (non-exempt) session total so callers
// that also need to refresh r.activeCount can reuse this single O(N) walk
// instead of running a second counting loop. R20260602190132-PERF-5 (#1607).
//
// LOCK: caller must hold r.mu for writing.
func (r *Router) reconcileSessionActiveByBackendLocked() int64 {
	var total int64
	perBackend := make(map[string]int64, 4)
	for _, s := range r.sessions {
		if s.exempt {
			continue
		}
		if s.isAlive() {
			total++
			perBackend[s.Backend()]++
		}
	}
	// Reconcile the unlabeled mirror (naozhi_session_active) by setting
	// it to the freshly counted total. expvar.Int has no Set, so use
	// Add(want - current) which is atomic enough for a gauge.
	currentTotal := metrics.SessionActive.Value()
	if delta := total - currentTotal; delta != 0 {
		metrics.SessionActive.Add(delta)
	}
	// Reconcile the labeled gauge per backend.
	allBackends := map[string]struct{}{}
	for k := range perBackend {
		allBackends[k] = struct{}{}
	}
	metrics.SessionActiveByBackend.ForEachKey(func(k string) {
		allBackends[k] = struct{}{}
	})
	// Drive each backend's gauge to its authoritative count via a single
	// atomic Add per key. The previous "loop of N Inc/Dec" exposed
	// partial intermediate values to /debug/vars scrapers between
	// iterations: we hold r.mu but not any lock on the expvar map, so a
	// reader sweeping concurrently could observe e.g. N/2 sessions for
	// 'kiro' while the loop was halfway through. expvar.Map.Add is
	// atomic per key, so the reconcile transition is now a single jump.
	for backend := range allBackends {
		current := metrics.SessionActiveByBackend.Get(backend)
		want := perBackend[backend]
		metrics.SessionActiveByBackend.Add(want-current, backend)
	}
}

// countExempt returns the total number of alive exempt sessions across
// all namespaces. Caller must hold r.mu.
//
// R242-ARCH-2: kept for the global-cap relief-valve check in spawn.
// Per-namespace gating goes through countExemptByKind so cron / planner
// / sys quotas are isolated.
func (r *Router) countExempt() int {
	count := 0
	for _, s := range r.sessions {
		if s.exempt && s.isAlive() {
			count++
		}
	}
	return count
}

// countExemptByKind returns the alive exempt-session count for a single
// namespace ("cron" / "project" / "sys"). Caller must hold r.mu. R242-
// ARCH-2: the per-kind walk is O(N) like countExempt but typed against
// the prefix family so a noisy cron chat (DefaultMaxJobsPerChat × N
// chats) can no longer push planner / sys stubs out of the global pool.
//
// "" kind returns 0 (defensive; an exempt session whose key matches no
// known prefix is a misconfiguration that countExempt also misses; a
// future audit hook should panic on encountering one but the current
// "log+continue" stance avoids a startup crash on operator misconfig).
func (r *Router) countExemptByKind(kind string) int {
	if kind == "" {
		return 0
	}
	count := 0
	for k, s := range r.sessions {
		if !s.exempt || !s.isAlive() {
			continue
		}
		if exemptKind(k) == kind {
			count++
		}
	}
	return count
}

// evictOldest closes the oldest idle (non-Running) session to free a slot.
// Releases and re-acquires r.mu during Close() to avoid blocking other goroutines.
// Returns true if a session was evicted.
func (r *Router) evictOldest() bool {
	var oldest *ManagedSession
	for _, s := range r.sessions {
		if s.exempt {
			continue // planner sessions are never evicted
		}
		if !s.isAlive() || s.loadProcess().IsRunning() {
			continue
		}
		if oldest == nil || s.LastActive().Before(oldest.LastActive()) {
			oldest = s
		}
	}
	if oldest == nil {
		return false
	}
	slog.Info("evicting oldest session", "key", oldest.key, "idle", time.Since(oldest.LastActive()))
	// OBS2: bump before Unlock so it aligns with the "decision to evict" point;
	// the subsequent proc.Close() is async-capable and can fail, but the eviction
	// decision is already committed (deathReason set, storeDirty marked below).
	metrics.SessionEvictTotal.Add(1)
	// Multi-Backend RFC §10 (Sprint 6a): evictOldest below relies on
	// r.countActive() to recompute the legacy total post-Close, but the
	// labeled gauge needs an explicit decrement keyed on the evictee's
	// backend. Done now (under the lock, before Unlock for Close) so the
	// metric reflects the eviction decision instead of the post-Close
	// recount which only sees the residual sessions.
	metrics.RecordSessionActive(oldest.Backend(), -1)
	storeAtomicString(&oldest.deathReason, "evicted")
	// Keep oldest.process non-nil so concurrent holders don't get nil-panic.
	// After Close(), Alive() returns false; countActive() below recounts correctly.
	//
	// Eviction does not re-spawn for the same key (it just frees a slot for
	// the next unrelated GetOrCreate), so we deliberately skip
	// waitSocketGoneForKey here. If a future caller starts immediately
	// re-spawning the evicted key, add it — see the UCCLEP-2026-04-26
	// fix in Reset/ResetAndRecreate/Takeover for the pattern.
	proc := oldest.loadProcess()
	r.mu.Unlock()
	proc.Close()
	r.mu.Lock()
	// R191-CONC-H1: Broadcast under r.mu to avoid missed-wakeup window with
	// Shutdown's cond.Wait. sync.Cond.Broadcast docs say holding L is optional
	// only when the wakeup predicate is purely state-atomic; here the predicate
	// reads r.sessions[*].loadProcess().IsRunning() which the Close() above
	// just flipped. R183-REL-H1 established the "hold r.mu across Broadcast"
	// pattern for NotifyIdle; extending here to evict/reset/remove/cleanup
	// (R191-CONC-H1-a/b/c/d).
	if r.shutdownCond != nil {
		r.shutdownCond.Broadcast()
	}
	r.countActive() // recount instead of manual decrement to avoid double-count races
	// Mark store dirty + bump version so the eviction is persisted on the
	// next save cycle and propagated to the dashboard on the next Version()
	// poll. Without these two lines, a crash within the (up to) 60-second
	// save interval re-spawns the evicted session on restart, and dashboards
	// polling on version diff skip the refresh that would remove the dead
	// session from the sidebar. Every other mutation site pairs liveness
	// changes with this pattern. R59-GO-H2.
	r.storeDirty = true
	r.storeGen.Add(1)
	return true
}
