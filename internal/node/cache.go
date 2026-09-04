package node

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// CacheManager periodically fetches and caches remote node data
// so dashboard API calls never block on unreachable nodes.
type CacheManager struct {
	mu         sync.RWMutex
	sessions   map[string][]map[string]any // nodeID → cached sessions
	projects   map[string][]map[string]any // nodeID → cached projects
	discovered map[string][]map[string]any // nodeID → cached discovered
	status     map[string]string           // nodeID → "ok" | "error"

	getNodes func() map[string]Conn // returns snapshot of active nodes under lock
	onChange func()                 // called after cache update (e.g. BroadcastSessionsUpdate)

	// baseCtx parents per-refresh RPC timeouts so shutdown cancels in-flight
	// fetches; set once by StartLoop.
	baseCtx context.Context
}

// NewCacheManager creates a cache manager.
// getNodes returns a snapshot of active nodes (caller handles locking).
// onChange is called after cache updates to notify UI clients.
func NewCacheManager(getNodes func() map[string]Conn, onChange func()) *CacheManager {
	return &CacheManager{
		sessions:   make(map[string][]map[string]any),
		projects:   make(map[string][]map[string]any),
		discovered: make(map[string][]map[string]any),
		status:     make(map[string]string),
		getNodes:   getNodes,
		onChange:   onChange,
	}
}

// StartLoop begins periodic cache refresh every 10 seconds.
func (m *CacheManager) StartLoop(ctx context.Context) {
	m.mu.Lock()
	m.baseCtx = ctx
	m.mu.Unlock()

	go m.RefreshAll()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.RefreshAll()
			}
		}
	}()
}

// refreshCtx returns a per-RPC timeout derived from baseCtx when set, else
// Background (bootstrap / tests).
func (m *CacheManager) refreshCtx(timeout time.Duration) (context.Context, context.CancelFunc) {
	m.mu.RLock()
	parent := m.baseCtx
	m.mu.RUnlock()
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, timeout)
}

// RefreshAll fetches and caches data from all active nodes in parallel.
func (m *CacheManager) RefreshAll() {
	nodesCopy := m.getNodes()

	type result struct {
		nodeID     string
		sessions   []map[string]any
		projects   []map[string]any
		discovered []map[string]any
		err        error
		projErr    error
		discErr    error
	}
	ch := make(chan result, len(nodesCopy))
	for id, nc := range nodesCopy {
		go func(id string, nc Conn) {
			ctx, cancel := m.refreshCtx(5 * time.Second)
			defer cancel()
			var wg sync.WaitGroup
			var sessions []map[string]any
			var projects []map[string]any
			var discovered []map[string]any
			var sessErr, projErr, discErr error
			wg.Add(3)
			go func() { defer wg.Done(); sessions, sessErr = nc.FetchSessions(ctx) }()
			go func() {
				defer wg.Done()
				projects, projErr = nc.FetchProjects(ctx)
				if projErr != nil {
					slog.Debug("node cache: FetchProjects failed", "node", id, "err", projErr)
				}
			}()
			go func() {
				defer wg.Done()
				discovered, discErr = nc.FetchDiscovered(ctx)
				if discErr != nil {
					slog.Debug("node cache: FetchDiscovered failed", "node", id, "err", discErr)
				}
			}()
			wg.Wait()
			ch <- result{id, sessions, projects, discovered, sessErr, projErr, discErr}
		}(id, nc)
	}

	newSessions := make(map[string][]map[string]any, len(nodesCopy))
	newProjects := make(map[string][]map[string]any, len(nodesCopy))
	newDiscovered := make(map[string][]map[string]any, len(nodesCopy))
	newStatus := make(map[string]string, len(nodesCopy))

	// Snapshot so a transient fetch error preserves the node's last-known data
	// instead of erasing it from the dashboard until the next success.
	m.mu.RLock()
	prevSessions := m.sessions
	prevProjects := m.projects
	prevDiscovered := m.discovered
	m.mu.RUnlock()

	for i := 0; i < len(nodesCopy); i++ {
		res := <-ch
		if res.err != nil {
			slog.Debug("node cache refresh", "node", res.nodeID, "err", res.err)
			newStatus[res.nodeID] = "error"
			if s, ok := prevSessions[res.nodeID]; ok {
				newSessions[res.nodeID] = s
			}
			if p, ok := prevProjects[res.nodeID]; ok {
				newProjects[res.nodeID] = p
			}
			if d, ok := prevDiscovered[res.nodeID]; ok {
				newDiscovered[res.nodeID] = d
			}
			continue
		}
		newStatus[res.nodeID] = "ok"
		for _, rs := range res.sessions {
			rs["node"] = res.nodeID
		}
		newSessions[res.nodeID] = res.sessions
		// projects/discovered are independent RPCs; preserve last-known on
		// failure so a transient error does not blank the sub-items.
		if res.projErr == nil {
			for _, rp := range res.projects {
				rp["node"] = res.nodeID
			}
			newProjects[res.nodeID] = res.projects
		} else if p, ok := prevProjects[res.nodeID]; ok {
			newProjects[res.nodeID] = p
		}
		if res.discErr == nil {
			for _, rd := range res.discovered {
				rd["node"] = res.nodeID
			}
			newDiscovered[res.nodeID] = res.discovered
		} else if d, ok := prevDiscovered[res.nodeID]; ok {
			newDiscovered[res.nodeID] = d
		}
	}

	m.mu.Lock()
	m.sessions = newSessions
	m.projects = newProjects
	m.discovered = newDiscovered
	m.status = newStatus
	m.mu.Unlock()

	if m.onChange != nil {
		m.onChange()
	}
}

// RefreshFor fetches and caches data for a single node immediately.
// Called on reverse-node connect and on sessions_changed push.
func (m *CacheManager) RefreshFor(id string) {
	nodes := m.getNodes()
	nc, ok := nodes[id]
	if !ok {
		return
	}

	ctx, cancel := m.refreshCtx(5 * time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var sessions, projects, discovered []map[string]any
	var sessErr, projErr, discErr error
	wg.Add(3)
	go func() { defer wg.Done(); sessions, sessErr = nc.FetchSessions(ctx) }()
	go func() {
		defer wg.Done()
		projects, projErr = nc.FetchProjects(ctx)
		if projErr != nil {
			slog.Debug("node cache: FetchProjects failed", "node", id, "err", projErr)
		}
	}()
	go func() {
		defer wg.Done()
		discovered, discErr = nc.FetchDiscovered(ctx)
		if discErr != nil {
			slog.Debug("node cache: FetchDiscovered failed", "node", id, "err", discErr)
		}
	}()
	wg.Wait()

	status := "ok"
	if sessErr != nil {
		status = "error"
	}

	// Preserve existing cache on transient errors. Copy-on-write: published
	// maps are never mutated in place, so the getters hand out the live
	// reference and readers iterate race-free.
	m.mu.Lock()
	if sessErr == nil {
		for _, rs := range sessions {
			rs["node"] = id
		}
		m.sessions = cloneSetSlice(m.sessions, id, sessions)
	}
	if projErr == nil {
		for _, rp := range projects {
			rp["node"] = id
		}
		m.projects = cloneSetSlice(m.projects, id, projects)
	}
	if discErr == nil {
		for _, rd := range discovered {
			rd["node"] = id
		}
		m.discovered = cloneSetSlice(m.discovered, id, discovered)
	}
	m.status = cloneSetString(m.status, id, status)
	m.mu.Unlock()

	if m.onChange != nil {
		m.onChange()
	}
}

// PurgeNode removes all cached data for a node and marks it as error
// (copy-on-write, see RefreshFor).
func (m *CacheManager) PurgeNode(id string) {
	m.mu.Lock()
	m.sessions = cloneDeleteSlice(m.sessions, id)
	m.projects = cloneDeleteSlice(m.projects, id)
	m.discovered = cloneDeleteSlice(m.discovered, id)
	m.status = cloneSetString(m.status, id, "error")
	m.mu.Unlock()
}

// Sessions returns the cached sessions and node status maps. Published
// copy-on-write, so callers may iterate without the lock but MUST treat the
// maps (and their slices/inner maps) as read-only.
func (m *CacheManager) Sessions() (map[string][]map[string]any, map[string]string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions, m.status
}

// Projects returns the cached projects per node. Read-only; see Sessions.
func (m *CacheManager) Projects() map[string][]map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.projects
}

// Discovered returns the cached discovered sessions per node. Read-only; see Sessions.
func (m *CacheManager) Discovered() map[string][]map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.discovered
}

// cloneSetSlice returns a copy of src with key=val set; src stays untouched.
func cloneSetSlice(src map[string][]map[string]any, key string, val []map[string]any) map[string][]map[string]any {
	dst := make(map[string][]map[string]any, len(src)+1)
	for k, v := range src {
		dst[k] = v
	}
	dst[key] = val
	return dst
}

// cloneDeleteSlice returns a copy of src with key removed.
func cloneDeleteSlice(src map[string][]map[string]any, key string) map[string][]map[string]any {
	if _, ok := src[key]; !ok {
		return src
	}
	dst := make(map[string][]map[string]any, len(src))
	for k, v := range src {
		if k == key {
			continue
		}
		dst[k] = v
	}
	return dst
}

// cloneSetString returns a copy of src with key=val set.
func cloneSetString(src map[string]string, key, val string) map[string]string {
	dst := make(map[string]string, len(src)+1)
	for k, v := range src {
		dst[k] = v
	}
	dst[key] = val
	return dst
}
