package session

import (
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/metrics"
	"github.com/naozhi/naozhi/internal/tuningspec"
)

// restoreStore publishes the sessions loaded from the store.
func (r *Router) restoreStore(tx sessTx, restored map[string]*storeEntry) {
	for key, entry := range restored {
		// SECURITY: reject sys: entries even though saveStore already skips
		// them (RFC v2.1 §3.4). A sys: entry on disk means a tampered
		// sessions.json; resurrecting it would let an attacker pre-seed a
		// ManagedSession with chosen label_origin etc. Daemons re-register
		// stubs at startup, so dropping the persisted copy is safe.
		if IsSysKey(key) {
			slog.Warn("session store: dropping unexpected sys: entry",
				"key", key,
				"hint", "sys entries should never persist; possible sessions.json tampering")
			continue
		}
		r.restoreSessionFromEntry(tx, key, entry)
	}
}

// restoreSessionFromEntry rebuilds a single persisted ManagedSession from its
// on-disk storeEntry and publishes it into the router's maps + indexes. The
// caller owns the IsSysKey skip guard and the loadStore range.
func (r *Router) restoreSessionFromEntry(tx sessTx, key string, entry *storeEntry) {
	// Resolve the wrapper that owned this session's backend so the snapshot
	// carries the correct CLI identity after a pure restore (no shim reconnect).
	// Pre-multi-backend entries have empty Backend → router default.
	restoreWrapper, restoreBackendID := r.backends.wrapperFor(entry.Backend)
	cliName, cliVersion := r.backends.CLIName(), r.backends.CLIVersion()
	if restoreWrapper != nil {
		cliName = restoreWrapper.CLIName
		cliVersion = restoreWrapper.CLIVersion
	}
	s := &ManagedSession{
		key:                key,
		prevSessionIDs:     entry.PrevSessionIDs,
		prevSessionOrigins: entry.PrevSessionOrigins,
		exempt:             isExemptKey(key),
		runStore:           r.runs.runs,
		costAcct:           r.runs.cost,
	}
	storeTotalCost(&s.totalCost, entry.TotalCost)
	// Legacy stores (predating cost_spent) seed costSpent from TotalCost so the
	// established total keeps showing; lastCumulativeCost stays 0 there, which
	// is safe — the first post-upgrade raw cumulative is a fresh delta against 0.
	if entry.CostSpent > 0 {
		storeTotalCost(&s.costSpent, entry.CostSpent)
	} else {
		storeTotalCost(&s.costSpent, entry.TotalCost)
	}
	storeTotalCost(&s.lastCumulativeCost, entry.LastCumulativeCost)
	// The store keeps only the USD baseline: a shim-reconnected CLI keeps
	// counting from there, but its per-model rows are unknown until the next
	// result, so the first turn reports no model drill-down.
	s.lastCumulative = costledger.Cumulative{USD: entry.LastCumulativeCost}
	s.modelsBaselineUnknown = entry.LastCumulativeCost > 0
	s.setWorkspace(entry.Workspace)
	s.SetBackend(restoreBackendID)
	// Restore the recorded access profile so a resume relocks the same auth
	// chain rather than re-resolving from a since-changed project binding (RFC §7).
	s.SetAccessProfile(entry.AccessProfile)
	s.SetCLIName(cliName)
	s.SetCLIVersion(cliVersion)
	if entry.UserLabel != "" {
		s.SetUserLabel(entry.UserLabel)
	}
	// Empty LabelOrigin in pre-v2.1 stores means "user" to daemons (RFC §7.3),
	// so no default is synthesised here.
	if entry.LabelOrigin != "" {
		s.setLabelOrigin(entry.LabelOrigin)
	}
	// Seed model from the store so the dashboard renders it on post-restart
	// reattach, before the first new turn re-emits system/init.
	if entry.Model != "" {
		s.SetModel(entry.Model)
	}
	// SECURITY: these values feed --model/--effort argv on the next spawn and
	// sessions.json is hand-editable, so re-validate with the same validators
	// as SetSessionTuning / config; drop-with-warn so one corrupt entry cannot
	// block the whole store load (docs/rfc/dashboard-model-effort-control.md §4.3).
	if entry.TuningModel != "" {
		if err := tuningspec.ValidateModel("stored tuning_model", entry.TuningModel); err != nil {
			slog.Warn("dropping invalid persisted tuning_model", "key", entry.Key, "err", err)
		} else {
			s.SetTuningModel(entry.TuningModel)
		}
	}
	if entry.TuningEffort != "" {
		if err := tuningspec.ValidateEffort("stored tuning_effort", entry.TuningEffort); err != nil {
			slog.Warn("dropping invalid persisted tuning_effort", "key", entry.Key, "err", err)
		} else {
			s.SetTuningEffort(entry.TuningEffort)
		}
	}
	s.setSessionID(entry.SessionID)
	if entry.LastActive != 0 {
		s.lastActive.Store(entry.LastActive)
	}
	// Sidebar order anchor: prefer persisted CreatedAt, fall back to LastActive
	// for pre-feature stores; if both are zero stamp now so the entry still
	// gets a stable comparator key.
	switch {
	case entry.CreatedAt != 0:
		s.createdAt.Store(entry.CreatedAt)
	case entry.LastActive != 0:
		s.createdAt.Store(entry.LastActive)
	default:
		s.initCreatedAtIfUnset()
	}
	// publishSession funnels attachHistorySource + map insert + index
	// update so the triple-index invariant is a property of the publish step.
	r.publishSession(tx, key, s, false)
	r.kid.Track(entry.SessionID)
	tx.SetID(entry.SessionID, key)
}

// startBackgroundHistoryLoaders launches the tier 1 / tier 2 history-load
// goroutines for every restored session. Tier 1 (naozhilog, when
// r.hist.persister is set) preserves Images / AskQuestion / agent-team
// linkage Claude JSONL cannot represent. Tier 2 (Claude CLI JSONL) skips
// sessions tier 1 filled; shim-managed sessions wait shimReconnectGraceDelay
// so ReconnectShims can inject first, then backfill only if still empty. One
// historyLoadSem bounds total history I/O across both tiers. Both finish
// BEFORE the process's PersistSink is installed, so replayed entries are
// tagged replayPhase=true and dropped. Tier 1 reads persist_gap fill after it
// injects (fillPersistGaps). NewRouter-only.
func (r *Router) startBackgroundHistoryLoaders() {
	historyLoadSem := make(chan struct{}, historyLoadConcurrency)
	var sessions []*ManagedSession
	r.ss.View(func(v sessView) {
		sessions = make([]*ManagedSession, 0, v.Len())
		for _, s := range v.All() {
			sessions = append(sessions, s)
		}
	})

	// Tier 1: naozhilog (in-process per-session log).
	if r.hist.persister != nil {
		sem := historyLoadSem
		for _, s := range sessions {
			r.hist.wg.Add(1)
			go func() {
				defer r.hist.wg.Done()
				select {
				case sem <- struct{}{}:
				case <-r.hist.ctx.Done():
					return
				}
				defer func() { <-sem }()
				src := newEventLogLocalSource(r.hist.eventLogDir, s.key)
				all, err := src.LoadLatest(r.hist.ctx, 2*maxPersistedHistory)
				if err != nil || len(all) == 0 {
					return
				}
				// The extra look-back lets a gap record just below the cut count.
				entries := all[max(0, len(all)-maxPersistedHistory):]
				// InjectHistoryIfEmpty atomically guards against a concurrent
				// ReconnectShims / Tier 2 loader having already filled the
				// session; a separate check-then-inject would double-inject (#1812).
				if !s.InjectHistoryIfEmpty(entries) {
					return
				}
				slog.Info("loaded session history from naozhi event log",
					"key", s.key, "entries", len(entries))
				r.notifyChange()
				s.fillPersistGaps(r.hist.ctx, all, entries[0].Time)
			}()
		}
	}

	// Tier 2: Claude CLI JSONL.
	if r.hist.claudeDir == "" {
		return
	}
	shimKeys := r.backends.shimManagedKeys()
	sem := historyLoadSem
	for _, s := range sessions {
		if s.getSessionID() == "" {
			continue
		}
		deferred := shimKeys[s.key]
		r.hist.wg.Add(1)
		go func() {
			defer r.hist.wg.Done()
			if deferred {
				// Wait for ReconnectShims' first pass; the history ctx cancel aborts.
				// NewTimer + Stop (not time.After) so a fast shutdown does not
				// leak a timer per goroutine for the whole grace window.
				graceTimer := time.NewTimer(shimReconnectGraceDelay)
				select {
				case <-graceTimer.C:
					// Fired — no Stop needed, channel already drained.
				case <-r.hist.ctx.Done():
					if !graceTimer.Stop() {
						<-graceTimer.C
					}
					return
				}
				if s.hasInjectedHistory() {
					return
				}
				// Counter sits AFTER the hasInjectedHistory short-circuit so
				// only the fallback branch (short-lived-shim race) increments.
				metrics.ShimReconnectGraceBackfillTotal.Add(1)
				slog.Info("shim-managed session missing history after reconnect grace, falling back to JSONL load",
					"key", s.key)
			}
			select {
			case sem <- struct{}{}:
			case <-r.hist.ctx.Done():
				return
			}
			defer func() { <-sem }()

			// Skip when tier 1 already filled the session — otherwise a deploy
			// with both sources would double-inject the first ~500 entries.
			if s.hasInjectedHistory() {
				return
			}

			// Ordered chain (prev + current) via SnapshotChainIDs() — a clone
			// under historyMu — because this goroutine holds neither the table lock nor
			// historyMu while a concurrent cron stub refresh may reassign the
			// slice header under the table lock (#2055). LoadHistoryChainTail walks
			// newest→oldest and stops at maxPersistedHistory entries.
			ids := s.SnapshotChainIDs()

			allEntries := r.hist.loader.LoadHistoryChainTail(
				r.hist.ctx, r.hist.claudeDir, ids, s.Workspace(), maxPersistedHistory,
			)
			if len(allEntries) == 0 {
				return
			}
			// The hasInjectedHistory() checks above only skip the expensive
			// read; the inject itself must be atomic, so InjectHistoryIfEmpty
			// does the final "still empty?" check and the append under one
			// historyMu hold (#1812).
			if !s.InjectHistoryIfEmpty(allEntries) {
				return
			}
			slog.Info("loaded session history on startup", "key", s.key, "entries", len(allEntries), "chain", len(ids), "deferred", deferred)
			r.notifyChange()
		}()
	}
}
