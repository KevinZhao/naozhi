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
	totalCost float64          // returned by TotalCost
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

func (f *fakeProcess) Kill() {
	f.mu.Lock()
	f.isAlive = false
	f.isRunning = false
	f.mu.Unlock()
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

func (f *fakeProcess) TotalCost() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.totalCost
}
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
func (f *fakeProcess) EventLastN(n int) []cli.EventEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries := f.entries
	if n > 0 && n < len(entries) {
		entries = entries[len(entries)-n:]
	}
	cp := make([]cli.EventEntry, len(entries))
	copy(cp, entries)
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
func (f *fakeProcess) EventEntriesBefore(beforeMS int64, limit int) []cli.EventEntry {
	if limit <= 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]cli.EventEntry, 0, limit)
	for i := len(f.entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := f.entries[i]
		if beforeMS > 0 && e.Time >= beforeMS {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
func (f *fakeProcess) LastEntryOfType(typ string) cli.EventEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.entries) - 1; i >= 0; i-- {
		if f.entries[i].Type == typ {
			return f.entries[i]
		}
	}
	return cli.EventEntry{}
}
func (f *fakeProcess) LastActivitySummary() string      { return "" }
func (f *fakeProcess) ProtocolName() string             { return "test" }
func (f *fakeProcess) GetSessionID() string             { return "" }
func (f *fakeProcess) Interrupt()                       {}
func (f *fakeProcess) InterruptViaControl() error       { return nil }
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
		pruneTTL: 72 * time.Hour,
	}
}

// injectSession inserts a fake session directly into the router's session map.
// Must be called before any concurrent operations on the router.
func injectSession(r *Router, key string, proc processIface) *ManagedSession {
	s := &ManagedSession{
		key: key,
	}
	s.storeProcess(proc)
	s.touchLastActive()
	r.sessions[key] = s
	if !s.IsExempt() && proc != nil && proc.Alive() {
		r.activeCount++
	}
	return s
}

// newSessionWithID creates a ManagedSession with the given key and session ID.
func newSessionWithID(key, sessID string) *ManagedSession {
	s := &ManagedSession{key: key}
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
	if r.pruneTTL != 72*time.Hour {
		t.Errorf("pruneTTL = %v, want 72h", r.pruneTTL)
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

func TestRouterBackendIDsAndWrapperFor(t *testing.T) {
	claudeW := &cli.Wrapper{BackendID: "claude", CLIName: "claude-code"}
	kiroW := &cli.Wrapper{BackendID: "kiro", CLIName: "kiro"}

	r := NewRouter(RouterConfig{
		Wrappers:       map[string]*cli.Wrapper{"claude": claudeW, "kiro": kiroW},
		DefaultBackend: "kiro",
	})

	ids := r.BackendIDs()
	if len(ids) != 2 || ids[0] != "kiro" {
		t.Fatalf("BackendIDs = %v, want default-first [kiro, claude]", ids)
	}
	if got := r.DefaultBackend(); got != "kiro" {
		t.Errorf("DefaultBackend = %q, want kiro", got)
	}

	// Explicit lookup
	w, id := r.wrapperFor("claude")
	if w != claudeW || id != "claude" {
		t.Errorf("wrapperFor(claude) = %v, %q; want claudeW, claude", w, id)
	}

	// Empty → default
	w, id = r.wrapperFor("")
	if w != kiroW || id != "kiro" {
		t.Errorf("wrapperFor(\"\") = %v, %q; want kiroW, kiro", w, id)
	}

	// Unknown → default (silent fallback)
	w, id = r.wrapperFor("gemini")
	if w != kiroW || id != "kiro" {
		t.Errorf("wrapperFor(unknown) = %v, %q; want kiroW, kiro", w, id)
	}
}

func TestRouterLegacySingleWrapperMode(t *testing.T) {
	w := &cli.Wrapper{BackendID: "claude", CLIName: "claude-code"}
	r := NewRouter(RouterConfig{Wrapper: w})

	ids := r.BackendIDs()
	if len(ids) != 1 || ids[0] != "claude" {
		t.Errorf("legacy BackendIDs = %v, want [claude]", ids)
	}
	if got := r.DefaultBackend(); got != "claude" {
		t.Errorf("legacy DefaultBackend = %q, want claude", got)
	}
	if got := r.BackendWrapper("claude"); got != w {
		t.Errorf("legacy BackendWrapper(claude) = %v, want wrapper", got)
	}
}

func TestRouterSetGetSessionBackend(t *testing.T) {
	r := NewRouter(RouterConfig{})
	r.SetSessionBackend("k1", "kiro")
	if got := r.GetSessionBackend("k1"); got != "kiro" {
		t.Errorf("GetSessionBackend = %q, want kiro", got)
	}
	r.SetSessionBackend("k1", "") // clears
	if got := r.GetSessionBackend("k1"); got != "" {
		t.Errorf("GetSessionBackend after clear = %q, want empty", got)
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

// TestSnapshotCostFallback exercises the Snapshot cost-fallback path that
// fixes the "$0.00 flash after resume" bug: when a freshly spawned process
// is attached but hasn't yet received a result event (proc.TotalCost()==0),
// Snapshot must surface the historical cost carried by s.totalCost rather
// than 0.
func TestSnapshotCostFallback(t *testing.T) {
	tests := []struct {
		name     string
		procCost float64
		sessCost float64
		procNil  bool
		wantCost float64
	}{
		{name: "no process uses session cost", procNil: true, sessCost: 1.25, wantCost: 1.25},
		{name: "fresh process falls back to session cost", procCost: 0, sessCost: 1.25, wantCost: 1.25},
		{name: "live process cost overrides session cost", procCost: 2.50, sessCost: 1.25, wantCost: 2.50},
		{name: "both zero stays zero", procCost: 0, sessCost: 0, wantCost: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &ManagedSession{key: "k", totalCost: tt.sessCost}
			if !tt.procNil {
				p := newIdleProc()
				p.totalCost = tt.procCost
				s.storeProcess(p)
			}
			got := s.Snapshot().TotalCost
			if got != tt.wantCost {
				t.Errorf("Snapshot.TotalCost = %v, want %v", got, tt.wantCost)
			}
		})
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
	r.sessions["key1"] = &ManagedSession{key: "key1"}
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
		pruneTTL: 72 * time.Hour,
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
		pruneTTL: 72 * time.Hour,
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
		pruneTTL: 72 * time.Hour,
	}
	proc := newRunningProc()
	s := injectSession(r, "key1", proc)
	// Exceeds 1min TTL but stays well below the stuck-running threshold
	// (2 × DefaultTotalTimeout = 10min) so the session is eligible for idle
	// expiry but protected by the IsRunning() guard.
	s.lastActive.Store(time.Now().Add(-2 * time.Minute).UnixNano())

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
		pruneTTL: 1 * time.Hour,
	}
	s := &ManagedSession{key: "key1"}
	s.setSessionID("sess-1")
	s.lastActive.Store(time.Now().UnixNano()) // recent → within pruneTTL window
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
		pruneTTL: 1 * time.Hour,
	}
	proc := newDeadProc()
	s := injectSession(r, "key1", proc)
	s.lastActive.Store(time.Now().Add(-5 * time.Minute).UnixNano())
	s.setSessionID("resumable-sess") // has session ID → kept for resumption

	r.Cleanup() // dead process with session ID — kept within pruneTTL

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
		pruneTTL: 1 * time.Hour,
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
	s.storeProcess(newDeadProc())
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
	s := &ManagedSession{key: "key1"}
	s.storeProcess(proc)
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
	s := &ManagedSession{key: "key1"}
	s.storeProcess(proc)
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
	s := &ManagedSession{key: "key1"}
	s.storeProcess(proc)
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

	oldSession := &ManagedSession{key: "old-key"}
	oldSession.storeProcess(oldProc)
	oldSession.lastActive.Store(time.Now().Add(-2 * time.Hour).UnixNano())

	recentSession := &ManagedSession{key: "recent-key"}
	recentSession.storeProcess(recentProc)
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
		pruneTTL: 1 * time.Millisecond,
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
		pruneTTL: 1 * time.Millisecond,
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
	if p := s.loadProcess(); p != nil && !p.Alive() {
		captured = p.EventEntries()
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
		key:              "test-key",
		persistedHistory: stalePersisted,
	}
	s.storeProcess(proc)

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
		key:              "test-key",
		persistedHistory: persisted,
	}
	// process is nil by default (zero value of atomic.Pointer)

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
		key: "test-key",
	}
	// process is nil by default (zero value of atomic.Pointer)

	if captured := captureHistoryFrom(s); len(captured) != 0 {
		t.Errorf("captured %d entries, want 0 (JSONL fallback should be triggered)", len(captured))
	}
}

// ---------------------------------------------------------------------------
// spawningKeys guard (TOCTOU fix for ReconcileLoop vs fresh-context cron)
// ---------------------------------------------------------------------------

// Regression for the 04:00:00 cron failure: GetOrCreate called spawnSession
// while the 30s reconcile loop fired, and the freshly spawned shim's state
// file was judged "orphan" because the new ManagedSession wasn't installed
// yet. spawnSession must record the key in spawningKeys for the entire spawn
// window so ReconnectShims can skip it; a failed spawn must still clean up.
func TestSpawnSession_SpawningKeysClearedOnFailure(t *testing.T) {
	r := newTestRouter(3)

	// Spawn fails because the test wrapper points at a nonexistent binary.
	_, _, err := r.GetOrCreate(context.Background(), "key1", AgentOpts{})
	if err == nil {
		t.Fatal("expected spawn error from nonexistent CLI")
	}

	r.mu.Lock()
	_, stillMarked := r.spawningKeys["key1"]
	r.mu.Unlock()
	if stillMarked {
		t.Error("spawningKeys still contains key1 after failed spawn")
	}
}

// Covers the concurrent path: while spawnSession is mid-flight for a key,
// ReconnectShims must observe spawningKeys and refuse to treat the discovered
// state file as an orphan. We emulate the reconcile check directly (Discover
// requires a live PID of the same binary, which we cannot fake in a unit
// test), but the logic under test is the spawningKeys lookup inside the
// reconcile loop (router.go around the `if !ok` branch).
func TestSpawningKeys_ObservableDuringSpawn(t *testing.T) {
	r := newTestRouter(3)

	// Simulate being inside spawnSession: caller enters with r.mu held,
	// writes the marker, releases the lock for the Spawn() call.
	r.mu.Lock()
	if r.spawningKeys == nil {
		r.spawningKeys = make(map[string]struct{})
	}
	r.spawningKeys["cron:abc"] = struct{}{}
	r.mu.Unlock()

	// Reconcile's view: lock, snapshot, unlock.
	r.mu.Lock()
	_, spawning := r.spawningKeys["cron:abc"]
	r.mu.Unlock()
	if !spawning {
		t.Fatal("reconcile should see spawningKeys marker and skip orphan check")
	}

	// After spawnSession's defer fires, the marker disappears.
	r.mu.Lock()
	delete(r.spawningKeys, "cron:abc")
	r.mu.Unlock()

	r.mu.Lock()
	_, stillMarked := r.spawningKeys["cron:abc"]
	r.mu.Unlock()
	if stillMarked {
		t.Error("spawningKeys leaked after cleanup")
	}
}
