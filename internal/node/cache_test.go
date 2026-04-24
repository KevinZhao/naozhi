package node

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// stubConn implements Conn for cache tests.
type stubConn struct {
	nodeID   string
	sessions []map[string]any
	projects []map[string]any
	disc     []map[string]any
	sessErr  error
	projErr  error
	discErr  error

	fetchSessionsCount atomic.Int32
}

func (s *stubConn) NodeID() string      { return s.nodeID }
func (s *stubConn) DisplayName() string { return s.nodeID }
func (s *stubConn) RemoteAddr() string  { return "stub" }
func (s *stubConn) Status() string      { return "ok" }
func (s *stubConn) Close()              {}

func (s *stubConn) FetchSessions(_ context.Context) ([]map[string]any, error) {
	s.fetchSessionsCount.Add(1)
	return s.sessions, s.sessErr
}
func (s *stubConn) FetchProjects(_ context.Context) ([]map[string]any, error) {
	return s.projects, s.projErr
}
func (s *stubConn) FetchDiscovered(_ context.Context) ([]map[string]any, error) {
	return s.disc, s.discErr
}
func (s *stubConn) FetchDiscoveredPreview(_ context.Context, _ string) ([]cli.EventEntry, error) {
	return nil, nil
}
func (s *stubConn) FetchEvents(_ context.Context, _ string, _ int64) ([]cli.EventEntry, error) {
	return nil, nil
}
func (s *stubConn) Send(_ context.Context, _, _, _ string) error { return nil }
func (s *stubConn) ProxyTakeover(_ context.Context, _ int, _, _ string, _ uint64) (string, error) {
	return "", nil
}
func (s *stubConn) ProxyCloseDiscovered(_ context.Context, _ int, _, _ string, _ uint64) error {
	return nil
}
func (s *stubConn) ProxyRestartPlanner(_ context.Context, _ string) error { return nil }
func (s *stubConn) ProxyUpdateConfig(_ context.Context, _ string, _ json.RawMessage) error {
	return nil
}
func (s *stubConn) ProxySetFavorite(_ context.Context, _ string, _ bool) error { return nil }
func (s *stubConn) ProxyRemoveSession(_ context.Context, _ string) (bool, error) {
	return true, nil
}
func (s *stubConn) ProxyInterruptSession(_ context.Context, _ string) (bool, error) {
	return true, nil
}
func (s *stubConn) Subscribe(_ EventSink, _ string, _ int64) {}
func (s *stubConn) Unsubscribe(_ EventSink, _ string)        {}
func (s *stubConn) RefreshSubscription(_ string)             {}
func (s *stubConn) RemoveClient(_ EventSink)                 {}

// ---- NewCacheManager ----

func TestCacheManager_NewCacheManager(t *testing.T) {
	called := false
	cm := NewCacheManager(
		func() map[string]Conn { return nil },
		func() { called = true },
	)
	if cm == nil {
		t.Fatal("expected non-nil CacheManager")
	}
	sessions, status := cm.Sessions()
	if len(sessions) != 0 {
		t.Errorf("expected empty sessions, got %v", sessions)
	}
	if len(status) != 0 {
		t.Errorf("expected empty status, got %v", status)
	}
	_ = called
}

// ---- RefreshAll populates cache ----

func TestCacheManager_RefreshAll(t *testing.T) {
	node := &stubConn{
		nodeID:   "node-1",
		sessions: []map[string]any{{"session_id": "s1"}, {"session_id": "s2"}},
		projects: []map[string]any{{"name": "proj1"}},
		disc:     []map[string]any{{"pid": 100}},
	}

	var onChangeCalled atomic.Int32
	cm := NewCacheManager(
		func() map[string]Conn { return map[string]Conn{"node-1": node} },
		func() { onChangeCalled.Add(1) },
	)
	cm.RefreshAll()

	sessions, status := cm.Sessions()
	if len(sessions["node-1"]) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(sessions["node-1"]))
	}
	if status["node-1"] != "ok" {
		t.Errorf("expected status 'ok', got %q", status["node-1"])
	}

	projs := cm.Projects()
	if len(projs["node-1"]) != 1 {
		t.Errorf("expected 1 project, got %d", len(projs["node-1"]))
	}

	disc := cm.Discovered()
	if len(disc["node-1"]) != 1 {
		t.Errorf("expected 1 discovered, got %d", len(disc["node-1"]))
	}

	if onChangeCalled.Load() == 0 {
		t.Error("expected onChange to be called")
	}
}

// ---- RefreshAll injects "node" field into session maps ----

func TestCacheManager_RefreshAll_injectsNodeField(t *testing.T) {
	node := &stubConn{
		nodeID:   "my-node",
		sessions: []map[string]any{{"session_id": "s1"}},
	}
	cm := NewCacheManager(
		func() map[string]Conn { return map[string]Conn{"my-node": node} },
		nil,
	)
	cm.RefreshAll()

	sessions, _ := cm.Sessions()
	for _, s := range sessions["my-node"] {
		if s["node"] != "my-node" {
			t.Errorf("expected node field 'my-node', got %v", s["node"])
		}
	}
}

// ---- RefreshAll marks node as error on FetchSessions failure ----

func TestCacheManager_RefreshAll_fetchError(t *testing.T) {
	node := &stubConn{
		nodeID:  "bad-node",
		sessErr: errors.New("connection refused"),
	}
	cm := NewCacheManager(
		func() map[string]Conn { return map[string]Conn{"bad-node": node} },
		nil,
	)
	cm.RefreshAll()

	_, status := cm.Sessions()
	if status["bad-node"] != "error" {
		t.Errorf("expected status 'error', got %q", status["bad-node"])
	}
}

// ---- RefreshAll with no nodes is a no-op ----

func TestCacheManager_RefreshAll_noNodes(t *testing.T) {
	var onChangeCalled atomic.Int32
	cm := NewCacheManager(
		func() map[string]Conn { return map[string]Conn{} },
		func() { onChangeCalled.Add(1) },
	)
	cm.RefreshAll()
	// onChange is always called even with no nodes.
	_ = onChangeCalled.Load()
}

// ---- RefreshFor updates a single node ----

func TestCacheManager_RefreshFor(t *testing.T) {
	node := &stubConn{
		nodeID:   "node-x",
		sessions: []map[string]any{{"session_id": "abc"}},
	}
	cm := NewCacheManager(
		func() map[string]Conn { return map[string]Conn{"node-x": node} },
		nil,
	)
	cm.RefreshFor("node-x")

	sessions, status := cm.Sessions()
	if len(sessions["node-x"]) != 1 {
		t.Errorf("expected 1 session, got %d", len(sessions["node-x"]))
	}
	if status["node-x"] != "ok" {
		t.Errorf("expected status 'ok', got %q", status["node-x"])
	}
}

// ---- RefreshFor with unknown node is a no-op ----

func TestCacheManager_RefreshFor_unknownNode(t *testing.T) {
	cm := NewCacheManager(
		func() map[string]Conn { return map[string]Conn{} },
		nil,
	)
	// Must not panic.
	cm.RefreshFor("no-such-node")
}

// ---- RefreshFor preserves existing cache on sessErr ----

func TestCacheManager_RefreshFor_preserveOnError(t *testing.T) {
	node := &stubConn{
		nodeID:   "node-y",
		sessions: []map[string]any{{"session_id": "existing"}},
	}
	cm := NewCacheManager(
		func() map[string]Conn { return map[string]Conn{"node-y": node} },
		nil,
	)
	// Seed with good data.
	cm.RefreshFor("node-y")

	// Now inject error.
	node.sessErr = errors.New("timeout")
	cm.RefreshFor("node-y")

	// Existing sessions should be preserved.
	sessions, status := cm.Sessions()
	if len(sessions["node-y"]) != 1 {
		t.Errorf("expected cached sessions preserved, got %d", len(sessions["node-y"]))
	}
	if status["node-y"] != "error" {
		t.Errorf("expected status 'error', got %q", status["node-y"])
	}
}

// ---- PurgeNode removes data ----

func TestCacheManager_PurgeNode(t *testing.T) {
	node := &stubConn{
		nodeID:   "purge-me",
		sessions: []map[string]any{{"session_id": "x"}},
		projects: []map[string]any{{"name": "p"}},
		disc:     []map[string]any{{"pid": 1}},
	}
	cm := NewCacheManager(
		func() map[string]Conn { return map[string]Conn{"purge-me": node} },
		nil,
	)
	cm.RefreshAll()

	cm.PurgeNode("purge-me")

	sessions, status := cm.Sessions()
	if len(sessions["purge-me"]) != 0 {
		t.Errorf("expected sessions purged, got %v", sessions["purge-me"])
	}
	if status["purge-me"] != "error" {
		t.Errorf("expected status 'error' after purge, got %q", status["purge-me"])
	}
	if projs := cm.Projects(); len(projs["purge-me"]) != 0 {
		t.Errorf("expected projects purged, got %v", projs["purge-me"])
	}
	if disc := cm.Discovered(); len(disc["purge-me"]) != 0 {
		t.Errorf("expected discovered purged, got %v", disc["purge-me"])
	}
}

// ---- Sessions returns a copy (modifying doesn't affect cache) ----

func TestCacheManager_Sessions_returnsCopy(t *testing.T) {
	node := &stubConn{
		nodeID:   "n1",
		sessions: []map[string]any{{"session_id": "s1"}},
	}
	cm := NewCacheManager(
		func() map[string]Conn { return map[string]Conn{"n1": node} },
		nil,
	)
	cm.RefreshAll()

	sessions1, _ := cm.Sessions()
	sessions1["extra-key"] = []map[string]any{{"injected": true}}

	sessions2, _ := cm.Sessions()
	if _, ok := sessions2["extra-key"]; ok {
		t.Error("modifying returned map should not affect cache")
	}
}

// ---- StartLoop runs periodic refreshes ----

func TestCacheManager_StartLoop(t *testing.T) {
	node := &stubConn{
		nodeID:   "loop-node",
		sessions: []map[string]any{{"session_id": "l1"}},
	}

	var mu sync.Mutex
	nodes := map[string]Conn{"loop-node": node}

	cm := NewCacheManager(
		func() map[string]Conn {
			mu.Lock()
			defer mu.Unlock()
			cp := make(map[string]Conn, len(nodes))
			for k, v := range nodes {
				cp[k] = v
			}
			return cp
		},
		nil,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cm.StartLoop(ctx)

	// Wait for at least one eager refresh.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sessions, _ := cm.Sessions()
		if len(sessions["loop-node"]) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	sessions, _ := cm.Sessions()
	if len(sessions["loop-node"]) == 0 {
		t.Error("expected StartLoop to populate sessions via eager refresh")
	}
}

// ---- RefreshAll parallel execution is race-free ----

func TestCacheManager_RefreshAll_parallel(t *testing.T) {
	// Each node returns a fresh slice on each call; the getNodes function
	// returns a fresh map snapshot to avoid concurrent iteration races.
	makeNodes := func() map[string]Conn {
		m := make(map[string]Conn)
		for i := 0; i < 5; i++ {
			id := "n" + string(rune('0'+i))
			m[id] = &stubConn{
				nodeID:   id,
				sessions: []map[string]any{{"session_id": id + "-s"}},
			}
		}
		return m
	}

	cm := NewCacheManager(makeNodes, nil)

	// Multiple concurrent RefreshAll calls must not race.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cm.RefreshAll()
		}()
	}
	wg.Wait()

	sessions, _ := cm.Sessions()
	for i := 0; i < 5; i++ {
		id := "n" + string(rune('0'+i))
		if len(sessions[id]) == 0 {
			t.Errorf("expected sessions for node %q", id)
		}
	}
}
