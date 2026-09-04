package session

import (
	"log/slog"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// autoChainRetiredOrigins is the set of prev_session_origins labels that mark
// a chain segment as machine-guessed ("same workspace dir + time window",
// docs/rfc/auto-workspace-chain.md) rather than a real session-ID rotation;
// retireAutoChainOnce strips them at startup.
var autoChainRetiredOrigins = map[string]bool{
	"auto-spawn":    true,
	"auto-backfill": true,
}

// retireAutoChainOnce strips auto-* segments from every session's
// prev_session_ids chain at startup while preserving the real rotation chain
// (origin manual / resume / empty); see docs/rfc/project-stable-session-key.md
// §9.2. RebuildChainFiltered rewrites prevSessionIDs + prevSessionOrigins
// atomically under one historyMu hold so no reader observes a misaligned pair.
// Idempotent: once clean, later startups strip nothing and skip the dirty bump.
//
// CALLER CONTRACT: invoked from NewRouter BEFORE the background history loaders
// launch, while the router is single-threaded — it snapshots r.ss.sessions
// under r.mu briefly, then mutates each session via historyMu only.
func (r *Router) retireAutoChainOnce() {
	startedAt := time.Now()

	r.mu.Lock()
	candidates := make([]*ManagedSession, 0, len(r.ss.sessions))
	for _, s := range r.ss.sessions {
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
			// keepMask length must match the live chain; a mismatch means a
			// concurrent mutation (none expected at startup).
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
		r.ss.dirty = true
		r.ss.gen.Add(1)
		r.mu.Unlock()
	}

	slog.Info("auto-chain retire complete",
		"sessions_cleaned", retired,
		"duration_ms", time.Since(startedAt).Milliseconds())
}
