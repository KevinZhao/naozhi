package session

import (
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// autoChainRetiredOrigins is the set of prev_session_origins labels written by
// the now-retired auto-workspace-chain feature. Any chain segment carrying one
// of these origins was machine-guessed by "same workspace dir + time window"
// (docs/rfc/auto-workspace-chain.md) rather than a real session-ID rotation,
// and is stripped by retireAutoChainOnce at startup.
var autoChainRetiredOrigins = map[string]bool{
	"auto-spawn":    true,
	"auto-backfill": true,
}

// retireAutoChainOnce strips auto-spawn / auto-backfill segments from every
// session's prev_session_ids chain at startup, replacing the old
// runAutoChainBackfillOnce. It is the data-cleanup half of retiring the
// auto-workspace-chain feature (RFC docs/rfc/project-stable-session-key.md
// §9.2): the feature produced semantically-wrong chains (e.g. a one-off
// "什么药治拉肚子" conversation chained onto a coding session merely because
// both lived under /home/ec2-user/workspace), and this removes that pollution
// while preserving the real rotation chain (origin manual / resume / empty).
//
// Per session, the kept indices are those whose origin is NOT an auto-* label;
// RebuildChainFiltered then rewrites prevSessionIDs + prevSessionOrigins
// atomically under one historyMu hold so no reader observes a misaligned pair.
//
// Idempotent: after a first run no auto-* origins remain, so subsequent
// startups strip nothing and skip the dirty/store bump.
//
// CALLER CONTRACT: invoked from NewRouter BEFORE the background history
// loaders launch (same slot the old backfill occupied) and while the router
// is single-threaded, so it does not take r.mu — it snapshots r.sessions
// under r.mu briefly, then mutates each session via historyMu only.
func (r *Router) retireAutoChainOnce() {
	startedAt := time.Now()

	r.mu.Lock()
	candidates := make([]*ManagedSession, 0, len(r.sessions))
	for _, s := range r.sessions {
		candidates = append(candidates, s)
	}
	r.mu.Unlock()

	retired := 0
	for _, s := range candidates {
		origins := s.SnapshotPrevSessionOrigins()
		if len(origins) == 0 {
			continue
		}
		keep := make([]bool, len(origins))
		hasAuto := false
		for i, o := range origins {
			if autoChainRetiredOrigins[o] {
				keep[i] = false
				hasAuto = true
			} else {
				keep[i] = true
			}
		}
		if !hasAuto {
			continue
		}
		removed := s.RebuildChainFiltered(keep)
		if removed == 0 {
			// keepMask length must match the live chain; a concurrent
			// mutation (none expected at startup) or already-clean chain
			// means nothing to do.
			slog.Warn("auto-chain retire: RebuildChainFiltered returned 0 with pending auto-chain origins; possible misaligned keep mask", "key", s.key)
			continue
		}
		retired++
		metrics.AutoChainRetiredOnStartup.Add(1)
		slog.Info("auto-chain retired",
			"key", s.key,
			"workspace", s.Workspace(),
			"removed", removed,
			"kept", len(s.SnapshotPrevSessionIDs()))
	}

	if retired > 0 {
		r.mu.Lock()
		r.storeDirty = true
		r.storeGen.Add(1)
		r.mu.Unlock()
	}

	slog.Info("auto-chain retire complete",
		"sessions_cleaned", retired,
		"duration_ms", time.Since(startedAt).Milliseconds())
}
