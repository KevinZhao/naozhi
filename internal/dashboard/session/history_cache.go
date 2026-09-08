package session

import (
	"context"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

// historySessions returns all filesystem sessions from the last 7 days.
// Results are cached for 120 seconds (see cacheTTL below).
func (h *Handlers) historySessions() []discovery.RecentSession {
	if h.claudeDir == "" {
		return nil
	}

	const cacheTTL = 120 * time.Second

	// Wait-free fast-path TTL check via the atomic mirror (#1404); the lock is
	// taken only when the TTL passes and we need a consistent slice header.
	if ns := h.historyCacheTimeUnixNano.Load(); ns != 0 && time.Since(time.Unix(0, ns)) < cacheTTL {
		h.historyCacheMu.RLock()
		// Re-confirm under lock — InvalidateHistoryCache could have written 0
		// between Load() and RLock().
		if !h.historyCacheTime.IsZero() && time.Since(h.historyCacheTime) < cacheTTL {
			cached := h.historyCache
			h.historyCacheMu.RUnlock()
			return cached
		}
		h.historyCacheMu.RUnlock()
	}

	v, _, _ := h.historyFlight.Do("history", func() (any, error) {
		// Re-check via the atomic — a prior leader may have populated the cache
		// before this closure ran (double-check pattern, as in
		// lookupSummariesCached). Population is judged by UnixNano != 0, not
		// historyCache != nil: an empty-history deployment legitimately caches a
		// nil slice and must not re-scan every TTL window.
		if ns := h.historyCacheTimeUnixNano.Load(); ns != 0 && time.Since(time.Unix(0, ns)) < cacheTTL {
			h.historyCacheMu.RLock()
			if !h.historyCacheTime.IsZero() && time.Since(h.historyCacheTime) < cacheTTL {
				cached := h.historyCache
				h.historyCacheMu.RUnlock()
				return cached, nil
			}
			h.historyCacheMu.RUnlock()
		}
		return h.loadHistorySessions(), nil
	})

	if res, ok := v.([]discovery.RecentSession); ok {
		return res
	}
	return nil
}

// uptimeSnapshot is the value cached by uptimeCache: Bucket is whole seconds
// since startedAt, Str the pre-formatted rendering at that resolution.
type uptimeSnapshot struct {
	Bucket int64
	Str    string
}

// uptimeStringAt returns time.Since(startedAt).Round(time.Second).String()
// memoised per 1-second bucket. Concurrent misses may format the same value;
// last-writer-wins via unconditional Store is intentional.
func (h *Handlers) uptimeStringAt(now time.Time) string {
	d := now.Sub(h.startedAt).Round(time.Second)
	bucket := int64(d / time.Second)
	if cur := h.uptimeCache.Load(); cur != nil && cur.Bucket == bucket {
		return cur.Str
	}
	s := d.String()
	h.uptimeCache.Store(&uptimeSnapshot{Bucket: bucket, Str: s})
	return s
}

// InitStaticStats pre-builds the immutable subset of /api/sessions stats so
// HandleList only overlays the dynamic counters per poll. The Once guards
// against a re-run racing with concurrent HandleList readers.
func (h *Handlers) InitStaticStats() {
	h.staticStatsOnce.Do(h.doInitStaticStats)
}
func (h *Handlers) doInitStaticStats() {
	// Deep-copy the callSystemInfo() singleton: staticStats is copied by value
	// per poll but the System map is a reference, so without this every
	// response would alias the process-wide map and any future mutable field
	// would be a data race.
	sysSrc := h.callSystemInfo()
	sysCopy := make(map[string]any, len(sysSrc))
	for k, v := range sysSrc {
		sysCopy[k] = v
	}
	// Copy agentIDs for the same read-only contract; guards against a future
	// mutable element type introducing a cross-goroutine race.
	agentsCopy := make([]string, len(h.agentIDs))
	copy(agentsCopy, h.agentIDs)
	h.staticStats = sessionStatsStatic{
		Backend:          h.backendTag,
		CLIName:          h.router.CLIName(),
		CLIVersion:       h.router.CLIVersion(),
		MaxProcs:         h.router.MaxProcs(),
		DefaultWorkspace: h.router.DefaultWorkspace(),
		WorkspaceID:      h.workspaceID,
		WorkspaceName:    h.workspaceName,
		System:           sysCopy,
		Agents:           agentsCopy,
	}
}

// WarmHistoryCache pre-populates the history cache in the background so the
// first dashboard load doesn't block on a full FS scan. The goroutine is
// tracked by warmHistoryWg so WaitWarmHistory can block shutdown until the
// scan finishes.
func (h *Handlers) WarmHistoryCache() {
	if h.claudeDir == "" {
		return
	}
	h.warmHistoryWg.Add(1)
	go func() {
		defer h.warmHistoryWg.Done()
		h.historyFlight.Do("history", func() (any, error) {
			return h.loadHistorySessions(), nil
		})
	}()
}

// WaitWarmHistory blocks until any in-flight WarmHistoryCache goroutine
// completes. Call from server shutdown after refusing new requests to
// guarantee no background loadHistorySessions races with teardown.
func (h *Handlers) WaitWarmHistory() {
	h.warmHistoryWg.Wait()
}

// InvalidateHistoryCache forces the next poll to repopulate historyCache from
// disk. Wired into Router.SetOnKeyRetired so a just-retired session's jsonl
// appears in the history popover within one poll instead of up to 120s later.
func (h *Handlers) InvalidateHistoryCache() {
	h.historyCacheMu.Lock()
	h.historyCache = nil
	h.historyCacheTime = time.Time{}
	// Keep the atomic mirror in lockstep (Store=0 ⇔ time.Time{}) so wait-free
	// readers see the invalidation immediately.
	h.historyCacheTimeUnixNano.Store(0)
	h.historyCacheMu.Unlock()
}

// lookupSummariesCached returns sessionID→summary with a 30s TTL cache. The
// full lookup result is stored and served to any snapshot subset; concurrent
// misses at the TTL boundary collapse via summaryFlight so N tab polls don't
// each pay the N×os.Stat fan-out.
func (h *Handlers) lookupSummariesCached(snapshots []sessionpkg.SessionSnapshot) map[string]string {
	const summaryTTL = 30 * time.Second

	h.summaryCacheMu.RLock()
	if h.summaryCache != nil && time.Since(h.summaryCacheTime) < summaryTTL {
		cached := h.summaryCache
		h.summaryCacheMu.RUnlock()
		return cached
	}
	h.summaryCacheMu.RUnlock()

	// Fixed "summary" flight key: the leader's result is cached for the whole
	// 30s window regardless of which subset drove the miss. sessionWorkspaces is
	// built INSIDE the closure so followers don't pay an O(N) map they'd
	// discard; whichever caller's snapshots win the race is acceptable for a
	// 30s window.
	v, _, _ := h.summaryFlight.Do("summary", func() (any, error) {
		// Re-check under lock — a prior leader could have populated the
		// cache between our expiry detection and this closure running.
		h.summaryCacheMu.RLock()
		if h.summaryCache != nil && time.Since(h.summaryCacheTime) < summaryTTL {
			cached := h.summaryCache
			h.summaryCacheMu.RUnlock()
			return cached, nil
		}
		h.summaryCacheMu.RUnlock()

		// Only look up sessions that don't already carry a Summary (#1403): the
		// fill loop is a no-op on missing keys, so wire output is identical
		// while a cold miss shrinks the fan-out to O(N_new). Sized worst-case so
		// the poll right after a workspace switch doesn't pay map growth.
		sessionWorkspaces := make(map[string]string, len(snapshots))
		for _, snap := range snapshots {
			if snap.SessionID == "" || snap.Workspace == "" {
				continue
			}
			if snap.Summary != "" {
				continue
			}
			sessionWorkspaces[snap.SessionID] = snap.Workspace
		}
		fresh := discovery.LookupSummaries(h.claudeDir, sessionWorkspaces)

		h.summaryCacheMu.Lock()
		h.summaryCache = fresh
		h.summaryCacheTime = time.Now()
		h.summaryCacheMu.Unlock()
		return fresh, nil
	})
	if m, ok := v.(map[string]string); ok {
		return m
	}
	return nil
}
func (h *Handlers) loadHistorySessions() []discovery.RecentSession {
	excludeIDs := h.router.DiscoveryExcludeIDs()

	// Hide cron-spawned and sys-session JSONLs (both have their own UI; the sys
	// workdir lives under ~/.claude/projects and would leak AutoTitler prompts).
	// KnownSessionIDs is O(jobs × 200), so snapshot it once per scan.
	filter := historyFilter{skipWorkspace: h.sysWorkDir}
	if h.cronSessions != nil {
		filter.skipSessions = h.cronSessions.KnownSessionIDs()
	}
	// Cap the walk so a slow/hung home (NFS, FUSE) can't pin the flight leader (#2134).
	ctx, cancel := context.WithTimeout(context.Background(), historyScanTimeout)
	defer cancel()
	all := discovery.RecentSessionsCtx(ctx, h.claudeDir, 200, 7*24*time.Hour, excludeIDs, filter)

	// Resolve project names in batch using the pooled scratch slice (#616).
	if h.projectMgr != nil && len(all) > 0 {
		wsPtr := borrowWorkspaces(len(all))
		workspaces := *wsPtr
		for _, rs := range all {
			workspaces = append(workspaces, rs.Workspace)
		}
		*wsPtr = workspaces
		wsMap := h.projectMgr.ResolveWorkspaces(workspaces)
		returnWorkspaces(wsPtr)
		for i := range all {
			all[i].Project = wsMap[all[i].Workspace]
		}
	}

	// Stamp retired_at from one Snapshot() so the loop is O(N) without N mutex
	// acquires.
	if h.retiredStore != nil && len(all) > 0 {
		retiredMap := h.retiredStore.Snapshot()
		if len(retiredMap) > 0 {
			for i := range all {
				if ts := retiredMap[all[i].SessionID]; ts > 0 {
					all[i].RetiredAt = ts
				}
			}
		}
	}

	now := time.Now() // outside the lock to keep vDSO off the critical section
	h.historyCacheMu.Lock()
	h.historyCache = all
	h.historyCacheTime = now
	// Mirror update under the lock so wait-free readers never see atomic-fresh
	// while h.historyCache still points at the old slice.
	h.historyCacheTimeUnixNano.Store(now.UnixNano())
	h.historyCacheMu.Unlock()

	return all
}

// callSystemInfo invokes the injected systemInfoFn. nil falls through to
// an empty map (test paths without system-probe wiring).
func (h *Handlers) callSystemInfo() map[string]any {
	if h.systemInfoFn == nil {
		return map[string]any{}
	}
	return h.systemInfoFn()
}
