package session

import (
	"log/slog"
	"slices"
)

// RenameSession moves a session entry from oldKey to newKey, preserving the
// running process, sessionID, history, and totalCost (scratch promote flow).
// Returns false when oldKey == newKey, oldKey does not exist, newKey already
// exists (a collision would drop an active session), or newKey fails
// session-key validation.
//
// The caller must ensure no Send is in flight for oldKey. The onSessionID
// closure captures newKey by value, so chaining a second rename on the same
// session would write a stale key into idToKey; rebuild the closure if a
// future caller needs that.
func (r *Router) RenameSession(oldKey, newKey string) bool {
	if oldKey == newKey {
		return false
	}
	if err := ValidateSessionKey(newKey); err != nil {
		slog.Warn("rename session: invalid new key", "err", err)
		return false
	}
	renamed := false
	r.ss.Update(func(tx sessTx) {
		old, ok := tx.Lookup(oldKey)
		if !ok {
			return
		}
		if _, collision := tx.Lookup(newKey); collision {
			return
		}

		// Session key is immutable on ManagedSession (parseKeyParts caches via
		// sync.Once), so a fresh struct is the only safe way to change it. Clone
		// prevSessionIDs/persistedHistory: an in-place append on old (later spawn
		// or the async history-load goroutine) would write into a backing array
		// fresh shares. History + user-turn count snapshot together under historyMu.
		old.historyMu.RLock()
		freshHistory := slices.Clone(old.persistedHistory)
		oldUserTurns := old.persistedUserTurns.Load()
		old.historyMu.RUnlock()
		fresh := &ManagedSession{
			key:              newKey,
			persistedHistory: freshHistory,
			prevSessionIDs:   slices.Clone(old.prevSessionIDs),
			exempt:           old.exempt,
			runStore:         r.runs.runs,
			costAcct:         r.runs.cost,
			onSessionID: func(id string) {
				r.ss.Update(func(tx sessTx) {
					r.kid.Track(id)
					tx.SetID(id, newKey)
				})
			},
		}
		// Seed persistedUserTurns so snapshot().MessageCount is correct before
		// any new turns arrive.
		if len(freshHistory) > 0 {
			fresh.persistedUserTurns.Store(oldUserTurns)
		}
		storeTotalCost(&fresh.totalCost, loadTotalCost(&old.totalCost))
		fresh.setWorkspace(old.Workspace())
		// Atomic fields: plain Load/Store round-trips are race-safe; the table lock blocks
		// all concurrent writers except the Send hot path (lastPrompt /
		// lastActivity), which are idempotent on copy.
		fresh.SetBackend(old.Backend())
		fresh.SetCLIName(old.CLIName())
		fresh.SetCLIVersion(old.CLIVersion())
		fresh.SetUserLabel(old.UserLabel())
		fresh.setLabelOrigin(old.LabelOrigin()) // label+origin travel as one unit
		// Tuning overrides follow the conversation: dropping them would make the
		// next respawn/drift check flip the session to default.
		fresh.SetTuningModel(old.TuningModel())
		fresh.SetTuningEffort(old.TuningEffort())
		fresh.setCodeChanges(old.CodeChanges())
		if dr := loadAtomicString(&old.deathReason); dr != "" {
			storeAtomicString(&fresh.deathReason, dr)
		}
		fresh.lastActive.Store(old.lastActive.Load())
		// Carry the creation timestamp so the renamed row keeps its sidebar position.
		if oldCreatedAt := old.createdAt.Load(); oldCreatedAt != 0 {
			fresh.createdAt.Store(oldCreatedAt)
		} else {
			fresh.initCreatedAtIfUnset()
		}
		// storeAtomicString allocates a fresh *string per write rather than
		// sharing old's pointer, matching the codebase-wide convention.
		if lp := loadAtomicString(&old.lastPrompt); lp != "" {
			storeAtomicString(&fresh.lastPrompt, lp)
		}
		if la := loadAtomicString(&old.lastActivity); la != "" {
			storeAtomicString(&fresh.lastActivity, la)
		}
		fresh.setSessionID(old.getSessionID())
		fresh.gapFill.Store(old.gapFillCell())

		// Move the process pointer; old becomes an orphan with process=nil so a
		// stale Send fails cleanly. The proc's EventLog already holds the entries
		// matching fresh.persistedHistory, so persistedSeededLen must mirror its
		// length (adoptProcessAlreadySeeded does that under historyMu) and a later
		// InjectHistory forwards only newly-arrived tail.
		proc := old.loadProcess()
		if proc != nil {
			fresh.adoptProcessAlreadySeeded(proc)
		}
		old.storeProcess(nil)
		// Rename keeps the SAME live process, so the delta baseline carries over
		// (a reset would double-count the next turn). Copied after the move, so a
		// CLI-started turn's reading is in the copy or dropped by old (accountCost).
		copyCostBaseline(fresh, old)
		if proc != nil {
			bookUnownedResults(fresh, proc)
			bookCodeChanges(fresh, proc, func() { r.ss.Update(markChanged); r.notifyChange() })
			bookProcessEnd(fresh, proc, r.hist.claudeDir)
		}

		// Rebind the history source (the old Source reads the orphaned struct);
		// oldKey's map entry and index slot are removed next so the rename is
		// atomic under the table lock.
		r.publishSession(tx, newKey, fresh, false)
		tx.Delete(oldKey)
		tx.SetID(fresh.getSessionID(), newKey)
		tx.Ext().picks.rename(oldKey, newKey)
		tx.MarkChanged()
		renamed = true
	})
	if !renamed {
		return false
	}

	slog.Info("session renamed", "old", oldKey, "new", newKey)
	r.notifyChange()
	return true
}
