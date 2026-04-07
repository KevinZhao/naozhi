package session

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// ---------------------------------------------------------------------------
// fakeProcess — test-only processIface implementation
// ---------------------------------------------------------------------------

type fakeProcess struct {
	mu        sync.Mutex
	isAlive   bool
	isRunning bool
	closeOnce sync.Once
	entries   []cli.EventEntry // returned by EventEntries
}

func newIdleProc() *fakeProcess {
	return &fakeProcess{isAlive: true, isRunning: false}
}

func newRunningProc() *fakeProcess {
	return &fakeProcess{isAlive: true, isRunning: true}
}

func newDeadProc() *fakeProcess {
	return &fakeProcess{isAlive: false, isRunning: false}
}

func newDeadProcWithEntries(entries []cli.EventEntry) *fakeProcess {
	return &fakeProcess{isAlive: false, entries: entries}
}

func (f *fakeProcess) Alive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.isAlive
}

func (f *fakeProcess) IsRunning() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.isRunning
}

func (f *fakeProcess) Close() {
	f.closeOnce.Do(func() {
		f.mu.Lock()
		f.isAlive = false
		f.isRunning = false
		f.mu.Unlock()
	})
}

func (f *fakeProcess) Send(_ context.Context, _ string, _ []cli.ImageData, _ cli.EventCallback) (*cli.SendResult, error) {
	return &cli.SendResult{Text: "fake"}, nil
}

func (f *fakeProcess) GetState() cli.ProcessState {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.isAlive {
		return cli.StateDead
	}
	if f.isRunning {
		return cli.StateRunning
	}
	return cli.StateReady
}

func (f *fakeProcess) TotalCost() float64 { return 0 }
func (f *fakeProcess) EventEntries() []cli.EventEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) == 0 {
		return nil
	}
	cp := make([]cli.EventEntry, len(f.entries))
	copy(cp, f.entries)
	return cp
}
func (f *fakeProcess) EventEntriesSince(afterMS int64) []cli.EventEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, e := range f.entries {
		if e.Time > afterMS {
			cp := make([]cli.EventEntry, len(f.entries)-i)
			copy(cp, f.entries[i:])
			return cp
		}
	}
	return nil
}
func (f *fakeProcess) ProtocolName() string             { return "test" }
func (f *fakeProcess) GetSessionID() string             { return "" }
func (f *fakeProcess) Interrupt()                       {}
func (f *fakeProcess) PID() int                         { return 0 }
func (f *fakeProcess) InjectHistory(_ []cli.EventEntry) {}
func (f *fakeProcess) TurnAgents() []cli.SubagentInfo   { return nil }
func (f *fakeProcess) SubscribeEvents() (<-chan struct{}, func()) {
	ch := make(chan struct{})
	return ch, func() {}
}

// setRunning safely changes the running state (used in shutdown tests).
func (f *fakeProcess) setRunning(v bool) {
	f.mu.Lock()
	f.isRunning = v
	f.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestRouter creates a Router with a failing wrapper so Spawn always errors.
func newTestRouter(maxProcs int) *Router {
	return &Router{
		sessions: make(map[string]*ManagedSession),
		wrapper:  cli.NewWrapper("/nonexistent/cli-binary", &cli.ClaudeProtocol{}, "claude"),
		maxProcs: maxProcs,
		ttl:      30 * time.Minute,
	}
}

// injectSession inserts a fake session directly into the router's session map.
// Must be called before any concurrent operations on the router.
func injectSession(r *Router, key string, proc processIface) *ManagedSession {
	s := &ManagedSession{
		Key:     key,
		process: proc,
	}
	s.touchLastActive()
	r.sessions[key] = s
	if !s.Exempt && proc != nil && proc.Alive() {
		r.activeCount++
	}
	return s
}

// newSessionWithID creates a ManagedSession with the given key and session ID.
func newSessionWithID(key, sessID string) *ManagedSession {
	s := &ManagedSession{Key: key}
	s.setSessionID(sessID)
	return s
}

// ---------------------------------------------------------------------------
// NewRouter
// ---------------------------------------------------------------------------

func TestNewRouterDefaults(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 0, TTL: 0})
	if r.maxProcs != 3 {
		t.Errorf("maxProcs = %d, want 3", r.maxProcs)
	}
	if r.ttl != 30*time.Minute {
		t.Errorf("ttl = %v, want 30m", r.ttl)
	}
}

func TestRouterDefaultWorkspaceAndMaxProcs(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 7, Workspace: "/my/workspace"})
	if got := r.DefaultWorkspace(); got != "/my/workspace" {
		t.Errorf("DefaultWorkspace() = %q, want /my/workspace", got)
	}
	if got := r.MaxProcs(); got != 7 {
		t.Errorf("MaxProcs() = %d, want 7", got)
	}
}

func TestNewRouterStoreRestore(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")

	saved := map[string]*ManagedSession{
		"feishu:direct:alice:general": newSessionWithID("feishu:direct:alice:general", "sess-111"),
		"feishu:direct:bob:general":   newSessionWithID("feishu:direct:bob:general", "sess-222"),
	}
	if err := saveStore(storePath, saved); err != nil {
		t.Fatalf("saveStore: %v", err)
	}

	r := NewRouter(RouterConfig{MaxProcs: 3, StorePath: storePath})

	active, total := r.Stats()
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if active != 0 {
		t.Errorf("active = %d, want 0 (no live processes)", active)
	}

	r.mu.Lock()
	s1 := r.sessions["feishu:direct:alice:general"]
	r.mu.Unlock()
	if s1 == nil || s1.getSessionID() != "sess-111" {
		t.Errorf("alice session not restored: %+v", s1)
	}
}

func TestNewRouterNoStore(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 3, StorePath: ""})
	_, total := r.Stats()
	if total != 0 {
		t.Errorf("total = %d, want 0 when no store", total)
	}
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

func TestStatsEmpty(t *testing.T) {
	r := newTestRouter(3)
	active, total := r.Stats()
	if active != 0 || total != 0 {
		t.Errorf("Stats() = (%d, %d), want (0, 0)", active, total)
	}
}

func TestStatsWithAliveSessions(t *testing.T) {
	r := newTestRouter(3)
	injectSession(r, "key1", newIdleProc())
	injectSession(r, "key2", newIdleProc())

	active, total := r.Stats()
	if active != 2 || total != 2 {
		t.Errorf("Stats() = (%d, %d), want (2, 2)", active, total)
	}
}

func TestStatsWithDeadSession(t *testing.T) {
	r := newTestRouter(3)
	injectSession(r, "key1", newDeadProc())

	active, total := r.Stats()
	if active != 0 || total != 1 {
		t.Errorf("Stats() = (%d, %d), want (0, 1)", active, total)
	}
}

func TestStatsNilProcessSession(t *testing.T) {
	r := newTestRouter(3)
	// Simulates a session restored from store (no live process yet).
	r.sessions["restored-key"] = newSessionWithID("restored-key", "sess-restore")

	active, total := r.Stats()
	if active != 0 || total != 1 {
		t.Errorf("Stats() = (%d, %d), want (0, 1)", active, total)
	}
}

func TestStatsMixedSessions(t *testing.T) {
	r := newTestRouter(5)
	injectSession(r, "alive1", newIdleProc())
	injectSession(r, "alive2", newRunningProc())
	injectSession(r, "dead1", newDeadProc())
	r.sessions["restored"] = newSessionWithID("restored", "sess-x")

	active, total := r.Stats()
	if active != 2 || total != 4 {
		t.Errorf("Stats() = (%d, %d), want (2, 4)", active, total)
	}
}

// ---------------------------------------------------------------------------
// Reset
// ---------------------------------------------------------------------------

func TestResetNonExistentKey(t *testing.T) {
	r := newTestRouter(3)
	r.Reset("no-such-key") // must not panic
	_, total := r.Stats()
	if total != 0 {
		t.Errorf("total after no-op reset = %d, want 0", total)
	}
}

func TestResetNilProcessSession(t *testing.T) {
	r := newTestRouter(3)
	r.sessions["key1"] = &ManagedSession{Key: "key1"}
	r.sessions["key1"].setSessionID("sess-1")

	r.Reset("key1")

	_, total := r.Stats()
	if total != 0 {
		t.Errorf("total after reset = %d, want 0", total)
	}
}

func TestResetAliveSession(t *testing.T) {
	r := newTestRouter(3)
	proc := newIdleProc()
	injectSession(r, "key1", proc)

	r.Reset("key1")

	if proc.Alive() {
		t.Error("process should be closed after Reset")
	}
	_, total := r.Stats()
	if total != 0 {
		t.Errorf("total after reset = %d, want 0", total)
	}
}

func TestResetRunningSession(t *testing.T) {
	r := newTestRouter(3)
	proc := newRunningProc()
	injectSession(r, "key1", proc)

	r.Reset("key1") // Reset closes even running sessions

	if proc.Alive() {
		t.Error("running process should be closed after Reset")
	}
}

// ---------------------------------------------------------------------------
// Cleanup
// ---------------------------------------------------------------------------

func TestCleanupNoExpiredSessions(t *testing.T) {
	r := &Router{
		sessions: make(map[string]*ManagedSession),
		maxProcs: 3,
		ttl:      1 * time.Hour,
	}
	proc := newIdleProc()
	s := injectSession(r, "key1", proc)
	s.touchLastActive() // just touched

	r.Cleanup()

	if !proc.Alive() {
		t.Error("non-expired session should not be closed")
	}
}

func TestCleanupExpiredSession(t *testing.T) {
	r := &Router{
		sessions: make(map[string]*ManagedSession),
		maxProcs: 3,
		ttl:      1 * time.Minute,
	}
	proc := newIdleProc()
	s := injectSession(r, "key1", proc)
	s.lastActive.Store(time.Now().Add(-2 * time.Hour).UnixNano())

	r.Cleanup()

	if proc.Alive() {
		t.Error("expired session process should be closed")
	}
}

func TestCleanupSkipsRunningSession(t *testing.T) {
	r := &Router{
		sessions: make(map[string]*ManagedSession),
		maxProcs: 3,
		ttl:      1 * time.Minute,
	}
	proc := newRunningProc()
	s := injectSession(r, "key1", proc)
	s.lastActive.Store(time.Now().Add(-2 * time.Hour).UnixNano())

	r.Cleanup()

	if !proc.Alive() {
		t.Error("running session should not be cleaned up even if idle time exceeds TTL")
	}
}

func TestCleanupSkipsNilProcess(t *testing.T) {
	r := &Router{
		sessions: make(map[string]*ManagedSession),
		maxProcs: 3,
		ttl:      1 * time.Minute,
	}
	s := &ManagedSession{Key: "key1"}
	s.setSessionID("sess-1")
	s.lastActive.Store(time.Now().UnixNano()) // recent → within 7*TTL window
	r.sessions["key1"] = s

	r.Cleanup() // must not panic

	_, total := r.Stats()
	if total != 1 {
		t.Errorf("nil-process session should remain in map after cleanup, total = %d", total)
	}
}

func TestCleanupSkipsDeadProcess(t *testing.T) {
	r := &Router{
		sessions: make(map[string]*ManagedSession),
		maxProcs: 3,
		ttl:      1 * time.Minute,
	}
	proc := newDeadProc()
	s := injectSession(r, "key1", proc)
	s.lastActive.Store(time.Now().Add(-5 * time.Minute).UnixNano())
	s.setSessionID("resumable-sess") // has session ID → kept for resumption

	r.Cleanup() // dead process with session ID — kept within 7*TTL

	_, total := r.Stats()
	if total != 1 {
		t.Errorf("dead session with session ID should remain in map after cleanup, total = %d", total)
	}
}

func TestCleanupMultipleSessions(t *testing.T) {
	r := &Router{
		sessions: make(map[string]*ManagedSession),
		maxProcs: 5,
		ttl:      1 * time.Minute,
	}
	expiredProc := newIdleProc()
	freshProc := newIdleProc()

	expiredSession := injectSession(r, "expired", expiredProc)
	expiredSession.lastActive.Store(time.Now().Add(-2 * time.Hour).UnixNano())

	freshSession := injectSession(r, "fresh", freshProc)
	freshSession.touchLastActive()

	r.Cleanup()

	if expiredProc.Alive() {
		t.Error("expired process should be closed")
	}
	if !freshProc.Alive() {
		t.Error("fresh process should not be closed")
	}
}

// ---------------------------------------------------------------------------
// GetOrCreate
// ---------------------------------------------------------------------------

func TestGetOrCreate_ExistingAliveSession(t *testing.T) {
	r := newTestRouter(3)
	proc := newIdleProc()
	injected := injectSession(r, "feishu:direct:user1:general", proc)
	injected.setSessionID("existing-sess")

	s, _, err := r.GetOrCreate(context.Background(), "feishu:direct:user1:general", AgentOpts{})
	if err != nil {
		t.Fatalf("GetOrCreate error: %v", err)
	}
	if s != injected {
		t.Error("should return existing session, not spawn a new one")
	}
	if s.getSessionID() != "existing-sess" {
		t.Errorf("SessionID = %q, want 'existing-sess'", s.getSessionID())
	}
}

func TestGetOrCreate_ExistingAlive_TouchesLastActive(t *testing.T) {
	r := newTestRouter(3)
	proc := newIdleProc()
	s := injectSession(r, "key1", proc)
	oldActive := s.GetLastActive()
	time.Sleep(2 * time.Millisecond)

	_, _, err := r.GetOrCreate(context.Background(), "key1", AgentOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !s.GetLastActive().After(oldActive) {
		t.Error("GetOrCreate should update lastActive for existing alive session")
	}
}

func TestGetOrCreate_NewSession_SpawnError(t *testing.T) {
	r := newTestRouter(3)
	_, _, err := r.GetOrCreate(context.Background(), "feishu:direct:user1:general", AgentOpts{})
	if err == nil {
		t.Fatal("expected error from spawn with nonexistent CLI")
	}
	if !strings.Contains(err.Error(), "spawn") {
		t.Errorf("error should mention spawn: %v", err)
	}
}

func TestGetOrCreate_DeadSession_AttemptResume(t *testing.T) {
	r := newTestRouter(3)
	s := newSessionWithID("feishu:direct:user1:general", "old-sess-id")
	s.process = newDeadProc()
	r.sessions["feishu:direct:user1:general"] = s

	_, _, err := r.GetOrCreate(context.Background(), "feishu:direct:user1:general", AgentOpts{})
	if err == nil {
		t.Fatal("expected error (spawn fails with nonexistent CLI)")
	}
	if !strings.Contains(err.Error(), "spawn") {
		t.Errorf("expected spawn error, got: %v", err)
	}
}

func TestGetOrCreate_NilProcessSession_AttemptSpawn(t *testing.T) {
	r := newTestRouter(3)
	// Restored session with no process (like after restart).
	r.sessions["key1"] = newSessionWithID("key1", "restored-sess")

	_, _, err := r.GetOrCreate(context.Background(), "key1", AgentOpts{})
	if err == nil {
		t.Fatal("expected spawn error")
	}
	if !strings.Contains(err.Error(), "spawn") {
		t.Errorf("expected spawn error, got: %v", err)
	}
}

func TestGetOrCreate_AgentOptsOverride(t *testing.T) {
	r := &Router{
		sessions:  make(map[string]*ManagedSession),
		wrapper:   cli.NewWrapper("/nonexistent/cli-binary", &cli.ClaudeProtocol{}, "claude"),
		maxProcs:  3,
		ttl:       30 * time.Minute,
		model:     "default-model",
		extraArgs: []string{"--base-arg"},
	}

	// Even with overrides, spawn will still fail — we just verify no panic.
	_, _, err := r.GetOrCreate(context.Background(), "key1", AgentOpts{
		Model:     "override-model",
		ExtraArgs: []string{"--extra"},
	})
	if err == nil {
		t.Fatal("expected spawn error from nonexistent CLI")
	}
}

// ---------------------------------------------------------------------------
// maxProcs enforcement
// ---------------------------------------------------------------------------

func TestMaxProcs_AllRunning_ReturnsError(t *testing.T) {
	r := newTestRouter(2)
	for i := 0; i < 2; i++ {
		injectSession(r, fmt.Sprintf("key%d", i), newRunningProc())
	}

	_, _, err := r.GetOrCreate(context.Background(), "new-key", AgentOpts{})
	if err == nil {
		t.Fatal("expected error when max procs reached and all busy")
	}
	if !strings.Contains(err.Error(), "max concurrent processes reached") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestMaxProcs_EvictsIdleThenSpawnFails(t *testing.T) {
	r := newTestRouter(1)
	oldProc := newIdleProc()
	s := injectSession(r, "old-key", oldProc)
	s.lastActive.Store(time.Now().Add(-1 * time.Hour).UnixNano())

	_, _, err := r.GetOrCreate(context.Background(), "new-key", AgentOpts{})
	// Spawn fails (nonexistent CLI), but eviction should have happened first.
	if err == nil {
		// If a CLI happened to be installed on this machine, just skip.
		t.Log("spawn succeeded unexpectedly; skipping eviction check")
		return
	}
	if !strings.Contains(err.Error(), "spawn") {
		t.Errorf("expected spawn error after eviction, got: %v", err)
	}
	if oldProc.Alive() {
		t.Error("old process should have been evicted and closed")
	}
}

func TestMaxProcs_EvictFailsWhenAllRunning(t *testing.T) {
	r := newTestRouter(1)
	injectSession(r, "running-key", newRunningProc())

	_, _, err := r.GetOrCreate(context.Background(), "new-key", AgentOpts{})
	if err == nil {
		t.Fatal("expected error: max procs with all running")
	}
	if !strings.Contains(err.Error(), "max concurrent processes reached") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// evictOldest (called with r.mu held)
// ---------------------------------------------------------------------------

func TestEvictOldestEmptyRouter(t *testing.T) {
	r := &Router{sessions: make(map[string]*ManagedSession)}

	r.mu.Lock()
	evicted := r.evictOldest()
	r.mu.Unlock()

	if evicted {
		t.Error("evictOldest should return false for empty router")
	}
}

func TestEvictOldestReturnsTrue(t *testing.T) {
	r := &Router{sessions: make(map[string]*ManagedSession), maxProcs: 1}
	proc := newIdleProc()
	s := &ManagedSession{Key: "key1", process: proc}
	s.touchLastActive()
	r.sessions["key1"] = s

	r.mu.Lock()
	evicted := r.evictOldest()
	r.mu.Unlock()

	if !evicted {
		t.Error("evictOldest should return true when an idle session exists")
	}
	if proc.Alive() {
		t.Error("evicted process should be closed")
	}
}

func TestEvictOldestSkipsRunning(t *testing.T) {
	r := &Router{sessions: make(map[string]*ManagedSession)}
	proc := newRunningProc()
	s := &ManagedSession{Key: "key1", process: proc}
	s.touchLastActive()
	r.sessions["key1"] = s

	r.mu.Lock()
	evicted := r.evictOldest()
	r.mu.Unlock()

	if evicted {
		t.Error("evictOldest should skip running sessions")
	}
	if !proc.Alive() {
		t.Error("running process should not be closed")
	}
}

func TestEvictOldestSkipsDead(t *testing.T) {
	r := &Router{sessions: make(map[string]*ManagedSession)}
	proc := newDeadProc()
	s := &ManagedSession{Key: "key1", process: proc}
	s.touchLastActive()
	r.sessions["key1"] = s

	r.mu.Lock()
	evicted := r.evictOldest()
	r.mu.Unlock()

	if evicted {
		t.Error("evictOldest should skip dead sessions")
	}
}

func TestEvictOldestPicksOldest(t *testing.T) {
	r := &Router{sessions: make(map[string]*ManagedSession)}

	oldProc := newIdleProc()
	recentProc := newIdleProc()

	oldSession := &ManagedSession{Key: "old-key", process: oldProc}
	oldSession.lastActive.Store(time.Now().Add(-2 * time.Hour).UnixNano())

	recentSession := &ManagedSession{Key: "recent-key", process: recentProc}
	recentSession.lastActive.Store(time.Now().Add(-1 * time.Minute).UnixNano())

	r.sessions["old-key"] = oldSession
	r.sessions["recent-key"] = recentSession

	r.mu.Lock()
	evicted := r.evictOldest()
	r.mu.Unlock()

	if !evicted {
		t.Fatal("expected eviction to succeed")
	}
	if oldProc.Alive() {
		t.Error("oldest process should be closed")
	}
	if !recentProc.Alive() {
		t.Error("most recent process should remain alive")
	}
}

func TestEvictOldestSkipsNilProcess(t *testing.T) {
	r := &Router{sessions: make(map[string]*ManagedSession)}
	r.sessions["nil-key"] = newSessionWithID("nil-key", "sess-1")

	r.mu.Lock()
	evicted := r.evictOldest()
	r.mu.Unlock()

	if evicted {
		t.Error("evictOldest should skip sessions with nil process")
	}
}

// ---------------------------------------------------------------------------
// Shutdown
// ---------------------------------------------------------------------------

func TestShutdownEmpty(t *testing.T) {
	r := newTestRouter(3)
	r.Shutdown() // must not panic or block
}

func TestShutdownClosesIdleSessions(t *testing.T) {
	r := newTestRouter(3)
	proc := newIdleProc()
	injectSession(r, "key1", proc)

	r.Shutdown()

	if proc.Alive() {
		t.Error("process should be closed after shutdown")
	}
}

func TestShutdownClosesMultipleSessions(t *testing.T) {
	r := newTestRouter(3)
	procs := []*fakeProcess{newIdleProc(), newIdleProc(), newIdleProc()}
	for i, p := range procs {
		injectSession(r, fmt.Sprintf("key%d", i), p)
	}

	r.Shutdown()

	for i, p := range procs {
		if p.Alive() {
			t.Errorf("procs[%d] should be closed after shutdown", i)
		}
	}
}

func TestShutdownSavesStore(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")

	r := &Router{
		sessions:  make(map[string]*ManagedSession),
		maxProcs:  3,
		ttl:       30 * time.Minute,
		storePath: storePath,
	}
	r.sessions["feishu:direct:user1:general"] = newSessionWithID("feishu:direct:user1:general", "sess-abc")

	r.Shutdown()

	loaded := loadStore(storePath)
	if loaded == nil {
		t.Fatal("store should have been saved after shutdown")
	}
	if loaded["feishu:direct:user1:general"].SessionID != "sess-abc" {
		t.Errorf("session not found in saved store: %v", loaded)
	}
}

func TestShutdownSkipsDeadProcesses(t *testing.T) {
	r := newTestRouter(3)
	proc := newDeadProc()
	injectSession(r, "key1", proc)

	r.Shutdown() // must not call Close on already-dead process (Close is idempotent anyway)
}

func TestShutdownWaitsForRunningThenProceeds(t *testing.T) {
	r := newTestRouter(3)
	proc := newRunningProc()
	injectSession(r, "key1", proc)

	// Transition the process to idle after a short delay.
	go func() {
		time.Sleep(120 * time.Millisecond)
		proc.setRunning(false)
	}()

	done := make(chan struct{})
	go func() {
		r.Shutdown()
		close(done)
	}()

	select {
	case <-done:
		if proc.Alive() {
			t.Error("process should be closed after shutdown")
		}
	case <-time.After(5 * time.Second):
		t.Error("Shutdown timed out waiting for running session")
	}
}

// ---------------------------------------------------------------------------
// countActive
// ---------------------------------------------------------------------------

func TestCountActive_ReflectsAliveProcesses(t *testing.T) {
	r := &Router{sessions: make(map[string]*ManagedSession)}
	injectSession(r, "alive1", newIdleProc())
	injectSession(r, "alive2", newRunningProc())
	injectSession(r, "dead1", newDeadProc())

	r.mu.Lock()
	r.countActive()
	r.mu.Unlock()

	if r.activeCount != 2 {
		t.Errorf("activeCount = %d, want 2", r.activeCount)
	}
}

// ---------------------------------------------------------------------------
// Concurrency / race detector
// ---------------------------------------------------------------------------

func TestConcurrentGetOrCreate_SameKey_Race(t *testing.T) {
	r := newTestRouter(5)
	key := "feishu:direct:user1:general"

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = r.GetOrCreate(context.Background(), key, AgentOpts{})
		}()
	}
	wg.Wait()
}

func TestConcurrentGetOrCreate_DifferentKeys_Race(t *testing.T) {
	r := newTestRouter(10)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		key := fmt.Sprintf("feishu:direct:user%d:general", i)
		go func(k string) {
			defer wg.Done()
			_, _, _ = r.GetOrCreate(context.Background(), k, AgentOpts{})
		}(key)
	}
	wg.Wait()
}

func TestConcurrentReset_Race(t *testing.T) {
	r := newTestRouter(5)
	for i := 0; i < 5; i++ {
		injectSession(r, fmt.Sprintf("key%d", i), newIdleProc())
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		key := fmt.Sprintf("key%d", i)
		go func(k string) {
			defer wg.Done()
			r.Reset(k)
		}(key)
	}
	wg.Wait()
}

func TestConcurrentStats_Race(t *testing.T) {
	r := newTestRouter(3)
	injectSession(r, "key1", newIdleProc())
	injectSession(r, "key2", newIdleProc())

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Stats()
		}()
	}
	wg.Wait()
}

func TestConcurrentCleanup_Race(t *testing.T) {
	r := &Router{
		sessions: make(map[string]*ManagedSession),
		maxProcs: 5,
		ttl:      1 * time.Millisecond, // very short so sessions expire quickly
	}
	for i := 0; i < 5; i++ {
		s := injectSession(r, fmt.Sprintf("key%d", i), newIdleProc())
		s.lastActive.Store(time.Now().Add(-1 * time.Second).UnixNano())
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Cleanup()
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// StartCleanupLoop
// ---------------------------------------------------------------------------

func TestStartCleanupLoop_TriggersCleanup(t *testing.T) {
	r := &Router{
		sessions: make(map[string]*ManagedSession),
		maxProcs: 3,
		ttl:      1 * time.Millisecond,
	}
	proc := newIdleProc()
	s := injectSession(r, "key1", proc)
	s.lastActive.Store(time.Now().Add(-1 * time.Second).UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r.StartCleanupLoop(ctx, 20*time.Millisecond)

	// Wait for at least one cleanup cycle.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !proc.Alive() {
			return // cleanup fired and closed the expired session
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("cleanup loop did not close expired session within 500ms")
}

func TestStartCleanupLoop_StopsOnContextCancel(t *testing.T) {
	r := newTestRouter(3)

	ctx, cancel := context.WithCancel(context.Background())
	r.StartCleanupLoop(ctx, 10*time.Millisecond)

	cancel() // cancelling the context should stop the goroutine (no hang)
	time.Sleep(50 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// History capture in spawnSession
// ---------------------------------------------------------------------------

// captureHistoryFrom simulates the history-collection branch in spawnSession:
// prefer dead process EventEntries (includes live events) over persistedHistory.
func captureHistoryFrom(s *ManagedSession) []cli.EventEntry {
	var captured []cli.EventEntry
	s.sendMu.Lock()
	if s.process != nil && !s.process.Alive() {
		captured = s.process.EventEntries()
	} else if len(s.persistedHistory) > 0 {
		captured = make([]cli.EventEntry, len(s.persistedHistory))
		copy(captured, s.persistedHistory)
	}
	s.sendMu.Unlock()
	return captured
}

// TestHistoryCapture_DeadProcessUsesEventEntries verifies that the
// spawnSession history-collection logic prefers process.EventEntries() over
// persistedHistory when the old process is dead. This ensures live events
// accumulated since the last JSONL load are preserved across process restarts.
func TestHistoryCapture_DeadProcessUsesEventEntries(t *testing.T) {
	liveEntries := []cli.EventEntry{
		{Time: 1000, Type: "user", Summary: "first message"},
		{Time: 2000, Type: "text", Summary: "first reply"},
		{Time: 3000, Type: "user", Summary: "second message (live)"},
		{Time: 4000, Type: "text", Summary: "second reply (live)"},
	}
	stalePersisted := []cli.EventEntry{
		{Time: 1000, Type: "user", Summary: "first message"},
		{Time: 2000, Type: "text", Summary: "first reply"},
	}

	proc := newDeadProcWithEntries(liveEntries)
	s := &ManagedSession{
		Key:              "test-key",
		process:          proc,
		persistedHistory: stalePersisted,
	}

	captured := captureHistoryFrom(s)

	if len(captured) != len(liveEntries) {
		t.Fatalf("captured %d entries, want %d", len(captured), len(liveEntries))
	}
	// Verify we got the live entries (not the stale persisted ones).
	if captured[len(captured)-1].Summary != "second reply (live)" {
		t.Errorf("last entry = %q, want 'second reply (live)'", captured[len(captured)-1].Summary)
	}
}

// TestHistoryCapture_NilProcessFallsBackToPersistedHistory verifies that when
// the old session has no process (service-restart scenario), persistedHistory
// is used as the history source, with JSONL reload as a further fallback.
func TestHistoryCapture_NilProcessFallsBackToPersistedHistory(t *testing.T) {
	persisted := []cli.EventEntry{
		{Time: 1000, Type: "user", Summary: "startup-loaded entry"},
	}

	s := &ManagedSession{
		Key:              "test-key",
		process:          nil,
		persistedHistory: persisted,
	}

	captured := captureHistoryFrom(s)

	if len(captured) != len(persisted) {
		t.Fatalf("captured %d entries, want %d", len(captured), len(persisted))
	}
	if captured[0].Summary != "startup-loaded entry" {
		t.Errorf("entry summary = %q, want 'startup-loaded entry'", captured[0].Summary)
	}
}

// TestHistoryCapture_EmptyFallsBackToJSONL verifies that when the old session
// has neither a dead process nor persistedHistory, captured history is empty,
// triggering the JSONL-load path in spawnSession.
func TestHistoryCapture_EmptyFallsBackToJSONL(t *testing.T) {
	s := &ManagedSession{
		Key:     "test-key",
		process: nil,
	}

	if captured := captureHistoryFrom(s); len(captured) != 0 {
		t.Errorf("captured %d entries, want 0 (JSONL fallback should be triggered)", len(captured))
	}
}
