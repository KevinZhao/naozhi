package session

import (
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// countActive recounts alive processes (corrects drift from undetected exits).
// Exempt sessions are not counted toward max_procs capacity. Reuses the single
// walk done by reconcileActiveByBackend so the active count and the
// per-backend gauges update in one pass.
func (r *Router) countActive(tx sessTx) {
	tx.SetActive(r.reconcileActiveByBackend(tx.View))
}

// reconcileActiveByBackend rebuilds the metrics.SessionActive
// pair (unlabeled mirror + per-backend labeled gauge) from the session table and
// returns the alive non-exempt total so callers can refresh the active count
// from the same O(N) walk (#1607). Used by bulk teardown paths; single-session
// sites (Reset / Remove) call metrics.RecordSessionActive(backend, -1) directly.
// evictOldest must NOT add a manual -1 on top of this absolute reconcile —
// that drifts the gauge (#1645). Backends with no remaining sessions are
// driven to zero via ForEachKey, otherwise their bucket sticks at its last value.
func (r *Router) reconcileActiveByBackend(v sessView) int64 {
	var total int64
	perBackend := make(map[string]int64, 4)
	for _, s := range v.All() {
		if s.exempt {
			continue
		}
		if s.isAlive() {
			total++
			perBackend[s.Backend()]++
		}
	}
	// expvar.Int has no Set, so drive the unlabeled mirror via Add(want-current).
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
	// One atomic Add per key: the table lock does not guard the expvar map, so a
	// /debug/vars scraper racing a loop of N Inc/Dec would observe partial
	// intermediate values. A single jump per backend avoids that.
	for backend := range allBackends {
		current := metrics.SessionActiveByBackend.Get(backend)
		want := perBackend[backend]
		metrics.SessionActiveByBackend.Add(want-current, backend)
	}
	return total
}

// countExempt returns the total number of alive exempt sessions across
// all namespaces (the global-cap relief valve in spawn). Per-namespace gating
// goes through countExemptByKind.
func countExempt(v sessView) int {
	count := 0
	for _, s := range v.All() {
		if s.exempt && s.isAlive() {
			count++
		}
	}
	return count
}

// countExemptByKind returns the alive exempt-session count for a single
// namespace ("cron" / "project" / "sys") so a noisy cron chat cannot push
// planner / sys stubs out of the global pool. kind == "" returns 0 (an exempt
// session matching no known prefix is a misconfiguration; log+continue rather
// than crash at startup).
func countExemptByKind(v sessView, kind string) int {
	if kind == "" {
		return 0
	}
	count := 0
	for k, s := range v.All() {
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
// namespace and the global alive exempt total in ONE walk of the session table
// (halves lock-held time on exempt spawns without drift-prone standalone
// counters). kind == "" yields perKind 0.
func countExemptCombined(v sessView, kind string) (perKind int, total int) {
	for k, s := range v.All() {
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
// Close() runs with the lock released (tx.Unlocked) so it does not block
// other goroutines. Returns true if a session was evicted.
func (r *Router) evictOldest(tx sessTx) bool {
	var oldest *ManagedSession
	for _, s := range tx.All() {
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
	// Bump at the "decision to evict" point: proc.Close() may fail, but the
	// eviction is already committed (deathReason set, store dirtied below).
	metrics.SessionEvictTotal.Add(1)
	// Do NOT decrement the labeled gauge here: countActive() below reconciles
	// each bucket to an absolute recount, and a manual -1 on top of that
	// drifts the gauge when Close() flips Alive() asynchronously (#1645).
	storeAtomicString(&oldest.deathReason, "evicted")
	// Keep oldest.process non-nil so concurrent holders don't nil-panic; after
	// Close(), Alive() is false and countActive() recounts. Eviction never
	// re-spawns the same key, so waitSocketGoneForKey is deliberately skipped
	// (add it if a caller starts re-spawning the evicted key immediately).
	proc := oldest.loadProcess()
	tx.Unlocked(proc.Close)
	// Broadcast under the lock: Shutdown's Wait predicate reads each
	// process's IsRunning(), which Close() just flipped, so an unlocked
	// Broadcast could be missed.
	tx.Broadcast()
	r.countActive(tx) // recount instead of manual decrement to avoid double-count races
	// Dirty + bump version so the eviction persists on the next save and the
	// dashboard's Version() poll refreshes; otherwise a crash inside the save
	// interval re-spawns the evicted session on restart and the sidebar keeps
	// the dead entry.
	tx.MarkChanged()
	return true
}
