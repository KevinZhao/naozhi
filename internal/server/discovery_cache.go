package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/project"
)

// discoveryCache periodically scans local Claude CLI sessions and caches
// the results so that handleAPIDiscovered never blocks on disk I/O.
type discoveryCache struct {
	mu       sync.RWMutex
	sessions []discovery.DiscoveredSession

	claudeDir  string
	getExclude func() (pids map[int]bool, sessionIDs map[string]bool, cwds map[string]bool)
	projectMgr *project.Manager
}

func newDiscoveryCache(claudeDir string, getExclude func() (map[int]bool, map[string]bool, map[string]bool), projectMgr *project.Manager) *discoveryCache {
	return &discoveryCache{
		claudeDir:  claudeDir,
		getExclude: getExclude,
		projectMgr: projectMgr,
	}
}

// startLoop begins periodic scanning every 10 seconds.
func (dc *discoveryCache) startLoop(ctx context.Context) {
	go dc.refresh()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				dc.refresh()
			}
		}
	}()
}

// refresh runs a full discovery scan and updates the cached snapshot.
func (dc *discoveryCache) refresh() {
	if dc.claudeDir == "" {
		return
	}

	pids, sids, cwds := dc.getExclude()
	sessions, err := discovery.Scan(dc.claudeDir, pids, sids, cwds)
	if err != nil {
		slog.Warn("discovery cache refresh", "err", err)
		sessions = nil
	}
	if sessions == nil {
		sessions = []discovery.DiscoveredSession{}
	}

	// Resolve CWD -> project name
	if dc.projectMgr != nil && len(sessions) > 0 {
		cwdList := make([]string, len(sessions))
		for i, d := range sessions {
			cwdList[i] = d.CWD
		}
		cwdMap := dc.projectMgr.ResolveWorkspaces(cwdList)
		for i := range sessions {
			sessions[i].Project = cwdMap[sessions[i].CWD]
		}
	}

	dc.mu.Lock()
	dc.sessions = sessions
	dc.mu.Unlock()
}

// snapshot returns a copy of the cached discovered sessions.
func (dc *discoveryCache) snapshot() []discovery.DiscoveredSession {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	out := make([]discovery.DiscoveredSession, len(dc.sessions))
	copy(out, dc.sessions)
	return out
}
