package session

import (
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// countActive recounts alive processes (corrects drift from undetected exits).
// Exempt sessions are not counted toward max_procs capacity. Caller must
// hold r.mu.
//
// R20260603-010128-GO-2: reuse the alive total returned by
// reconcileSessionActiveByBackendLocked instead of running a separate O(N)
// counting loop. reconcile already walks r.sessions once to rebuild the
// per-backend gauges and returns the non-exempt alive count, which is
// identical to what the former loop computed. Dropping the redundant walk
// keeps activeCount and the per-backend gauges updated in a single pass.
func (r *Router) countActive() {
	// reconcileSessionActiveByBackendLocked walks r.sessions once, drives
	// the per-backend labeled gauge, and returns the non-exempt alive total.
	// Store that total directly — no second O(N) counting loop needed.
	count := r.reconcileSessionActiveByBackendLocked()
	r.activeCount.Store(count)
}

// reconcileSessionActiveByBackendLocked rebuilds the metrics.SessionActive
// pair (legacy unlabeled mirror + per-backend labeled gauge) from r.sessions.
//
// Used by bulk teardown paths (countActive / cleanupSessionsByChatPrefix /
// Cleanup prune) where per-key Inc/Dec bookkeeping in the loop would
// require threading each session's backend through the batched-close
// machinery. Single-session sites (Reset / Remove) call
// metrics.RecordSessionActive(backend, -1) directly for lower overhead.
// evictOldest deliberately does NOT — it relies on the post-Close
// countActive() reconcile instead, because a manual -1 layered on top of an
// absolute reconcile drifts the gauge (#1645).
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
	return total
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

// countExemptCombined returns both the alive exempt count for a single
// namespace and the global alive exempt total in ONE walk of r.sessions.
// Caller must hold r.mu.
//
// R20260603-PERF-1: spawnSession's exempt branch previously called
// countExemptByKind then countExempt back-to-back — two O(N) sweeps of the
// same map under the write lock on every exempt spawn. Folding them into a
// single pass halves the work and the lock-held time without introducing the
// drift risk of standalone atomic counters (which the file's existing comments
// document as easy to desync). When kind=="" the per-kind result is 0,
// matching countExemptByKind's defensive contract.
func (r *Router) countExemptCombined(kind string) (perKind int, total int) {
	for k, s := range r.sessions {
		if !s.exempt || !s.isAlive() {
			continue
		}
		total++
		if kind != "" && exemptKind(k) == kind {
			perKind++
		}
	}
	return perKind, total
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
	// #1645: do NOT explicitly decrement the labeled gauge here. The
	// post-Close r.countActive() below calls reconcileSessionActiveByBackendLocked,
	// which SETS each backend bucket to the absolute recounted value
	// (Add(want-current)), not a relative delta. A manual -1 on top of an
	// absolute reconcile is at best a no-op and at worst causes drift: when
	// proc.Close() flips Alive() asynchronously, the recount can still observe
	// oldest as alive, so reconcile drives the gauge back up to include it —
	// and the earlier manual -1 made the pre-reconcile baseline diverge from
	// the true count. Relying solely on the reconcile keeps the gauge equal to
	// whatever the recount sees, which is the single source of truth.
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
