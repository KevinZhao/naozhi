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
func (s *stubConn) Meta() *NodeMeta     { return &NodeMeta{NodeID: s.nodeID} }
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
func (s *stubConn) ProxySetSessionLabel(_ context.Context, _, _ string) (bool, error) {
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

// ---- RefreshAll preserves last-known cache on transient FetchSessions error ----

func TestCacheManager_RefreshAll_preservesCacheOnTransientError(t *testing.T) {
	node := &stubConn{
		nodeID:   "flaky",
		sessions: []map[string]any{{"session_id": "s1"}, {"session_id": "s2"}},
		projects: []map[string]any{{"name": "proj1"}},
		disc:     []map[string]any{{"pid": 100}},
	}
	cm := NewCacheManager(
		func() map[string]Conn { return map[string]Conn{"flaky": node} },
		nil,
	)

	// First refresh succeeds and populates the cache.
	cm.RefreshAll()

	// Next refresh: FetchSessions returns a transient error.
	node.sessErr = errors.New("connection refused")
	cm.RefreshAll()

	sessions, status := cm.Sessions()
	if status["flaky"] != "error" {
		t.Errorf("expected status 'error', got %q", status["flaky"])
	}
	if len(sessions["flaky"]) != 2 {
		t.Errorf("expected 2 preserved sessions on transient error, got %d", len(sessions["flaky"]))
	}
	if got := cm.Projects()["flaky"]; len(got) != 1 {
		t.Errorf("expected 1 preserved project on transient error, got %d", len(got))
	}
	if got := cm.Discovered()["flaky"]; len(got) != 1 {
		t.Errorf("expected 1 preserved discovered on transient error, got %d", len(got))
	}

	// Recovery: error clears and fresh data replaces the preserved snapshot.
	node.sessErr = nil
	node.sessions = []map[string]any{{"session_id": "s3"}}
	cm.RefreshAll()
	sessions, status = cm.Sessions()
	if status["flaky"] != "ok" {
		t.Errorf("expected status 'ok' after recovery, got %q", status["flaky"])
	}
	if len(sessions["flaky"]) != 1 {
		t.Errorf("expected 1 fresh session after recovery, got %d", len(sessions["flaky"]))
	}
}

// ---- RefreshAll preserves projects/discovered when only those RPCs fail (#1993) ----

func TestCacheManager_RefreshAll_preservesProjectsOnPartialError(t *testing.T) {
	node := &stubConn{
		nodeID:   "partial",
		sessions: []map[string]any{{"session_id": "s1"}},
		projects: []map[string]any{{"name": "proj1"}},
		disc:     []map[string]any{{"pid": 100}},
	}
	cm := NewCacheManager(
		func() map[string]Conn { return map[string]Conn{"partial": node} },
		nil,
	)

	// First refresh succeeds and populates the cache.
	cm.RefreshAll()

	// Next refresh: sessions succeed but projects/discovered RPCs fail
	// independently. The node must NOT be blanked — it should keep its
	// last-known projects/discovered (regression for #1993).
	node.projErr = errors.New("projects 500")
	node.discErr = errors.New("discovered timeout")
	node.projects = nil
	node.disc = nil
	cm.RefreshAll()

	_, status := cm.Sessions()
	if status["partial"] != "ok" {
		t.Errorf("expected status 'ok' (sessions succeeded), got %q", status["partial"])
	}
	if got := cm.Projects()["partial"]; len(got) != 1 {
		t.Errorf("expected 1 preserved project on partial error, got %d", len(got))
	}
	if got := cm.Discovered()["partial"]; len(got) != 1 {
		t.Errorf("expected 1 preserved discovered on partial error, got %d", len(got))
	}

	// Recovery: errors clear and fresh data replaces the preserved snapshot.
	node.projErr = nil
	node.discErr = nil
	node.projects = []map[string]any{{"name": "proj1"}, {"name": "proj2"}}
	node.disc = []map[string]any{{"pid": 100}, {"pid": 200}}
	cm.RefreshAll()
	if got := cm.Projects()["partial"]; len(got) != 2 {
		t.Errorf("expected 2 fresh projects after recovery, got %d", len(got))
	}
	if got := cm.Discovered()["partial"]; len(got) != 2 {
		t.Errorf("expected 2 fresh discovered after recovery, got %d", len(got))
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

// ---- Copy-on-write: getters return immutable published maps (#2230) ----

// TestCacheManager_Getters_ImmutableAfterWrite asserts a map reference handed
// out by a getter is never mutated in place by a subsequent RefreshFor /
// PurgeNode. This is what lets the getters skip the per-call defensive copy.
func TestCacheManager_Getters_ImmutableAfterWrite(t *testing.T) {
	node := &stubConn{
		nodeID:   "node-z",
		sessions: []map[string]any{{"session_id": "first"}},
	}
	cm := NewCacheManager(
		func() map[string]Conn { return map[string]Conn{"node-z": node} },
		nil,
	)
	cm.RefreshFor("node-z")

	// Capture the published reference.
	sessions, status := cm.Sessions()
	if len(sessions["node-z"]) != 1 {
		t.Fatalf("setup: expected 1 session, got %d", len(sessions["node-z"]))
	}
	if status["node-z"] != "ok" {
		t.Fatalf("setup: expected status ok, got %q", status["node-z"])
	}

	// A subsequent write must not mutate the captured snapshot.
	node.sessions = []map[string]any{{"session_id": "second"}, {"session_id": "third"}}
	cm.RefreshFor("node-z")
	if len(sessions["node-z"]) != 1 {
		t.Errorf("captured sessions map mutated in place: now %d entries", len(sessions["node-z"]))
	}

	// PurgeNode must not delete from the captured snapshot.
	cm.PurgeNode("node-z")
	if _, ok := sessions["node-z"]; !ok {
		t.Error("captured sessions map had node-z deleted in place by PurgeNode")
	}
	if status["node-z"] != "ok" {
		t.Errorf("captured status map mutated in place by PurgeNode: %q", status["node-z"])
	}

	// And the live view reflects the purge.
	live, _ := cm.Sessions()
	if _, ok := live["node-z"]; ok {
		t.Error("live sessions still has node-z after PurgeNode")
	}
}

// TestCacheManager_ConcurrentReadWrite stresses readers (getters + iteration)
// against writers (RefreshFor/PurgeNode) to catch concurrent map access under
// -race. The getters return live references, so iteration must stay race-free.
func TestCacheManager_ConcurrentReadWrite(t *testing.T) {
	node := &stubConn{
		nodeID:   "node-c",
		sessions: []map[string]any{{"session_id": "s"}},
		projects: []map[string]any{{"name": "p"}},
		disc:     []map[string]any{{"session_id": "d"}},
	}
	cm := NewCacheManager(
		func() map[string]Conn { return map[string]Conn{"node-c": node} },
		nil,
	)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					cm.RefreshFor("node-c")
					cm.PurgeNode("node-c")
				}
			}
		}()
	}
	// Readers iterating the returned maps (would race if maps mutated in place).
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					sessions, status := cm.Sessions()
					for k, v := range sessions {
						_ = k
						_ = len(v)
					}
					for range status {
					}
					for range cm.Projects() {
					}
					for range cm.Discovered() {
					}
				}
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
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

// ---- Sessions returns a read-only live view; RefreshAll publishes a fresh
// map (copy-on-write), so a previously-returned snapshot is never mutated by a
// later refresh. #2230 changed the getter from a defensive copy to a live view;
// the invariant that matters is "published maps are immutable", verified here
// (and in TestCacheManager_Getters_ImmutableAfterWrite for RefreshFor/Purge). ----

func TestCacheManager_Sessions_publishesFreshMapOnRefresh(t *testing.T) {
	node := &stubConn{
		nodeID:   "n1",
		sessions: []map[string]any{{"session_id": "s1"}},
	}
	cm := NewCacheManager(
		func() map[string]Conn { return map[string]Conn{"n1": node} },
		nil,
	)
	cm.RefreshAll()

	// Capture the published snapshot, then trigger another publish.
	sessions1, _ := cm.Sessions()
	node.sessions = []map[string]any{{"session_id": "s1"}, {"session_id": "s2"}}
	cm.RefreshAll()

	// The earlier snapshot must be unaffected by the new publish.
	if got := len(sessions1["n1"]); got != 1 {
		t.Errorf("earlier Sessions() snapshot mutated by RefreshAll: %d entries", got)
	}
	sessions2, _ := cm.Sessions()
	if got := len(sessions2["n1"]); got != 2 {
		t.Errorf("new Sessions() snapshot should reflect refresh: %d entries", got)
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
