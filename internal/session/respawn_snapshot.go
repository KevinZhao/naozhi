package session

import (
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
	// Overrides are snapshotted HERE, in the same transaction, from the same
	// object as history/cost/createdAt. installFreshSession must not re-read
	// the key's entry: it may be swapped or removed during the history copy
	// outside the transaction, pairing one session's history with another's
	// tuning.
	ov := sessionOverrides{
		tuningModel:  old.TuningModel(),
		tuningEffort: old.TuningEffort(),
		userLabel:    old.UserLabel(),
		labelOrigin:  old.LabelOrigin(),
	}
	return oldPrevIDs, oldTotalCost, oldCostSpent, oldCreatedAt, ov
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
}

func snapshotRespawn(v sessView, old *ManagedSession) respawnSnapshot {
	var snap respawnSnapshot
	snap.prevIDs, snap.cost, snap.costSpent, snap.createdAt, snap.overrides = snapshotOldSession(v, old)
	if old != nil {
		snap.sid = old.getSessionID()
		snap.spent = old.CostTotals()
	}
	return snap
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

	// Append the old session ID to the chain only when it differs from
	// resumeID (a new CLI session replaces the old one, not a same-ID resume).
	var prevIDs []string
	if oldID := oldSess.getSessionID(); oldID != "" && oldID != resumeID {
		prevIDs = make([]string, len(oldPrevIDs), len(oldPrevIDs)+1)
		copy(prevIDs, oldPrevIDs)
		prevIDs = append(prevIDs, oldID)
	} else {
		prevIDs = oldPrevIDs
	}
	// Cap the chain to bound sessions.json size and JSONL load time; the
	// retained tail carries the most recent context.
	if len(prevIDs) > maxPrevSessionIDs {
		prevIDs = prevIDs[len(prevIDs)-maxPrevSessionIDs:]
	}
	if userTurns < 0 {
		userTurns = countUserTurns(entries)
	}
	return entries, prevIDs, userTurns
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
