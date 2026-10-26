package session

import (
	"fmt"
	"log/slog"
	"time"
)

// countActive recounts alive processes (corrects drift from undetected exits).
// Exempt sessions are not counted toward max_procs capacity.
func (r *Router) countActive(tx sessTx) {
	tx.SetActive(countAlive(tx.View))
}

// countAlive returns the alive non-exempt session count.
func countAlive(v sessView) int64 {
	var total int64
	for _, s := range v.All() {
		if !s.exempt && s.isAlive() {
			total++
		}
	}
	return total
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

// takeoverHasSlot is reserveSpawn's non-exempt capacity gate for a Takeover of
// key, read-only. Takeover closes an alive session on key first, so it holds
// no slot and cannot be the eviction victim. Admission passes on the active
// count, else on a recount, else when evicting one idle session makes room.
func takeoverHasSlot(v sessView, key string, maxProcs int) bool {
	active := v.Active()
	var alive int64
	evictable := false
	for k, s := range v.All() {
		if s.exempt || !s.isAlive() {
			continue
		}
		if k == key {
			active--
			continue
		}
		alive++
		evictable = evictable || !s.loadProcess().IsRunning()
	}
	pending, limit := int64(v.Ext().PendingSpawns()), int64(maxProcs)
	return min(active, alive)+pending < limit || (evictable && alive-1+pending < limit)
}

// takeoverExemptRefusal is reserveSpawn's exempt quota gate for a Takeover of
// key, read-only. Takeover closes an alive session on key first, so it is
// not counted against either quota.
func takeoverExemptRefusal(v sessView, key string) error {
	kind := exemptKind(key)
	perKind, total := countExemptCombined(v, kind)
	if s, ok := v.Lookup(key); ok && s.exempt && s.isAlive() {
		total--
		if kind != "" {
			perKind--
		}
	}
	if kind != "" && perKind >= exemptCapFor(kind) {
		return fmt.Errorf("%w: %s namespace (%d)", ErrMaxExemptSessions, kind, exemptCapFor(kind))
	}
	if total >= maxExemptSessions {
		return fmt.Errorf("%w (%d)", ErrMaxExemptSessions, maxExemptSessions)
	}
	return nil
}

// evictOldest closes the oldest idle (non-Running) session to free a slot,
// preferring one whose CLI runs no workflow; any idle session still counts
// as evictable (takeoverHasSlot). Close() runs with the lock released
// (tx.Unlocked) so it does not block other goroutines. Returns true if a
// session was evicted.
func (r *Router) evictOldest(tx sessTx) bool {
	now := time.Now()
	var oldest, oldestPinned *ManagedSession
	for _, s := range tx.All() {
		if s.exempt {
			continue // planner sessions are never evicted
		}
		if !s.isAlive() || s.loadProcess().IsRunning() {
			continue
		}
		pick := &oldest
		if s.workflowPinned(now) {
			pick = &oldestPinned
		}
		if *pick == nil || s.LastActive().Before((*pick).LastActive()) {
			*pick = s
		}
	}
	if oldest == nil {
		oldest = oldestPinned
	}
	if oldest == nil {
		return false
	}
	slog.Info("evicting oldest session", "key", oldest.key, "idle", time.Since(oldest.LastActive()))
	// Do NOT decrement activeCount here: countActive() below recounts, and a
	// manual -1 on top of that drifts when Close() flips Alive() asynchronously
	// (#1645).
	storeAtomicString(&oldest.deathReason, DeathReasonEvicted)
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
