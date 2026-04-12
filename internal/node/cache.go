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
	// Eager first fetch in background (no-op if no nodes yet)
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

// RefreshAll fetches and caches data from all active nodes in parallel.
func (m *CacheManager) RefreshAll() {
	nodesCopy := m.getNodes()

	type result struct {
		nodeID     string
		sessions   []map[string]any
		projects   []map[string]any
		discovered []map[string]any
		err        error
	}
	ch := make(chan result, len(nodesCopy))
	for id, nc := range nodesCopy {
		go func(id string, nc Conn) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var wg sync.WaitGroup
			var sessions []map[string]any
			var projects []map[string]any
			var discovered []map[string]any
			var sessErr error
			wg.Add(3)
			go func() { defer wg.Done(); sessions, sessErr = nc.FetchSessions(ctx) }()
			go func() {
				defer wg.Done()
				var err error
				projects, err = nc.FetchProjects(ctx)
				if err != nil {
					slog.Debug("node cache: FetchProjects failed", "node", id, "err", err)
				}
			}()
			go func() {
				defer wg.Done()
				var err error
				discovered, err = nc.FetchDiscovered(ctx)
				if err != nil {
					slog.Debug("node cache: FetchDiscovered failed", "node", id, "err", err)
				}
			}()
			wg.Wait()
			ch <- result{id, sessions, projects, discovered, sessErr}
		}(id, nc)
	}

	newSessions := make(map[string][]map[string]any, len(nodesCopy))
	newProjects := make(map[string][]map[string]any, len(nodesCopy))
	newDiscovered := make(map[string][]map[string]any, len(nodesCopy))
	newStatus := make(map[string]string, len(nodesCopy))

	for i := 0; i < len(nodesCopy); i++ {
		res := <-ch
		if res.err != nil {
			slog.Debug("node cache refresh", "node", res.nodeID, "err", res.err)
			newStatus[res.nodeID] = "error"
			continue
		}
		newStatus[res.nodeID] = "ok"
		for _, rs := range res.sessions {
			rs["node"] = res.nodeID
		}
		newSessions[res.nodeID] = res.sessions
		for _, rp := range res.projects {
			rp["node"] = res.nodeID
		}
		newProjects[res.nodeID] = res.projects
		for _, rd := range res.discovered {
			rd["node"] = res.nodeID
		}
		newDiscovered[res.nodeID] = res.discovered
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

	// Only update successfully-fetched data; preserve existing cache on
	// transient errors (matching RefreshAll's continue-on-error behavior).
	m.mu.Lock()
	if sessErr == nil {
		for _, rs := range sessions {
			rs["node"] = id
		}
		m.sessions[id] = sessions
	}
	if projErr == nil {
		for _, rp := range projects {
			rp["node"] = id
		}
		m.projects[id] = projects
	}
	if discErr == nil {
		for _, rd := range discovered {
			rd["node"] = id
		}
		m.discovered[id] = discovered
	}
	m.status[id] = status
	m.mu.Unlock()

	if m.onChange != nil {
		m.onChange()
	}
}

// PurgeNode removes all cached data for a node and marks it as error.
// Called when a reverse-connected node disconnects.
func (m *CacheManager) PurgeNode(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	delete(m.projects, id)
	delete(m.discovered, id)
	m.status[id] = "error"
	m.mu.Unlock()
}

// Sessions returns a snapshot of cached sessions and node status maps.
// Returns shallow copies so callers can iterate without holding the lock.
func (m *CacheManager) Sessions() (map[string][]map[string]any, map[string]string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessions := make(map[string][]map[string]any, len(m.sessions))
	for k, v := range m.sessions {
		sessions[k] = v
	}
	status := make(map[string]string, len(m.status))
	for k, v := range m.status {
		status[k] = v
	}
	return sessions, status
}

// Projects returns a snapshot of cached projects per node.
func (m *CacheManager) Projects() map[string][]map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := make(map[string][]map[string]any, len(m.projects))
	for k, v := range m.projects {
		cp[k] = v
	}
	return cp
}

// Discovered returns a snapshot of cached discovered sessions per node.
func (m *CacheManager) Discovered() map[string][]map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := make(map[string][]map[string]any, len(m.discovered))
	for k, v := range m.discovered {
		cp[k] = v
	}
	return cp
}
