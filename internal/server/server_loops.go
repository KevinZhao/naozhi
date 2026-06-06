// Background lifecycle loops extracted out of server.go as a Phase-3 physical
// split (ARCH1 / #387). server.go has carried the retired-store flusher and the
// project-scan loop alongside the constructor, the Start/Shutdown sequencer,
// and 200+ lines of warning consts; both loops are self-contained
// ctx-driven goroutine drivers with no routing or constructor coupling, so
// moving them here shrinks server.go toward the <800-line target the issue
// names with zero behaviour change (pure move). The retired-store interval
// consts stay in server.go next to RetiredStore wiring; this file references
// them as package-level identifiers.
package server

import (
	"context"
	"time"

	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// runRetiredStoreFlusher writes the retired-store to disk every
// retiredStoreFlushInterval and prunes stale entries every
// retiredStorePruneInterval. Stops on ctx.Done; the shutdown goroutine
// invokes a final FlushRetiredStore so the most recent retirement event
// survives a clean shutdown.
func (s *Server) runRetiredStoreFlusher(ctx context.Context) {
	flushTicker := time.NewTicker(retiredStoreFlushInterval)
	defer flushTicker.Stop()
	pruneTicker := time.NewTicker(retiredStorePruneInterval)
	defer pruneTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-flushTicker.C:
			s.sessionH.FlushRetiredStore()
		case <-pruneTicker.C:
			cutoffMs := time.Now().Add(-retiredStorePruneCutoff).UnixMilli()
			s.sessionH.PruneRetiredStore(cutoffMs)
		}
	}
}

// removedProjectNames returns the project names present in old but absent in
// current — i.e. the projects deleted between two consecutive scans. It is the
// pure decision rule extracted from startProjectScanLoop (ARCH-SVR-2 / #460):
// no Router, Hub, or logging is touched, so it can be exercised directly in a
// unit test rather than only through the 60s ticker goroutine. The caller is
// responsible for the side effects (orphaned-planner removal, WS broadcast),
// which remain the server adapter's concern.
func removedProjectNames(old, current map[string]struct{}) []string {
	if len(old) == 0 {
		return nil
	}
	var removed []string
	for name := range old {
		if _, ok := current[name]; !ok {
			removed = append(removed, name)
		}
	}
	return removed
}

// startProjectScanLoop periodically rescans the projects root for added or
// removed subdirectories and cleans up orphaned planner sessions for removed
// projects.
func (s *Server) startProjectScanLoop(ctx context.Context) {
	if s.projectMgr == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(session.ProjectScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				oldNames := s.projectMgr.ProjectNames()
				if err := s.projectMgr.Scan(); err != nil {
					s.log().Warn("project rescan", "err", err)
					continue
				}
				newNames := s.projectMgr.ProjectNames()

				// Detect removed projects and clean up orphaned planner
				// sessions. The set-diff is the pure business rule (which
				// projects disappeared) and lives in removedProjectNames so
				// it is unit-testable without a Router/Hub — the first
				// concrete slice of ARCH-SVR-2 (#460) "sink business logic
				// out of server". The router/hub side effects below stay in
				// the server layer because they are the HTTP/WS adapter's job.
				removed := removedProjectNames(oldNames, newNames)
				changed := len(oldNames) != len(newNames)
				for _, name := range removed {
					changed = true
					plannerKey := project.PlannerKeyFor(name)
					if s.router.Remove(plannerKey) {
						s.log().Info("removed orphaned planner", "project", name)
					}
				}
				if changed {
					s.log().Info("project list changed", "count", len(newNames))
					if s.hub != nil {
						s.hub.BroadcastSessionsUpdate()
					}
				}
			}
		}
	}()
}
