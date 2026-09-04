package server

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/project"
)

// discoveryCache periodically scans local Claude CLI sessions and caches
// the results so that handleAPIDiscovered never blocks on disk I/O.
type discoveryCache struct {
	mu       sync.RWMutex
	sessions []discovery.DiscoveredSession

	// refreshMu single-flights refresh() across the two startLoop goroutines
	// (initial one-shot + ticker); it is also what makes refreshScratch safe
	// as shared state (#1700).
	refreshMu sync.Mutex

	// refreshScratch is owned by the refresh path (guarded by refreshMu, never
	// published to readers); tryShortCircuit runs RefreshDynamic in it and
	// publishes a fresh copy only when something changed.
	refreshScratch []discovery.DiscoveredSession

	wg sync.WaitGroup // tracks the initial refresh goroutine started by startLoop

	claudeDir  string
	getExclude func() (pids map[int]bool, sessionIDs map[string]bool, cwds map[string]bool)
	projectMgr *project.Manager

	// scanFn is the discovery scan entry point; nil means discovery.Scan.
	// Tests inject a failing scanner to exercise the error path.
	scanFn func(claudeDir string, pids map[int]bool, sids map[string]bool, cwds map[string]bool) ([]discovery.DiscoveredSession, error)

	// lastDirMtime is the last observed mtime of ~/.claude/sessions/.
	// When it hasn't changed and all cached PIDs are still alive,
	// we skip the expensive full Scan() call.
	lastDirMtime time.Time

	// evictedPIDs tracks PIDs removed by evictPID with their eviction time.
	// refresh() filters these out for a grace period so a full scan during
	// the WaitAndCleanup window doesn't re-add a session being taken over.
	evictedPIDs map[int]time.Time
}

func newDiscoveryCache(claudeDir string, getExclude func() (map[int]bool, map[string]bool, map[string]bool), projectMgr *project.Manager) *discoveryCache {
	return &discoveryCache{
		claudeDir:   claudeDir,
		getExclude:  getExclude,
		projectMgr:  projectMgr,
		evictedPIDs: make(map[int]time.Time),
	}
}

// startLoop begins periodic scanning every 10 seconds. Both the initial
// refresh and the ticker loop are tracked by dc.wg so Server.Shutdown can
// Wait() on them after cancelling ctx and not race projectMgr cleanup.
func (dc *discoveryCache) startLoop(ctx context.Context) {
	dc.wg.Add(1)
	go func() {
		defer dc.wg.Done()
		// Scan doesn't take ctx, so this pre-check is the only pre-emption
		// point; an in-flight Scan still runs to completion.
		select {
		case <-ctx.Done():
			return
		default:
		}
		dc.refresh()
	}()
	dc.wg.Add(1)
	go func() {
		defer dc.wg.Done()
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

// Wait blocks until all goroutines started by startLoop have exited.
// Call this during server shutdown after cancelling the context.
func (dc *discoveryCache) Wait() {
	dc.wg.Wait()
}

// refresh runs a discovery scan and updates the cached snapshot.
// It short-circuits the expensive full Scan when the sessions directory
// hasn't changed and all previously discovered PIDs are still alive.
func (dc *discoveryCache) refresh() {
	if dc.claudeDir == "" {
		return
	}

	dc.refreshMu.Lock()
	defer dc.refreshMu.Unlock()

	if dc.tryShortCircuit() {
		return
	}

	// Capture dir mtime BEFORE scan so files created mid-scan make the next
	// tryShortCircuit miss instead of being missed permanently.
	sessDir := filepath.Join(dc.claudeDir, "sessions")
	var newDirMtime time.Time
	if info, err := os.Stat(sessDir); err == nil {
		newDirMtime = info.ModTime()
	}

	pids, sids, cwds := dc.getExclude()
	scan := dc.scanFn
	if scan == nil {
		scan = discovery.Scan
	}
	sessions, err := scan(dc.claudeDir, pids, sids, cwds)
	if err != nil {
		// Keep the previous snapshot and leave lastDirMtime untouched so the
		// next tick retries the full scan instead of short-circuiting on an
		// empty list.
		slog.Warn("discovery cache refresh", "err", err)
		return
	}
	if sessions == nil {
		sessions = []discovery.DiscoveredSession{}
	}

	// Resolve CWD -> project name (outside lock — no shared state)
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

	// Filter recently-evicted PIDs against a snapshot of evictedPIDs taken
	// under a brief RLock so snapshot() readers are not blocked for the O(N)
	// pass; the publish re-checks PIDs evicted after the snapshot so a
	// just-evicted session cannot be resurrected (#2123).
	now := time.Now()
	const evictGrace = 60 * time.Second

	dc.mu.RLock()
	var evictedSnap map[int]time.Time
	if len(dc.evictedPIDs) > 0 {
		evictedSnap = make(map[int]time.Time, len(dc.evictedPIDs))
		for pid, at := range dc.evictedPIDs {
			evictedSnap[pid] = at
		}
	}
	dc.mu.RUnlock()

	if len(evictedSnap) > 0 {
		for pid, evictedAt := range evictedSnap {
			if now.Sub(evictedAt) > evictGrace {
				delete(evictedSnap, pid)
			}
		}
	}
	if len(evictedSnap) > 0 {
		filtered := sessions[:0:0]
		for _, s := range sessions {
			if _, evicted := evictedSnap[s.PID]; !evicted {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
	}

	dc.mu.Lock()
	// Apply expiry to the live map and re-filter against PIDs evicted after
	// our snapshot instant (concurrent evictPID).
	if len(dc.evictedPIDs) > 0 {
		for pid, evictedAt := range dc.evictedPIDs {
			if now.Sub(evictedAt) > evictGrace {
				delete(dc.evictedPIDs, pid)
				continue
			}
			if _, seen := evictedSnap[pid]; !seen {
				out := sessions[:0:0]
				for _, s := range sessions {
					if s.PID != pid {
						out = append(out, s)
					}
				}
				sessions = out
			}
		}
	}
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
		if s.PID > 0 && !osutil.PidAlive(s.PID) {
			return false // a process died, do full scan
		}
	}

	// Session list is stable but dynamic fields (lastActive, state, summary,
	// lastPrompt) may have changed. The published slice is immutable (readers
	// copy it without the lock), so RefreshDynamic must NOT run on dc.sessions
	// in place — refresh into refreshScratch (single user via refreshMu) and
	// publish a fresh copy only when something changed (#1700).
	if len(cached) > 0 {
		scratch := dc.refreshScratch[:0]
		scratch = append(scratch, cached...)
		dc.refreshScratch = scratch // retain grown capacity for reuse
		if discovery.RefreshDynamic(dc.claudeDir, scratch) {
			// Publish a fresh copy, never the scratch array. evictPID takes only
			// dc.mu (not refreshMu) and may have run since `cached` was read, so
			// filter evictedPIDs under the write lock or the killed session
			// would be resurrected.
			dc.mu.Lock()
			updated := make([]discovery.DiscoveredSession, 0, len(scratch))
			for _, s := range scratch {
				if _, evicted := dc.evictedPIDs[s.PID]; evicted {
					continue
				}
				updated = append(updated, s)
			}
			dc.sessions = updated
			dc.mu.Unlock()
		}
	}

	return true
}

// evictPID removes a specific PID from the cached snapshot immediately.
// Called after session takeover so the killed process doesn't reappear
// in the sidebar while the 10-second discovery cache is still stale.
// The PID is also added to evictedPIDs so that refresh() won't re-add
// it during the WaitAndCleanup window when the process/session file
// may still exist on disk.
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
	dc.evictedPIDs[pid] = time.Now()
}

// snapshot returns a copy of the cached discovered sessions.
func (dc *discoveryCache) snapshot() []discovery.DiscoveredSession {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	out := make([]discovery.DiscoveredSession, len(dc.sessions))
	copy(out, dc.sessions)
	return out
}

// Snapshot is the exported alias used by internal/dashboard/discovery via the
// CacheView interface.
func (dc *discoveryCache) Snapshot() []discovery.DiscoveredSession { return dc.snapshot() }

// EvictPID is the exported alias used by internal/dashboard/discovery via the
// CacheView interface.
func (dc *discoveryCache) EvictPID(pid int) { dc.evictPID(pid) }
