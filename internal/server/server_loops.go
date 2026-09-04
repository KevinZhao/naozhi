// Background lifecycle loops: retired-store flusher and project-scan loop.
// The retired-store interval consts live in server.go next to RetiredStore wiring.
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
// current — i.e. the projects deleted between two consecutive scans. Pure;
// the caller applies side effects (orphaned-planner removal, WS broadcast).
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
				s.projectScanTick()
			}
		}
	}()
}

// projectScanTick runs one rescan of the projects root and applies the
// router/hub side effects for any added or removed project.
func (s *Server) projectScanTick() {
	oldNames := s.projectMgr.ProjectNames()
	if err := s.projectMgr.Scan(); err != nil {
		s.log().Warn("project rescan", "err", err)
		return
	}
	newNames := s.projectMgr.ProjectNames()

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
		// The dashboard gates its re-render on stats.version, so bump before
		// broadcasting or the sidebar never picks up the change.
		if s.router != nil {
			s.router.BumpVersion()
		}
		if s.hub != nil {
			s.hub.BroadcastSessionsUpdate()
		}
	}
}
