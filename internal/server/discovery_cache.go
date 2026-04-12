package server

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
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

	// lastDirMtime is the last observed mtime of ~/.claude/sessions/.
	// When it hasn't changed and all cached PIDs are still alive,
	// we skip the expensive full Scan() call.
	lastDirMtime time.Time
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

// refresh runs a discovery scan and updates the cached snapshot.
// It short-circuits the expensive full Scan when the sessions directory
// hasn't changed and all previously discovered PIDs are still alive.
func (dc *discoveryCache) refresh() {
	if dc.claudeDir == "" {
		return
	}

	if dc.tryShortCircuit() {
		return
	}

	// Capture dir mtime BEFORE scan so that any files created during the scan
	// will have a newer mtime, causing the next tryShortCircuit to miss and
	// trigger a full scan. This avoids a TOCTOU where a newly created session
	// file is missed permanently.
	sessDir := filepath.Join(dc.claudeDir, "sessions")
	var newDirMtime time.Time
	if info, err := os.Stat(sessDir); err == nil {
		newDirMtime = info.ModTime()
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
	dc.lastDirMtime = newDirMtime
	dc.mu.Unlock()
}

// tryShortCircuit returns true if the full scan can be skipped.
// Conditions: the sessions directory mtime is unchanged AND every
// previously discovered PID is still alive (kill(pid, 0)).
func (dc *discoveryCache) tryShortCircuit() bool {
	dc.mu.RLock()
	lastMtime := dc.lastDirMtime
	cached := dc.sessions
	dc.mu.RUnlock()

	if lastMtime.IsZero() {
		return false // first run, must do full scan
	}

	info, err := os.Stat(filepath.Join(dc.claudeDir, "sessions"))
	if err != nil {
		return false // directory gone or inaccessible, do full scan
	}
	if !info.ModTime().Equal(lastMtime) {
		return false // files added or removed, do full scan
	}

	// Dir unchanged — verify all cached PIDs are still alive.
	for _, s := range cached {
		if s.PID > 0 && !pidAlive(s.PID) {
			return false // a process died, do full scan
		}
	}

	// Session list is stable (no new/removed processes), but dynamic fields
	// (lastActive, state, summary, lastPrompt) may have changed because the
	// CLI keeps writing to JSONL files.  Do a lightweight refresh that only
	// stats files and hits the mtime-based caches.
	if len(cached) > 0 {
		updated := make([]discovery.DiscoveredSession, len(cached))
		copy(updated, cached)
		discovery.RefreshDynamic(dc.claudeDir, updated)
		dc.mu.Lock()
		dc.sessions = updated
		dc.mu.Unlock()
	}

	return true
}

// pidAlive checks whether a process exists via kill(pid, 0).
// Returns true for EPERM (process alive but owned by another user).
func pidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// evictPID removes a specific PID from the cached snapshot immediately.
// Called after session takeover so the killed process doesn't reappear
// in the sidebar while the 10-second discovery cache is still stale.
func (dc *discoveryCache) evictPID(pid int) {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	filtered := dc.sessions[:0:0]
	for _, s := range dc.sessions {
		if s.PID != pid {
			filtered = append(filtered, s)
		}
	}
	dc.sessions = filtered
}

// snapshot returns a copy of the cached discovered sessions.
func (dc *discoveryCache) snapshot() []discovery.DiscoveredSession {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	out := make([]discovery.DiscoveredSession, len(dc.sessions))
	copy(out, dc.sessions)
	return out
}
