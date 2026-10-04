package session

import (
	"context"
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

// startBackgroundHistoryLoaders starts one history-load goroutine per restored
// session: the naozhi event log first, then the Claude JSONL only when the log
// has no rows for it (see injectRestoredHistory). A shim-managed session waits
// shimReconnectGraceDelay before its JSONL read so ReconnectShims can fill it
// first. One historyLoadSem bounds all history I/O and is not held during the
// grace wait. The loads finish BEFORE the process's PersistSink is installed,
// so replayed entries are tagged replayPhase=true and dropped. NewRouter-only.
func (r *Router) startBackgroundHistoryLoaders() {
	historyLoadSem := make(chan struct{}, historyLoadConcurrency)
	var sessions []*ManagedSession
	r.ss.View(func(v sessView) {
		sessions = make([]*ManagedSession, 0, v.Len())
		for _, s := range v.All() {
			sessions = append(sessions, s)
		}
	})
	var shimKeys map[string]bool
	if r.hist.claudeDir != "" {
		shimKeys = r.backends.shimManagedKeys()
	}
	for _, s := range sessions {
		jsonl := r.hist.claudeDir != "" && s.getSessionID() != ""
		if r.hist.persister == nil && !jsonl {
			continue
		}
		deferred := jsonl && shimKeys[s.key]
		r.hist.wg.Add(1)
		go func() {
			defer r.hist.wg.Done()
			if r.hist.loadStartupHistory(s, historyLoadSem, jsonl, deferred) {
				r.notifyChange()
			}
		}()
	}
}

// loadStartupHistory is one session's startup load: the event log under sem,
// then, if it had no rows and jsonl is set, the JSONL tail under sem again.
// Reports whether it injected.
func (h *HistoryIO) loadStartupHistory(s *ManagedSession, sem chan struct{}, jsonl, deferred bool) (injected bool) {
	ctx := h.ctx
	found := false
	if !withHistorySem(ctx, sem, func() {
		found, injected = h.injectEventLogHistory(ctx, s, restoreViaStartup)
	}) || found || !jsonl {
		return injected
	}
	if deferred {
		// NewTimer + Stop (not time.After) so a fast shutdown does not leak a
		// timer per goroutine for the whole grace window.
		graceTimer := time.NewTimer(shimReconnectGraceDelay)
		select {
		case <-graceTimer.C:
		case <-ctx.Done():
			graceTimer.Stop()
			return false
		}
		if s.hasInjectedHistory() {
			return false
		}
		// Counted after the short-circuit so only the fallback branch
		// (short-lived-shim race) increments.
		metrics.ShimReconnectGraceBackfillTotal.Add(1)
		slog.Info("shim-managed session missing history after reconnect grace, falling back to JSONL load",
			"key", s.key)
	}
	withHistorySem(ctx, sem, func() {
		if !s.hasInjectedHistory() {
			// SnapshotChainIDs clones under historyMu: a cron stub refresh may
			// reassign the slice header under the table lock (#2055).
			injected = h.injectJSONLHistory(ctx, s, s.SnapshotChainIDs(), restoreViaStartup)
		}
	})
	return injected
}

// withHistorySem runs fn holding a sem slot, or reports false when ctx is
// cancelled first.
func withHistorySem(ctx context.Context, sem chan struct{}, fn func()) bool {
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	defer func() { <-sem }()
	fn()
	return true
}

// Callers of injectRestoredHistory, logged as "via".
const (
	restoreViaStartup       = "startup"
	restoreViaShimReconnect = "shim_reconnect"
	restoreViaShimDrift     = "shim_drift"
)

// injectRestoredHistory fills an empty s with its restored history and
// reports whether this call's inject won. The naozhi event log comes first: it
// keeps images, AskQuestion and tool turns the Claude JSONL tail over ids
// cannot represent, and once it has rows the JSONL is never read. Every
// startup and shim inject goes through here and through InjectHistoryIfEmpty
// (#1812), so whichever caller wins injects the same view, and nothing is
// appended under the gap fill the event-log winner computed.
func (h *HistoryIO) injectRestoredHistory(ctx context.Context, s *ManagedSession, ids []string, via string) bool {
	if found, injected := h.injectEventLogHistory(ctx, s, via); found {
		return injected
	}
	return h.injectJSONLHistory(ctx, s, ids, via)
}

// injectEventLogHistory injects the event-log tail into an empty s, then reads
// the persist_gap fill for it. found reports the log had rows; the inject may
// still lose to another reader of the same log.
func (h *HistoryIO) injectEventLogHistory(ctx context.Context, s *ManagedSession, via string) (found, injected bool) {
	if h.persister == nil {
		return false, false
	}
	all, err := newEventLogLocalSource(h.eventLogDir, s.key).LoadLatest(ctx, 2*maxPersistedHistory)
	if err != nil || len(all) == 0 {
		return false, false
	}
	// The extra look-back lets a gap record just below the cut count.
	entries := all[max(0, len(all)-maxPersistedHistory):]
	if !s.InjectHistoryIfEmpty(entries) {
		return true, false
	}
	slog.Info("loaded session history from naozhi event log",
		"key", s.key, "entries", len(entries), "via", via)
	s.fillPersistGaps(ctx, all, entries[0].Time)
	return true, true
}

// injectJSONLHistory injects the Claude JSONL tail of the ids chain into an
// empty s. No-op without a claudeDir or ids.
func (h *HistoryIO) injectJSONLHistory(ctx context.Context, s *ManagedSession, ids []string, via string) bool {
	if h.claudeDir == "" || len(ids) == 0 {
		return false
	}
	entries := h.loader.LoadHistoryChainTail(ctx, h.claudeDir, ids, s.Workspace(), maxPersistedHistory)
	if len(entries) == 0 || !s.InjectHistoryIfEmpty(entries) {
		return false
	}
	slog.Info("loaded session history from Claude JSONL",
		"key", s.key, "entries", len(entries), "chain", len(ids), "via", via)
	return true
}
