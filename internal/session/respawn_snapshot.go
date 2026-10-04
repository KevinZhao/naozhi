package session

import (
	"slices"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/costledger"
)

// snapshotOldSession captures the per-session fields a spawn needs after it
// leaves the transaction. Pure read; nil-safe. It takes a View because these
// fields are written inside transactions by sibling paths (RegisterCronStubWithChain,
// evictOldest, the spawn itself), so reading them outside one races those
// writers.
func snapshotOldSession(_ sessView, old *ManagedSession) ([]string, float64, float64, int64, sessionOverrides) {
	if old == nil {
		return nil, 0, 0, 0, sessionOverrides{}
	}
	var oldPrevIDs []string
	if len(old.prevSessionIDs) > 0 {
		oldPrevIDs = make([]string, len(old.prevSessionIDs))
		copy(oldPrevIDs, old.prevSessionIDs)
	}
	// Preserve cumulative cost across process replacement so the dashboard
	// doesn't flash $0.00. Prefer the live process's value; fall back to the
	// store-restored total when no process is attached.
	var oldTotalCost float64
	if p := old.loadProcess(); p != nil {
		oldTotalCost = p.TotalCost()
	}
	if oldTotalCost == 0 {
		oldTotalCost = loadTotalCost(&old.totalCost)
	}
	// Carry the original creation timestamp so the session keeps its sidebar
	// position; installFreshSession stamps now when zero.
	oldCreatedAt := old.createdAt.Load()
	// costSpent MUST carry across the replacement (same logical session).
	// lastCumulativeCost is NOT carried: the new CLI counts from 0, or from the
	// cost-state a resume restores (resumed_cost.go). Known bounded loss: a turn
	// still in flight on the OLD process lands its delta on the orphaned struct;
	// cost is advisory, not billing-authoritative (#2284).
	oldCostSpent := loadTotalCost(&old.costSpent)
	// Overrides come from the same object as history/cost/createdAt. They are
	// never re-read through the key's entry, which may be swapped or removed
	// during the unlocked part of a spawn, pairing one session's history with
	// another's tuning; completeSpawn re-reads them from old itself.
	return oldPrevIDs, oldTotalCost, oldCostSpent, oldCreatedAt, snapshotOverrides(old)
}

// snapshotOverrides reads old's operator-owned overrides. Nil-safe; call it
// inside a transaction.
func snapshotOverrides(old *ManagedSession) sessionOverrides {
	if old == nil {
		return sessionOverrides{}
	}
	return sessionOverrides{
		tuningModel:  old.TuningModel(),
		tuningEffort: old.TuningEffort(),
		userLabel:    old.UserLabel(),
		labelOrigin:  old.LabelOrigin(),
	}
}

// respawnSnapshot is what a respawn carries over from the session it
// replaces, read under the table lock in one critical section.
type respawnSnapshot struct {
	sid       string // the ID being replaced; installFreshSession clears idToKey[sid] on rotation
	prevIDs   []string
	cost      float64 // the replaced process's cumulative cost (loadTotalCost fallback)
	costSpent float64
	createdAt int64
	overrides sessionOverrides
	// spent is the monotonic metering total, which follows the logical
	// session across process replacement like costSpent.
	spent costledger.Totals
	// startupFails is the startup-failure streak the replacement inherits.
	startupFails int32
}

func snapshotRespawn(v sessView, old *ManagedSession) respawnSnapshot {
	var snap respawnSnapshot
	snap.prevIDs, snap.cost, snap.costSpent, snap.createdAt, snap.overrides = snapshotOldSession(v, old)
	if old != nil {
		snap.sid = old.getSessionID()
		snap.spent = old.CostTotals()
		snap.startupFails = startupFailureOf(old).streak
	}
	return snap
}

// rereadSameEntry refreshes snap and hist from old, still the key's entry at
// commit, with what operator writes may have changed on it while the spawn
// ran unlocked: the overrides and the session-ID chain. History and cost are
// written only by old's own, dead, process. Call it inside the commit
// transaction; nil-safe.
func rereadSameEntry(old *ManagedSession, snap *respawnSnapshot, hist *respawnHistory, resumeID string) {
	if old == nil {
		return
	}
	snap.overrides = snapshotOverrides(old)
	if !slices.Equal(old.prevSessionIDs, snap.prevIDs) {
		snap.prevIDs = slices.Clone(old.prevSessionIDs)
		hist.prevIDs = respawnChain(snap.prevIDs, old.getSessionID(), resumeID)
	}
}

// respawnHistory is the replaced session's history, copied outside the table lock.
type respawnHistory struct {
	entries   []clievent.EventEntry
	prevIDs   []string
	userTurns int64
}

func collectRespawnHistory(old *ManagedSession, snap respawnSnapshot, resumeID string) respawnHistory {
	entries, prevIDs, userTurns := collectPreviousHistory(old, snap.prevIDs, resumeID)
	return respawnHistory{entries: entries, prevIDs: prevIDs, userTurns: userTurns}
}

// collectPreviousHistory gathers JSONL-backed history entries and the
// session ID chain for a respawn. Returns (entries, chain, userTurns);
// userTurns is computed here so the spawn path seeds persistedUserTurns
// without an independent O(n) rescan (#2089). Called outside the lock:
// historyMu must not nest inside it. The dead-process branch prefers EventEntries() over persistedHistory because it
// includes live events accumulated since the JSONL snapshot was loaded.
func collectPreviousHistory(oldSess *ManagedSession, oldPrevIDs []string, resumeID string) ([]clievent.EventEntry, []string, int64) {
	if oldSess == nil {
		return nil, nil, 0
	}

	// p.EventEntries() must be invoked WITHOUT holding historyMu: it takes
	// eventLog.mu internally, and a historyMu → eventLog.mu order would
	// deadlock against any sink calling back into the session. So: snapshot
	// the process pointer + persistedHistory under RLock, release, then read
	// entries (the old Process keeps its eventLog alive until GC).
	var entries []clievent.EventEntry
	// userTurns == -1 signals "unknown"; the dead-process branch counts once.
	userTurns := int64(-1)
	oldSess.historyMu.RLock()
	p := oldSess.loadProcess()
	var persistedSnapshot []clievent.EventEntry
	if (p == nil || p.Alive()) && len(oldSess.persistedHistory) > 0 {
		persistedSnapshot = make([]clievent.EventEntry, len(oldSess.persistedHistory))
		copy(persistedSnapshot, oldSess.persistedHistory)
		userTurns = oldSess.persistedUserTurns.Load()
	}
	oldSess.historyMu.RUnlock()

	if p != nil && !p.Alive() {
		entries = p.EventEntries()
	} else {
		entries = persistedSnapshot
	}

	if userTurns < 0 {
		userTurns = countUserTurns(entries)
	}
	return entries, respawnChain(oldPrevIDs, oldSess.getSessionID(), resumeID), userTurns
}

// respawnChain is the session-ID chain a respawn carries: oldPrevIDs plus
// oldID when it differs from resumeID (a new CLI session replaces the old
// one, not a same-ID resume), capped to the most recent maxPrevSessionIDs to
// bound sessions.json size and JSONL load time. Never aliases oldPrevIDs.
func respawnChain(oldPrevIDs []string, oldID, resumeID string) []string {
	prevIDs := slices.Clone(oldPrevIDs)
	if oldID != "" && oldID != resumeID {
		prevIDs = append(prevIDs, oldID)
	}
	if len(prevIDs) > maxPrevSessionIDs {
		prevIDs = prevIDs[len(prevIDs)-maxPrevSessionIDs:]
	}
	return prevIDs
}

// countUserTurns returns the number of Type=="user" entries in entries; the
// single definition of "what counts as a user turn".
func countUserTurns(entries []clievent.EventEntry) int64 {
	var n int64
	for i := range entries {
		if entries[i].Type == clievent.KindUser {
			n++
		}
	}
	return n
}
