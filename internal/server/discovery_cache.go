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

	// refreshMu serialises refresh() so the two startLoop goroutines (initial
	// one-shot + the 10s ticker) cannot run a Scan / tryShortCircuit
	// concurrently. startLoop launches both independently and nothing
	// otherwise orders them — a first full Scan that runs longer than the 10s
	// tick interval (slow disk / huge ~/.claude) would otherwise let the tick
	// fire a second concurrent refresh. Beyond preventing duplicate Scan work,
	// this single-flight guarantee is what makes refreshScratch safe to keep
	// as shared state. R20260603-PERF-2 (#1700).
	refreshMu sync.Mutex

	// refreshScratch is a reusable buffer owned by the refresh path (guarded
	// by refreshMu, never published to readers). tryShortCircuit copies the
	// cached sessions into it and runs RefreshDynamic in place, allocating a
	// fresh published snapshot only when a dynamic field actually changed.
	// This drops the steady-state idle tick from one N-element allocation to
	// zero. R20260603-PERF-2 (#1700).
	refreshScratch []discovery.DiscoveredSession

	wg sync.WaitGroup // tracks the initial refresh goroutine started by startLoop

	claudeDir  string
	getExclude func() (pids map[int]bool, sessionIDs map[string]bool, cwds map[string]bool)
	projectMgr *project.Manager

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
// Wait() on them after cancelling ctx — otherwise a tick fired between
// ctx-cancel and projectMgr cleanup would race on disposed state. R218B-GO-1.
//
// R242-GO-19: the initial refresh goroutine previously ignored ctx
// entirely — Server.Shutdown could cancel before refresh()'s blocking
// discovery.Scan finished, leaving Wait() blocked on a goroutine no one
// could interrupt. Tick-loop already short-circuits on ctx.Done; gate
// the initial call the same way so a SIGTERM during boot doesn't burn
// the full Scan budget on a server about to die anyway.
func (dc *discoveryCache) startLoop(ctx context.Context) {
	dc.wg.Add(1)
	go func() {
		defer dc.wg.Done()
		// Quick pre-scan ctx check so a SIGTERM caught before this
		// goroutine is scheduled (or while still parked) skips the
		// expensive Scan entirely. Scan itself doesn't take ctx, so
		// this is the only point we can pre-empt without invasive
		// surgery to discovery.Scan; an in-flight Scan still runs to
		// completion (≤O(few hundred ms) for a typical claude dir).
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

	// R20260603-PERF-2 (#1700): single-flight the refresh path so the two
	// startLoop goroutines never Scan / mutate refreshScratch concurrently.
	dc.refreshMu.Lock()
	defer dc.refreshMu.Unlock()

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

	// Filter out recently-evicted PIDs and store the final result.
	now := time.Now()
	const evictGrace = 60 * time.Second
	dc.mu.Lock()
	if len(dc.evictedPIDs) > 0 {
		for pid, evictedAt := range dc.evictedPIDs {
			if now.Sub(evictedAt) > evictGrace {
				delete(dc.evictedPIDs, pid)
			}
		}
	}
	if len(dc.evictedPIDs) > 0 {
		filtered := sessions[:0:0]
		for _, s := range sessions {
			if _, evicted := dc.evictedPIDs[s.PID]; !evicted {
				filtered = append(filtered, s)
			}
		}
		sessions = filtered
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

	// Session list is stable (no new/removed processes), but dynamic fields
	// (lastActive, state, summary, lastPrompt) may have changed because the
	// CLI keeps writing to JSONL files.  Do a lightweight refresh that only
	// stats files and hits the mtime-based caches.
	//
	// R20260603-PERF-2 (#1700): we must NOT call RefreshDynamic on dc.sessions
	// in place — snapshot() hands the same backing array to dashboard readers
	// (and evictPID's [:0:0] establishes the published-slice-is-immutable
	// contract), so a concurrent reader copying it while RefreshDynamic
	// rewrites string headers in place is a data race. The previous code
	// therefore allocated a full N-element copy EVERY tick (~10s) even when no
	// dynamic field changed — pure churn on the steady-state idle path.
	//
	// RefreshDynamic returns changed, so we can refresh into a refresh-
	// goroutine-private scratch buffer (refreshScratch, never published) and
	// only publish a fresh immutable copy when something actually changed. On
	// the common idle path (no JSONL writes since the last tick) changed=false
	// and we allocate nothing and never take the write lock. refreshMu
	// (held by the caller) guarantees refreshScratch has a single user.
	if len(cached) > 0 {
		scratch := dc.refreshScratch[:0]
		scratch = append(scratch, cached...)
		dc.refreshScratch = scratch // retain grown capacity for reuse
		if discovery.RefreshDynamic(dc.claudeDir, scratch) {
			// Publish a fresh immutable snapshot — never hand readers the
			// reusable scratch backing array.
			updated := make([]discovery.DiscoveredSession, len(scratch))
			copy(updated, scratch)
			dc.mu.Lock()
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
// CacheView interface. Phase 3b (server-split-phase4-design.md §6.5 Plan B).
func (dc *discoveryCache) Snapshot() []discovery.DiscoveredSession { return dc.snapshot() }

// EvictPID is the exported alias used by internal/dashboard/discovery via the
// CacheView interface. Phase 3b.
func (dc *discoveryCache) EvictPID(pid int) { dc.evictPID(pid) }
