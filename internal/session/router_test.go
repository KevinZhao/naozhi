package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/discovery"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// ---------------------------------------------------------------------------
// fakeProcess — test-only processIface implementation
// ---------------------------------------------------------------------------

type fakeProcess struct {
	mu            sync.Mutex
	isAlive       bool
	isRunning     bool
	closeOnce     sync.Once
	entries       []cli.EventEntry // returned by EventEntries
	totalCost     float64          // returned by TotalCost
	userTurnCount int64            // returned by UserTurnCount (test-only)
	lastEventAt   time.Time        // returned by LastEventAt (test-only)

	// Interrupt instrumentation (used by TestInterruptSessionSafe_*).
	// viaControlErr is what InterruptViaControl() returns. interruptCalls is
	// bumped every time Interrupt() is called. Both writes happen under
	// mu so the test assertions race-free.
	viaControlErr     error
	viaControlCalls   int
	interruptCalls    int
	viaControlRunning bool // if true, InterruptViaControl only succeeds when isRunning is true
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

func (f *fakeProcess) State() cli.ProcessState {
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

func (f *fakeProcess) DeathReason() string { return "" }
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
func (f *fakeProcess) EventLastNVisible(visibleTarget, maxTotal int) []cli.EventEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.entries)
	if n == 0 {
		return nil
	}
	limit := maxTotal
	if limit <= 0 || limit > n {
		limit = n
	}
	visible := 0
	start := n
	for i := n - 1; i >= 0 && (n-i) <= limit; i-- {
		start = i
		if cli.IsVisibleEntry(f.entries[i]) {
			visible++
			if visibleTarget > 0 && visible >= visibleTarget {
				break
			}
		}
	}
	cp := make([]cli.EventEntry, n-start)
	copy(cp, f.entries[start:])
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
func (f *fakeProcess) EventEntriesSinceAppend(dst []cli.EventEntry, afterMS int64) []cli.EventEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, e := range f.entries {
		if e.Time > afterMS {
			return append(dst, f.entries[i:]...)
		}
	}
	// Preserve the "nil when empty" contract for nil dst; append-mode callers
	// passing a pooled dst[:0] get their buffer back length-zero.
	if dst == nil {
		return nil
	}
	return dst[:0]
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
func (f *fakeProcess) LastActivitySummary() string { return "" }
func (f *fakeProcess) LastResponseSummary() string { return "" }
func (f *fakeProcess) LastEventAt() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastEventAt
}
func (f *fakeProcess) UserTurnCount() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.userTurnCount
}
func (f *fakeProcess) ProtocolName() string { return "test" }
func (f *fakeProcess) SessionID() string    { return "" }
func (f *fakeProcess) Interrupt() {
	f.mu.Lock()
	f.interruptCalls++
	f.mu.Unlock()
}
func (f *fakeProcess) InterruptViaControl() error {
	f.mu.Lock()
	f.viaControlCalls++
	// Simulate the real Process.InterruptViaControl semantics: it returns
	// ErrNoActiveTurn when the CLI is not Running. Callers can toggle
	// viaControlRunning to force that branch without touching isRunning
	// (which has other side-effects across the test suite).
	err := f.viaControlErr
	if f.viaControlRunning && !f.isRunning {
		err = cli.ErrNoActiveTurn
	}
	f.mu.Unlock()
	return err
}
func (f *fakeProcess) PID() int                         { return 0 }
func (f *fakeProcess) InjectHistory(_ []cli.EventEntry) {}
func (f *fakeProcess) TurnAgents() []cli.SubagentInfo   { return nil }

// Normalize-layer stubs (multi-backend §8.8) — fakeProcess is used by router
// tests that pre-date multi-backend, so all three return zero values to
// preserve historical SessionSnapshot output.
func (f *fakeProcess) ContextUsagePercent() float64       { return 0 }
func (f *fakeProcess) TurnDurationMs() int64              { return 0 }
func (f *fakeProcess) MeteringUsage() []cli.MeteringEntry { return nil }
func (f *fakeProcess) Model() string                      { return "" }
func (f *fakeProcess) SubscribeEvents() (<-chan struct{}, func()) {
	ch := make(chan struct{})
	return ch, func() {}
}

// Passthrough mocks — default to "not supported" so legacy-path tests are
// unchanged. Passthrough-specific tests inject a real *cli.Process.
func (f *fakeProcess) SendPassthrough(ctx context.Context, text string, images []cli.ImageData, onEvent cli.EventCallback, priority string) (*cli.SendResult, error) {
	return f.Send(ctx, text, images, onEvent)
}
func (f *fakeProcess) DiscardPassthroughPending(_ error) {}
func (f *fakeProcess) PassthroughDepth() int             { return 0 }
func (f *fakeProcess) SupportsPassthrough() bool         { return false }

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
	r := &Router{
		ss:       sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs: maxProcs,
		ttl:      30 * time.Minute,
		pruneTTL: 72 * time.Hour,
	}
	r.bkStore.wrapper = cli.NewWrapper("/nonexistent/cli-binary", &cli.ClaudeProtocol{}, "claude")
	return r
}

// injectSession inserts a fake session directly into the router's session map.
// Must be called before any concurrent operations on the router.
func injectSession(r *Router, key string, proc processIface) *ManagedSession {
	s := &ManagedSession{
		key: key,
	}
	s.storeProcess(proc)
	s.touchLastActive()
	r.ss.sessions[key] = s
	if !s.IsExempt() && proc != nil && proc.Alive() {
		r.ss.activeCount.Add(1)
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
	if r.maxProcs != DefaultMaxProcs {
		t.Errorf("maxProcs = %d, want DefaultMaxProcs=%d", r.maxProcs, DefaultMaxProcs)
	}
	if r.ttl != DefaultTTL {
		t.Errorf("ttl = %v, want DefaultTTL=%v", r.ttl, DefaultTTL)
	}
	if r.pruneTTL != DefaultPruneTTL {
		t.Errorf("pruneTTL = %v, want DefaultPruneTTL=%v", r.pruneTTL, DefaultPruneTTL)
	}
	// Freeze the exported defaults so an operator who wires these into config
	// validation / dashboard tooltips never has the values silently drift.
	// R70-ARCH-H5.
	if DefaultMaxProcs != 3 {
		t.Errorf("DefaultMaxProcs = %d, want 3", DefaultMaxProcs)
	}
	if DefaultTTL != 30*time.Minute {
		t.Errorf("DefaultTTL = %v, want 30m", DefaultTTL)
	}
	if DefaultPruneTTL != 72*time.Hour {
		t.Errorf("DefaultPruneTTL = %v, want 72h", DefaultPruneTTL)
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

func TestRouterSetUserLabel(t *testing.T) {
	r := NewRouter(RouterConfig{})
	// Inject a managed session directly so we can exercise the label path
	// without running a full spawnSession — the contract under test is
	// atomic.Value round-trip + storeGen/storeDirty bookkeeping.
	r.mu.Lock()
	r.ss.sessions["k1"] = &ManagedSession{key: "k1"}
	r.mu.Unlock()

	before := r.ss.gen.Load()
	if ok := r.SetUserLabel("k1", "我的会话"); !ok {
		t.Fatalf("SetUserLabel on existing session returned false")
	}
	if got := r.SessionFor("k1").UserLabel(); got != "我的会话" {
		t.Errorf("UserLabel = %q, want %q", got, "我的会话")
	}
	if gen := r.ss.gen.Load(); gen <= before {
		t.Errorf("storeGen did not advance: before=%d after=%d", before, gen)
	}
	if !r.ss.dirty {
		t.Errorf("storeDirty should be true after SetUserLabel")
	}

	// Clearing the label (empty string) is an explicit feature.
	if ok := r.SetUserLabel("k1", ""); !ok {
		t.Fatalf("SetUserLabel clear returned false")
	}
	if got := r.SessionFor("k1").UserLabel(); got != "" {
		t.Errorf("UserLabel after clear = %q, want empty", got)
	}

	// Unknown key returns false and does not bump storeGen.
	genBefore := r.ss.gen.Load()
	if ok := r.SetUserLabel("missing", "x"); ok {
		t.Errorf("SetUserLabel on unknown key returned true")
	}
	if r.ss.gen.Load() != genBefore {
		t.Errorf("storeGen advanced on unknown-key call")
	}
}

func TestRouterStoreRestoreUserLabel(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")

	labeled := newSessionWithID("feishu:direct:alice:general", "sess-111")
	labeled.SetUserLabel("alpha")
	if err := saveStore(storePath, map[string]*ManagedSession{labeled.key: labeled}); err != nil {
		t.Fatalf("saveStore: %v", err)
	}

	r := NewRouter(RouterConfig{StorePath: storePath})
	got := r.SessionFor(labeled.key)
	if got == nil {
		t.Fatalf("session not restored")
	}
	if got.UserLabel() != "alpha" {
		t.Errorf("restored UserLabel = %q, want alpha", got.UserLabel())
	}
}

func TestRouterSetGetSessionBackend(t *testing.T) {
	r := NewRouter(RouterConfig{})
	r.SetSessionBackend("k1", "kiro")
	if got := r.SessionBackend("k1"); got != "kiro" {
		t.Errorf("GetSessionBackend = %q, want kiro", got)
	}
	r.SetSessionBackend("k1", "") // clears
	if got := r.SessionBackend("k1"); got != "" {
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
	s1 := r.ss.sessions["feishu:direct:alice:general"]
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
			s := &ManagedSession{key: "k"}
			storeTotalCost(&s.totalCost, tt.sessCost)
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
	r.ss.sessions["restored-key"] = newSessionWithID("restored-key", "sess-restore")

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
	r.ss.sessions["restored"] = newSessionWithID("restored", "sess-x")

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
	r.ss.sessions["key1"] = &ManagedSession{key: "key1"}
	r.ss.sessions["key1"].setSessionID("sess-1")

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

// TestRouter_ResetAndDiscardOverride_RacesWithSetWorkspace verifies the
// atomic Reset+delete path used by /new (Round-207 SM1). Deterministic
// case asserts the override is gone; race case stresses the codepath
// under -race to catch any lock regression.
func TestRouter_ResetAndDiscardOverride_RacesWithSetWorkspace(t *testing.T) {
	r := newTestRouter(3)
	r.wsStore.overrides = make(map[string]string)
	r.defaultCWD = "/default"
	injectSession(r, "key1", newIdleProc())
	r.SetWorkspace("key1", "/tmp/override")
	if got := r.Workspace("key1"); got != "/tmp/override" {
		t.Fatalf("pre-reset workspace = %q, want /tmp/override", got)
	}
	r.ResetAndDiscardOverride("key1")
	if _, ok := r.wsStore.overrides["key1"]; ok {
		t.Error("workspaceOverrides[key1] still present after ResetAndDiscardOverride")
	}
	if got := r.Workspace("key1"); got != "/default" {
		t.Errorf("post-reset workspace = %q, want /default", got)
	}

	// Race sub-case: concurrent SetWorkspace with ResetAndDiscardOverride
	// must not trip -race; either order is acceptable as long as the lock
	// pairs the clear with the override delete.
	injectSession(r, "key2", newIdleProc())
	r.SetWorkspace("key2", "/tmp/initial")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			r.SetWorkspace("key2", fmt.Sprintf("/tmp/racing-%d", i))
		}
	}()
	r.ResetAndDiscardOverride("key2")
	<-done
	_ = r.Workspace("key2")
}

// TestRouter_SetWorkspace_RejectsEmptyChatKey pins R20260527122801-CR-16:
// SetWorkspace must reject chatKey=="" before mutating workspaceOverrides.
//
// An unauthenticated or misrouted dashboard request that reaches this
// path with chatKey="" used to silently install an override under the
// empty-string key — that single slot is harmless on its own, but the
// hardening also disarms a class of misuse where every sentinel-keyed
// caller stomps the same slot, masking the originating call site, and
// (worse) Workspace("") would then return the attacker-supplied path
// instead of the configured workspace fallback. Fail closed at the
// entry point.
func TestRouter_SetWorkspace_RejectsEmptyChatKey(t *testing.T) {
	r := newTestRouter(3)
	r.wsStore.overrides = make(map[string]string)
	r.defaultCWD = "/default"

	r.SetWorkspace("", "/tmp/attacker")

	// 1) Empty-key slot must not be installed.
	if _, ok := r.wsStore.overrides[""]; ok {
		t.Error("workspaceOverrides[\"\"] was installed; expected empty-chatKey reject")
	}
	// 2) Map cap must not have been consumed.
	if got := len(r.wsStore.overrides); got != 0 {
		t.Errorf("len(workspaceOverrides) = %d after empty-chatKey SetWorkspace; want 0", got)
	}
	// 3) Workspace("") must fall through to the configured default,
	// not return the attacker-supplied path.
	if got := r.Workspace(""); got != "/default" {
		t.Errorf("Workspace(\"\") = %q, want %q (must fall through to default)", got, "/default")
	}

	// 4) Real chatKeys still work — the guard must not regress the happy
	// path.
	r.SetWorkspace("real:user:alice", "/tmp/real")
	if got := r.Workspace("real:user:alice"); got != "/tmp/real" {
		t.Errorf("Workspace(real:user:alice) = %q, want /tmp/real", got)
	}
}

// TestWaitSocketGoneForKey_EmptyKey — the helper must be a no-op when
// called with an empty key (unused session). Without this guard Reset
// would block the caller for 2s for every test that never started a shim.
func TestWaitSocketGoneForKey_EmptyKey(t *testing.T) {
	start := time.Now()
	waitSocketGoneForKey("", 2*time.Second)
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("waitSocketGoneForKey('') took %v; want ~0", elapsed)
	}
}

// TestWaitSocketGoneForKey_NoSocketReturnsFast — for a key whose shim
// was never spawned, the derived socket path doesn't exist, so the
// helper should return in a single stat().
func TestWaitSocketGoneForKey_NoSocketReturnsFast(t *testing.T) {
	start := time.Now()
	waitSocketGoneForKey("test:fresh:key-that-never-spawned", 2*time.Second)
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("waitSocketGoneForKey(missing-socket) took %v; want ~0", elapsed)
	}
}

// ---------------------------------------------------------------------------
// Cleanup
// ---------------------------------------------------------------------------

func TestCleanupNoExpiredSessions(t *testing.T) {
	r := &Router{
		ss:       sessionStore{sessions: make(map[string]*ManagedSession)},
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
		ss:       sessionStore{sessions: make(map[string]*ManagedSession)},
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

// TestCleanupRunningSession_LiveEventsBlockStuckKill verifies the fix for
// "running sessions disappear from the sidebar during long turns": lastActive
// is only touched at Send entry, so a 20-minute code analysis would age past
// the 2×DefaultTotalTimeout stuck threshold and be Kill()'d mid-turn. The
// fix folds EventLog.LastEventAt() into the activity calculation so any
// streamed tool_use / thinking / assistant event proves the turn is alive.
func TestCleanupRunningSession_LiveEventsBlockStuckKill(t *testing.T) {
	r := &Router{
		ss:           sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs:     3,
		ttl:          1 * time.Minute,
		pruneTTL:     72 * time.Hour,
		totalTimeout: 5 * time.Minute, // stuckThreshold = 10 min
	}
	proc := newRunningProc()
	// Ancient lastActive (25 min ago) would normally trip stuck_running, but
	// a fresh LastEventAt (10 s ago) should protect the session.
	proc.lastEventAt = time.Now().Add(-10 * time.Second)
	s := injectSession(r, "key1", proc)
	s.lastActive.Store(time.Now().Add(-25 * time.Minute).UnixNano())

	r.Cleanup()

	if !proc.Alive() {
		t.Fatal("running session with live events must survive Cleanup; stuckKill regression")
	}
	if got := loadAtomicString(&s.deathReason); got == "stuck_running" {
		t.Errorf("deathReason = %q, want empty (session should not be classified as stuck)", got)
	}
}

// TestCleanupRunningSession_NoLiveEventsStillKilled verifies the stuckKill
// safety net still fires when the process is truly silent: lastActive stale
// AND LastEventAt stale (or zero) means the turn is not making progress.
func TestCleanupRunningSession_NoLiveEventsStillKilled(t *testing.T) {
	r := &Router{
		ss:           sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs:     3,
		ttl:          1 * time.Minute,
		pruneTTL:     72 * time.Hour,
		totalTimeout: 5 * time.Minute,
	}
	proc := newRunningProc()
	// LastEventAt deliberately left at zero so "max(lastActive, LastEventAt)"
	// falls back to lastActive. Simulates a shim that accepted a Send but
	// never returned a single stream-json event (genuine hang).
	s := injectSession(r, "key1", proc)
	s.lastActive.Store(time.Now().Add(-25 * time.Minute).UnixNano())

	r.Cleanup()

	if proc.Alive() {
		// expected — Kill() set isAlive=false
	} else if got := loadAtomicString(&s.deathReason); got != "stuck_running" {
		t.Errorf("deathReason = %q, want %q", got, "stuck_running")
	}
	if proc.Alive() {
		t.Error("truly stuck running session must still be killed")
	}
}

func TestCleanupSkipsRunningSession(t *testing.T) {
	r := &Router{
		ss:       sessionStore{sessions: make(map[string]*ManagedSession)},
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
		ss:       sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs: 3,
		ttl:      1 * time.Minute,
		pruneTTL: 1 * time.Hour,
	}
	s := &ManagedSession{key: "key1"}
	s.setSessionID("sess-1")
	s.lastActive.Store(time.Now().UnixNano()) // recent → within pruneTTL window
	r.ss.sessions["key1"] = s

	r.Cleanup() // must not panic

	_, total := r.Stats()
	if total != 1 {
		t.Errorf("nil-process session should remain in map after cleanup, total = %d", total)
	}
}

func TestCleanupSkipsDeadProcess(t *testing.T) {
	r := &Router{
		ss:       sessionStore{sessions: make(map[string]*ManagedSession)},
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

// TestCleanup_PrunesBackendOverride verifies R70-ARCH-MED: a nil-process
// session that ages past pruneTTL is removed from r.ss.sessions AND its entry
// in r.bkStore.backendOverrides is freed. A previous version of shouldPrune-branch
// only touched r.ss.sessions, so a session that was SetSessionBackend'd and
// then never spawned (e.g. config error at spawn time) would leave a
// backendOverride live forever. R71-TEST-M1.
func TestCleanup_PrunesBackendOverride(t *testing.T) {
	r := &Router{
		ss:       sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs: 3,
		ttl:      1 * time.Minute,
		pruneTTL: 1 * time.Hour,
	}
	r.bkStore.backendOverrides = map[string]string{}
	// nil-process session past pruneTTL — shouldPrune returns true.
	s := &ManagedSession{key: "k1"}
	s.lastActive.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	r.ss.sessions["k1"] = s
	r.bkStore.backendOverrides["k1"] = "kiro"
	r.bkStore.backendOverrides["other"] = "claude" // unrelated, must survive

	r.Cleanup()

	if _, ok := r.ss.sessions["k1"]; ok {
		t.Error("pruned session should be gone from r.ss.sessions")
	}
	if _, ok := r.bkStore.backendOverrides["k1"]; ok {
		t.Error("pruned session's backendOverride should be freed")
	}
	if got := r.bkStore.backendOverrides["other"]; got != "claude" {
		t.Errorf("unrelated backendOverride should survive, got %q", got)
	}
}

// TestUnregisterSessionLocked_KeepBackendOverride covers the two call-site
// semantics of the new unregisterSessionLocked helper introduced for the R70
// teardown-consolidation work:
//
//   - keepBackendOverride=true: ResetAndRecreate / Takeover paths respawn on
//     the same key and need spawnSession to consume the override atomically.
//   - keepBackendOverride=false: Reset / Remove / Cleanup prune are terminal
//     removals — the override MUST be freed to prevent leaks when the same
//     key is later created fresh.
//
// R71-TEST-M2.
func TestUnregisterSessionLocked_KeepBackendOverride(t *testing.T) {
	cases := []struct {
		name         string
		keep         bool
		wantOverride string // "" means entry must be absent
	}{
		{name: "keep=true preserves override", keep: true, wantOverride: "kiro"},
		{name: "keep=false deletes override", keep: false, wantOverride: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Router{
				ss: sessionStore{sessions: make(map[string]*ManagedSession)},
			}
			r.bkStore.backendOverrides = map[string]string{"k1": "kiro"}
			s := &ManagedSession{key: "k1"}
			s.setSessionID("sess-1")
			r.ss.sessions["k1"] = s

			r.mu.Lock()
			r.unregisterSessionLocked("k1", s, tc.keep)
			r.mu.Unlock()

			if _, ok := r.ss.sessions["k1"]; ok {
				t.Error("session must be removed from r.ss.sessions regardless of keepBackendOverride")
			}
			got, ok := r.bkStore.backendOverrides["k1"]
			if tc.wantOverride == "" {
				if ok {
					t.Errorf("backendOverride must be freed, got %q", got)
				}
			} else if got != tc.wantOverride {
				t.Errorf("backendOverride = %q, want %q", got, tc.wantOverride)
			}
		})
	}
}

func TestCleanupMultipleSessions(t *testing.T) {
	r := &Router{
		ss:       sessionStore{sessions: make(map[string]*ManagedSession)},
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
	oldActive := s.LastActive()
	time.Sleep(2 * time.Millisecond)

	_, _, err := r.GetOrCreate(context.Background(), "key1", AgentOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !s.LastActive().After(oldActive) {
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
	r.ss.sessions["feishu:direct:user1:general"] = s

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
	r.ss.sessions["key1"] = newSessionWithID("key1", "restored-sess")

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
		ss:       sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs: 3,
		ttl:      30 * time.Minute,
	}
	r.bkStore.wrapper = cli.NewWrapper("/nonexistent/cli-binary", &cli.ClaudeProtocol{}, "claude")
	r.bkStore.model = "default-model"
	r.bkStore.extraArgs = []string{"--base-arg"}

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
	r := &Router{ss: sessionStore{sessions: make(map[string]*ManagedSession)}}

	r.mu.Lock()
	evicted := r.evictOldest()
	r.mu.Unlock()

	if evicted {
		t.Error("evictOldest should return false for empty router")
	}
}

func TestEvictOldestReturnsTrue(t *testing.T) {
	r := &Router{ss: sessionStore{sessions: make(map[string]*ManagedSession)}, maxProcs: 1}
	proc := newIdleProc()
	s := &ManagedSession{key: "key1"}
	s.storeProcess(proc)
	s.touchLastActive()
	r.ss.sessions["key1"] = s

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
	r := &Router{ss: sessionStore{sessions: make(map[string]*ManagedSession)}}
	proc := newRunningProc()
	s := &ManagedSession{key: "key1"}
	s.storeProcess(proc)
	s.touchLastActive()
	r.ss.sessions["key1"] = s

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
	r := &Router{ss: sessionStore{sessions: make(map[string]*ManagedSession)}}
	proc := newDeadProc()
	s := &ManagedSession{key: "key1"}
	s.storeProcess(proc)
	s.touchLastActive()
	r.ss.sessions["key1"] = s

	r.mu.Lock()
	evicted := r.evictOldest()
	r.mu.Unlock()

	if evicted {
		t.Error("evictOldest should skip dead sessions")
	}
}

func TestEvictOldestPicksOldest(t *testing.T) {
	r := &Router{ss: sessionStore{sessions: make(map[string]*ManagedSession)}}

	oldProc := newIdleProc()
	recentProc := newIdleProc()

	oldSession := &ManagedSession{key: "old-key"}
	oldSession.storeProcess(oldProc)
	oldSession.lastActive.Store(time.Now().Add(-2 * time.Hour).UnixNano())

	recentSession := &ManagedSession{key: "recent-key"}
	recentSession.storeProcess(recentProc)
	recentSession.lastActive.Store(time.Now().Add(-1 * time.Minute).UnixNano())

	r.ss.sessions["old-key"] = oldSession
	r.ss.sessions["recent-key"] = recentSession

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
	r := &Router{ss: sessionStore{sessions: make(map[string]*ManagedSession)}}
	r.ss.sessions["nil-key"] = newSessionWithID("nil-key", "sess-1")

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
		ss:        sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs:  3,
		ttl:       30 * time.Minute,
		storePath: storePath,
	}
	r.ss.sessions["feishu:direct:user1:general"] = newSessionWithID("feishu:direct:user1:general", "sess-abc")

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

// TestShutdownIdempotent verifies that calling Shutdown a second time is a
// no-op rather than racing the broadcast timer or re-detaching processes.
// R49-REL-SHUTDOWN-ONCE.
func TestShutdownIdempotent(t *testing.T) {
	r := newTestRouter(3)
	proc := newIdleProc()
	injectSession(r, "key1", proc)

	r.Shutdown()
	if proc.Alive() {
		t.Fatalf("first Shutdown should close the process")
	}
	// Second call must not panic or block even though historyCancel/procs
	// already ran once; sync.Once swallows the re-entry.
	done := make(chan struct{})
	go func() {
		r.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second Shutdown hung")
	}
}

// ---------------------------------------------------------------------------
// countActive
// ---------------------------------------------------------------------------

func TestCountActive_ReflectsAliveProcesses(t *testing.T) {
	r := &Router{ss: sessionStore{sessions: make(map[string]*ManagedSession)}}
	injectSession(r, "alive1", newIdleProc())
	injectSession(r, "alive2", newRunningProc())
	injectSession(r, "dead1", newDeadProc())

	r.mu.Lock()
	r.countActive()
	r.mu.Unlock()

	if got := r.ss.activeCount.Load(); got != 2 {
		t.Errorf("activeCount = %d, want 2", got)
	}
}

// ---------------------------------------------------------------------------
// Concurrency / race detector
// ---------------------------------------------------------------------------

func TestConcurrentGetOrCreate_SameKey_Race(t *testing.T) {
	r := newTestRouter(5)
	key := "feishu:direct:user1:general"

	const N = 10
	// Hard timeout — newTestRouter's wrapper points at a nonexistent binary,
	// so each spawn fails fast (sub-millisecond). Even with 10 concurrent
	// goroutines serialising through r.mu, a healthy run finishes in <1s.
	// 10s is generous against -race + slow CI; a deadlock or missed
	// close(doneCh) trips this rather than the test hanging the suite.
	// R248-TEST-9.
	const hardTimeout = 10 * time.Second

	var (
		wg            sync.WaitGroup
		errCount      atomic.Int64
		successCount  atomic.Int64
		nonNilErrSeen atomic.Int64
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
			if err != nil {
				errCount.Add(1)
				nonNilErrSeen.Add(1)
			} else {
				successCount.Add(1)
			}
		}()
	}

	// Drive wg.Wait through a guarded channel so the hard timer can fail
	// the test loud rather than hanging the whole suite.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(hardTimeout):
		t.Fatalf("TestConcurrentGetOrCreate_SameKey_Race did not finish within %v — "+
			"likely deadlock in spawningKeys handoff or missed close(doneCh)", hardTimeout)
	}

	// At least N-1 goroutines must have taken the inflight-wait branch.
	// newTestRouter's wrapper is missing-binary so every Spawn errors,
	// meaning at most ONE goroutine can be the first-spawn winner — the
	// rest necessarily observed spawningKeys[key] and parked on doneCh.
	// (Even if the winner finishes its failed spawn before some siblings
	// arrive, those siblings re-enter the loop, see no session, become
	// the next "winner" themselves; either way they did not race the
	// shim-socket dial and the contract holds.)
	//
	// The signal we want to defend: every concurrent GetOrCreate either
	// succeeds (impossible here — Spawn always fails) or surfaces an
	// error. A regression that corrupts spawningKeys could result in a
	// nil-result + nil-error return, which we'd see as a goroutine that
	// counted neither. R248-TEST-9.
	totalAccountedFor := errCount.Load() + successCount.Load()
	if totalAccountedFor != int64(N) {
		t.Errorf("only %d/%d goroutines returned a recorded result (errs=%d successes=%d); "+
			"a missing return path is a regression", totalAccountedFor, N, errCount.Load(), successCount.Load())
	}
	if successCount.Load() != 0 {
		t.Errorf("successes = %d, want 0 — newTestRouter wrapper points at /nonexistent/cli-binary",
			successCount.Load())
	}
	if nonNilErrSeen.Load() < int64(N-1) {
		t.Errorf("only %d goroutines saw a non-nil error; expected at least N-1=%d "+
			"(only one can be the spawn-winner; the rest must take the inflight-wait path "+
			"and ultimately surface a Spawn error of their own). R248-TEST-9.",
			nonNilErrSeen.Load(), N-1)
	}
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

// Stats must never return active > total. Pre-R59-GO-H1 the activeCount.Load()
// ran outside the RLock that sampled len(sessions), so a mutation landing
// between the two reads could bump active past total and the dashboard
// would show an impossible "3/2 active". The mutator drives session
// liveness changes through the lock-holding Reset path so we exercise the
// real concurrency boundary, not a helper bypass. R59-GO-H1.
func TestStats_ActiveNeverExceedsTotal(t *testing.T) {
	r := newTestRouter(10)
	// Seed 8 live sessions through the write-lock helper.
	for i := 0; i < 8; i++ {
		injectSession(r, fmt.Sprintf("key%d", i), newIdleProc())
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Mutator: flip session liveness via Reset (which acquires r.mu.Lock)
	// to create contention with Stats's RLock. We intentionally don't
	// re-inject: Reset decrements activeCount and total in the same
	// critical section, so the invariant must hold throughout.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			key := fmt.Sprintf("key%d", i%8)
			r.Reset(key)
			// Re-inject under the write lock so total grows and shrinks
			// in sync with activeCount.
			r.mu.Lock()
			s := &ManagedSession{key: key}
			s.storeProcess(newIdleProc())
			s.touchLastActive()
			r.ss.sessions[key] = s
			r.ss.activeCount.Add(1)
			r.mu.Unlock()
		}
	}()

	// Observers: assert invariant across many samples.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5000; j++ {
				active, total := r.Stats()
				if active > total {
					t.Errorf("active=%d > total=%d", active, total)
					return
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestConcurrentCleanup_Race(t *testing.T) {
	r := &Router{
		ss:       sessionStore{sessions: make(map[string]*ManagedSession)},
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
		ss:       sessionStore{sessions: make(map[string]*ManagedSession)},
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
	testhelper.Eventually(t, func() bool {
		return !proc.Alive() // cleanup fired and closed the expired session
	}, 500*time.Millisecond, "cleanup loop did not close expired session")
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
	_, stillMarked := r.pp.spawningKeys["key1"]
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
	if r.pp.spawningKeys == nil {
		r.pp.spawningKeys = make(map[string]chan struct{})
	}
	doneCh := make(chan struct{})
	r.pp.spawningKeys["cron:abc"] = doneCh
	r.mu.Unlock()

	// Reconcile's view: lock, snapshot, unlock.
	r.mu.Lock()
	_, spawning := r.pp.spawningKeys["cron:abc"]
	r.mu.Unlock()
	if !spawning {
		t.Fatal("reconcile should see spawningKeys marker and skip orphan check")
	}

	// After spawnSession's defer fires, the marker disappears (close +
	// delete mirror the production defer order in spawnSession).
	r.mu.Lock()
	close(doneCh)
	delete(r.pp.spawningKeys, "cron:abc")
	r.mu.Unlock()

	r.mu.Lock()
	_, stillMarked := r.pp.spawningKeys["cron:abc"]
	r.mu.Unlock()
	if stillMarked {
		t.Error("spawningKeys leaked after cleanup")
	}
}

// TestStripResumeArgs_FastPath verifies the no-resume common case returns
// the input slice unchanged (same backing array identity), not a copy. The
// startup drift-check runs this once per discovered shim; when no session
// was mid-turn, every call previously paid a full slice alloc + copy.
// R64-PERF-9 regression.
func TestStripResumeArgs_FastPath(t *testing.T) {
	args := []string{"--setting-sources", "", "--output-format", "stream-json"}
	got := stripResumeArgs(args)
	if len(got) != len(args) {
		t.Fatalf("len(got)=%d, want %d", len(got), len(args))
	}
	// Same backing array: no alloc/copy when --resume is absent.
	if len(args) > 0 && &got[0] != &args[0] {
		t.Errorf("fast path should return same backing array when --resume is absent")
	}
}

// TestStripResumeArgs_WithResume verifies the stripping behavior is unchanged
// for args containing --resume <id>.
func TestStripResumeArgs_WithResume(t *testing.T) {
	args := []string{"--resume", "abc-123", "--output-format", "stream-json"}
	got := stripResumeArgs(args)
	want := []string{"--output-format", "stream-json"}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got=%q, want=%q", i, got[i], want[i])
		}
	}
}

// TestStripResumeArgs_TrailingResume covers the edge case where --resume is
// the final arg with no value following. Previously the guard
// `i+1 < len(args)` kept the bare flag in the output, leaking `--resume`
// into drift-check compares and spuriously shutting down the shim when
// config hadn't changed. R65-GO-M-2 regression.
func TestStripResumeArgs_TrailingResume(t *testing.T) {
	args := []string{"--output-format", "stream-json", "--resume"}
	got := stripResumeArgs(args)
	want := []string{"--output-format", "stream-json"}
	if len(got) != len(want) {
		t.Fatalf("trailing --resume: len(got)=%d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got=%q, want=%q", i, got[i], want[i])
		}
	}
}

// TestValidateSessionKey exercises the shared session-key validator used at
// reverse-RPC / HTTP trust boundaries. R65-SEC-M-2.
func TestValidateSessionKey(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty rejected", "", true},
		{"plain ascii", "feishu:direct:alice:general", false},
		{"utf8 chinese allowed", "feishu:direct:张三:general", false},
		{"trailing tab rejected", "a:b:c\t:d", true},
		{"newline rejected", "a:b:c\n:d", true},
		{"C1 NEL rejected (U+0085)", "a:b:c\u0085:d", true},
		{"C1 U+009F rejected", "a:b:c\u009F:d", true},
		{"DEL rejected", "a:b:c\x7f:d", true},
		{"zero-width space rejected", "a:b:c\u200B:d", true},
		{"RLO rejected", "a:b:c\u202E:d", true},
		{"BOM rejected", "a:b:c\uFEFF:d", true},
		{"LSEP rejected", "a:b:c\u2028:d", true},
		{"invalid utf-8 rejected", "a:b:\xc3\x28:d", true},
		{"oversized rejected", strings.Repeat("a", MaxSessionKeyBytes+1), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateSessionKey(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// InterruptSessionSafe (F0) — dashboard-facing entry point
// ---------------------------------------------------------------------------
//
// Rationale recap: raw SIGINT on Claude `-p` mode terminates the whole
// CLI process (not just the current turn). That cascades into
// cli_exited → shim socket close → naozhi spawning a brand-new shim on
// the next message, leaking the old socket path and sometimes losing
// resume context. InterruptSessionSafe must prefer the in-band
// control_request path and only fall back to SIGINT when necessary.

func TestInterruptSessionSafe_PrefersControlRequest(t *testing.T) {
	r := newTestRouter(3)
	proc := newRunningProc()
	injectSession(r, "k1", proc)

	outcome := r.InterruptSessionSafe("k1")
	if outcome != InterruptSent {
		t.Errorf("outcome = %v, want InterruptSent", outcome)
	}
	proc.mu.Lock()
	viaCtl, sigint := proc.viaControlCalls, proc.interruptCalls
	proc.mu.Unlock()
	if viaCtl != 1 {
		t.Errorf("InterruptViaControl calls = %d, want 1", viaCtl)
	}
	if sigint != 0 {
		t.Errorf("Interrupt (SIGINT) calls = %d, want 0 — control_request succeeded, no fallback expected", sigint)
	}
}

func TestInterruptSessionSafe_FallsBackOnUnsupported(t *testing.T) {
	r := newTestRouter(3)
	proc := newRunningProc()
	proc.viaControlErr = cli.ErrInterruptUnsupported // ACP-like protocol
	injectSession(r, "k1", proc)

	outcome := r.InterruptSessionSafe("k1")
	if outcome != InterruptSent {
		t.Errorf("outcome = %v, want InterruptSent (after SIGINT fallback)", outcome)
	}
	proc.mu.Lock()
	viaCtl, sigint := proc.viaControlCalls, proc.interruptCalls
	proc.mu.Unlock()
	if viaCtl != 1 {
		t.Errorf("InterruptViaControl calls = %d, want 1", viaCtl)
	}
	if sigint != 1 {
		t.Errorf("Interrupt (SIGINT) calls = %d, want 1 — unsupported protocol should fall back", sigint)
	}
}

func TestInterruptSessionSafe_NoActiveTurnDoesNotFallBack(t *testing.T) {
	// Idle Claude `-p` session: raw SIGINT terminates the CLI, which is
	// exactly the regression we are defending against. The button press
	// should report "nothing was running" instead of silently ending the
	// session. The HTTP/WS layers both map InterruptNoTurn → "not_running".
	r := newTestRouter(3)
	proc := newIdleProc()
	proc.viaControlRunning = true // returns ErrNoActiveTurn when idle
	injectSession(r, "k1", proc)

	outcome := r.InterruptSessionSafe("k1")
	if outcome != InterruptNoTurn {
		t.Errorf("outcome = %v, want InterruptNoTurn (no fallback — would kill -p CLI)", outcome)
	}
	proc.mu.Lock()
	sigint := proc.interruptCalls
	proc.mu.Unlock()
	if sigint != 0 {
		t.Errorf("Interrupt (SIGINT) calls = %d, want 0 — idle session must not fall back to SIGINT on -p mode", sigint)
	}
}

func TestInterruptSessionSafe_TransportErrorDoesNotFallBack(t *testing.T) {
	// control_request write failed — the shim socket is almost certainly
	// broken. SIGINT would travel the same broken path and also fail.
	// Surface the error so F6's reconcile path cleans up the zombie.
	r := newTestRouter(3)
	proc := newRunningProc()
	proc.viaControlErr = cli.ErrMessageTooLarge // any non-sentinel write-ish error
	injectSession(r, "k1", proc)

	outcome := r.InterruptSessionSafe("k1")
	if outcome != InterruptError {
		t.Errorf("outcome = %v, want InterruptError (no fallback on transport failure)", outcome)
	}
	proc.mu.Lock()
	sigint := proc.interruptCalls
	proc.mu.Unlock()
	if sigint != 0 {
		t.Errorf("Interrupt calls = %d, want 0 — transport error must not trigger SIGINT", sigint)
	}
}

func TestInterruptSessionSafe_NoSession(t *testing.T) {
	r := newTestRouter(3)
	outcome := r.InterruptSessionSafe("missing-key")
	if outcome != InterruptNoSession {
		t.Errorf("outcome = %v, want InterruptNoSession", outcome)
	}
}

func TestInterruptSessionSafe_DeadProcess(t *testing.T) {
	r := newTestRouter(3)
	proc := newDeadProc()
	injectSession(r, "k1", proc)

	outcome := r.InterruptSessionSafe("k1")
	// Dead process → InterruptViaControl returns InterruptNoSession (via
	// ManagedSession.InterruptViaControl's `!proc.Alive()` branch), which
	// is a terminal outcome — we do NOT fall back because SIGINT on a
	// dead process is a no-op and just adds log noise.
	if outcome != InterruptNoSession {
		t.Errorf("outcome = %v, want InterruptNoSession (no fallback on dead)", outcome)
	}
	proc.mu.Lock()
	sigint := proc.interruptCalls
	proc.mu.Unlock()
	if sigint != 0 {
		t.Errorf("Interrupt calls = %d, want 0 — dead proc should not SIGINT", sigint)
	}
}

// ---------------------------------------------------------------------------
// resolveResumeID — jsonl-existence pre-check
// ---------------------------------------------------------------------------

func TestClaudeProjectSlug(t *testing.T) {
	cases := []struct {
		name string
		cwd  string
		want string
	}{
		{"root", "/", "-"},
		{"typical", "/home/user/workspace/proj", "-home-user-workspace-proj"},
		{"trailing slash preserved", "/home/user/", "-home-user-"},
		{"nested", "/home/user/workspace/naozhi", "-home-user-workspace-naozhi"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeProjectSlug(tc.cwd); got != tc.want {
				t.Errorf("claudeProjectSlug(%q) = %q, want %q", tc.cwd, got, tc.want)
			}
		})
	}
}

// TestClaudeProjectSlug_MatchesDiscovery locks session.claudeProjectSlug and
// discovery.ClaudeProjectSlug to the same output for every input, so a future
// change to Claude CLI's project-directory naming scheme (which affects
// ~/.claude/projects/ layout) cannot be applied to only one of the two call
// sites. RNEW-002.
func TestClaudeProjectSlug_MatchesDiscovery(t *testing.T) {
	inputs := []string{
		"",
		"/",
		"/home/user",
		"/home/user/",
		"/home/user/workspace/naozhi",
		"/tmp/my-proj",
		"relative/path",
		"//double//slash//",
		"/with spaces/in path",
		"/unicode/目录/路径",
	}
	for _, cwd := range inputs {
		// Subtest name must not contain "/", which go test treats as a
		// hierarchy separator and silently rewrites to "_" — two inputs
		// differing only in slashes would collide under -run.
		t.Run(fmt.Sprintf("cwd=%q", cwd), func(t *testing.T) {
			s := claudeProjectSlug(cwd)
			d := discovery.ClaudeProjectSlug(cwd)
			if s != d {
				t.Errorf("session %q vs discovery %q for cwd %q — the two implementations have drifted; update both call sites in lock-step", s, d, cwd)
			}
		})
	}
}

func TestResolveResumeID(t *testing.T) {
	// Scratch claudeDir with a single jsonl under workspace slug "A" only.
	claudeDir := t.TempDir()
	workspaceA := "/home/u/wsA"
	workspaceB := "/home/u/wsB"
	okID := "sess-ok"
	missingID := "sess-missing"

	projA := filepath.Join(claudeDir, "projects", claudeProjectSlug(workspaceA))
	if err := os.MkdirAll(projA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projA, okID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		claudeDir string
		workspace string
		resumeID  string
		want      string // "" means downgraded to fresh
	}{
		{"empty resumeID unchanged", claudeDir, workspaceA, "", ""},
		{"empty claudeDir skipped", "", workspaceA, okID, okID},
		{"empty workspace skipped", claudeDir, "", okID, okID},
		{"jsonl exists keeps resumeID", claudeDir, workspaceA, okID, okID},
		{"jsonl missing in same workspace downgrades", claudeDir, workspaceA, missingID, ""},
		{"jsonl in wrong workspace downgrades (work_dir edit regression)",
			claudeDir, workspaceB, okID, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveResumeID(tc.claudeDir, tc.workspace, "cron:test", tc.resumeID)
			if got != tc.want {
				t.Errorf("resolveResumeID(cd=%q, ws=%q, id=%q) = %q, want %q",
					tc.claudeDir, tc.workspace, tc.resumeID, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// resolveSpawnParamsLocked — R70-ARCH-H2
// ---------------------------------------------------------------------------

func TestResolveSpawnParamsLocked(t *testing.T) {
	// Router with one default backend "claude" plus a secondary "kiro" so
	// backend-override cases have a real target.
	mkRouter := func() *Router {
		r := &Router{
			ss:         sessionStore{sessions: make(map[string]*ManagedSession)},
			defaultCWD: "/default/ws",
		}
		r.bkStore.wrappers = map[string]*cli.Wrapper{
			"claude": cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "claude"),
			"kiro":   cli.NewWrapper("/bin/false", &cli.ClaudeProtocol{}, "kiro"),
		}
		r.bkStore.defaultBackend = "claude"
		r.bkStore.model = "sonnet-default"
		r.bkStore.extraArgs = []string{"--flag-a"}
		r.bkStore.backendModels = map[string]string{"kiro": "kiro-model"}
		r.bkStore.backendExtraArgs = map[string][]string{"kiro": {"--kiro-arg"}}
		r.bkStore.backendOverrides = make(map[string]string)
		r.wsStore.overrides = make(map[string]string)
		return r
	}

	t.Run("backendOverride wins when opts.Backend empty", func(t *testing.T) {
		r := mkRouter()
		r.bkStore.backendOverrides["feishu:user:bob:agent1"] = "kiro"
		sp := r.resolveSpawnParamsLocked("feishu:user:bob:agent1", "", AgentOpts{})
		if sp.BackendID != "kiro" {
			t.Errorf("BackendID = %q, want kiro", sp.BackendID)
		}
		if sp.Model != "kiro-model" {
			t.Errorf("Model = %q, want kiro-model", sp.Model)
		}
		if len(sp.Args) != 1 || sp.Args[0] != "--kiro-arg" {
			t.Errorf("Args = %v, want [--kiro-arg]", sp.Args)
		}
		// Override is consumed (one-shot).
		if _, still := r.bkStore.backendOverrides["feishu:user:bob:agent1"]; still {
			t.Error("backendOverride was not consumed")
		}
	})

	t.Run("opts.Backend beats backendOverride", func(t *testing.T) {
		r := mkRouter()
		r.bkStore.backendOverrides["feishu:user:bob:agent1"] = "kiro"
		sp := r.resolveSpawnParamsLocked("feishu:user:bob:agent1", "",
			AgentOpts{Backend: "claude"})
		if sp.BackendID != "claude" {
			t.Errorf("BackendID = %q, want claude", sp.BackendID)
		}
	})

	t.Run("workspaceOverride (chatKey) wins when opts.Workspace empty", func(t *testing.T) {
		r := mkRouter()
		r.wsStore.overrides["feishu:user:alice"] = "/override/ws"
		sp := r.resolveSpawnParamsLocked("feishu:user:alice:agent1", "", AgentOpts{})
		if sp.Workspace != "/override/ws" {
			t.Errorf("Workspace = %q, want /override/ws", sp.Workspace)
		}
	})

	t.Run("opts.Workspace beats workspaceOverride", func(t *testing.T) {
		r := mkRouter()
		r.wsStore.overrides["feishu:user:alice"] = "/override/ws"
		sp := r.resolveSpawnParamsLocked("feishu:user:alice:agent1", "",
			AgentOpts{Workspace: "/opts/ws"})
		if sp.Workspace != "/opts/ws" {
			t.Errorf("Workspace = %q, want /opts/ws", sp.Workspace)
		}
	})

	t.Run("invalid resumeID downgrades to empty", func(t *testing.T) {
		// claudeDir + workspace set, jsonl missing → resolveResumeID returns "".
		r := mkRouter()
		r.claudeDir = t.TempDir()
		sp := r.resolveSpawnParamsLocked("feishu:user:bob:agent1",
			"00000000-0000-0000-0000-000000000000", AgentOpts{Workspace: "/some/ws"})
		if sp.ResumeID != "" {
			t.Errorf("ResumeID = %q, want \"\" (downgraded)", sp.ResumeID)
		}
	})

	t.Run("all defaults when opts empty and no overrides", func(t *testing.T) {
		r := mkRouter()
		sp := r.resolveSpawnParamsLocked("feishu:user:bob:agent1", "", AgentOpts{})
		if sp.BackendID != "claude" {
			t.Errorf("BackendID = %q, want claude", sp.BackendID)
		}
		if sp.Model != "sonnet-default" {
			t.Errorf("Model = %q, want sonnet-default", sp.Model)
		}
		if len(sp.Args) != 1 || sp.Args[0] != "--flag-a" {
			t.Errorf("Args = %v, want [--flag-a]", sp.Args)
		}
		if sp.Workspace != "/default/ws" {
			t.Errorf("Workspace = %q, want /default/ws", sp.Workspace)
		}
		if sp.ResumeID != "" {
			t.Errorf("ResumeID = %q, want empty", sp.ResumeID)
		}
	})

	t.Run("opts.ExtraArgs appended after backend args", func(t *testing.T) {
		r := mkRouter()
		sp := r.resolveSpawnParamsLocked("k", "",
			AgentOpts{Backend: "kiro", ExtraArgs: []string{"--extra"}})
		want := []string{"--kiro-arg", "--extra"}
		if len(sp.Args) != 2 || sp.Args[0] != want[0] || sp.Args[1] != want[1] {
			t.Errorf("Args = %v, want %v", sp.Args, want)
		}
	})

	// Regression: a session whose process exited but whose entry is still
	// in r.ss.sessions must resume against the SAME backend it ran on. Before
	// this fix, resolveSpawnParamsLocked fell through to r.bkStore.defaultBackend
	// when opts.Backend was empty AND backendOverrides[key] was already
	// consumed (one-shot). For a kiro session that meant the second turn
	// silently respawned under claude with the kiro session_id, which then
	// fails the .claude/projects jsonl stat and downgrades to a fresh
	// claude session — the dashboard chip flips from kiro→cc and the
	// kiro conversation is lost. Naozhi-RFKB-1.
	t.Run("resume falls back to existing session backend when opts/override empty", func(t *testing.T) {
		r := mkRouter()
		key := "feishu:user:bob:agent1"
		old := &ManagedSession{key: key}
		old.SetBackend("kiro")
		r.ss.sessions[key] = old
		sp := r.resolveSpawnParamsLocked(key,
			"00000000-0000-0000-0000-000000000000", AgentOpts{})
		if sp.BackendID != "kiro" {
			t.Errorf("BackendID = %q, want kiro (inherited from existing session)", sp.BackendID)
		}
		// Backend-scoped overrides must follow the inherited backend so a
		// kiro respawn keeps using kiro's model + args.
		if sp.Model != "kiro-model" {
			t.Errorf("Model = %q, want kiro-model", sp.Model)
		}
		if len(sp.Args) != 1 || sp.Args[0] != "--kiro-arg" {
			t.Errorf("Args = %v, want [--kiro-arg]", sp.Args)
		}
	})

	// Existing-session backend MUST NOT override an explicit opts.Backend.
	// This protects the rare flow where the operator picks a different
	// backend for the next session via dashboard before the old one tears
	// down — ResetAndRecreate / Takeover both feed AgentOpts.Backend.
	t.Run("opts.Backend beats existing session backend", func(t *testing.T) {
		r := mkRouter()
		key := "feishu:user:bob:agent1"
		old := &ManagedSession{key: key}
		old.SetBackend("kiro")
		r.ss.sessions[key] = old
		sp := r.resolveSpawnParamsLocked(key, "",
			AgentOpts{Backend: "claude"})
		if sp.BackendID != "claude" {
			t.Errorf("BackendID = %q, want claude (opts.Backend wins)", sp.BackendID)
		}
	})

	// One-shot backendOverride must still beat session.Backend(), matching
	// the documented precedence: opts > one-shot override > existing
	// session > default.
	t.Run("backendOverride beats existing session backend", func(t *testing.T) {
		r := mkRouter()
		key := "feishu:user:bob:agent1"
		old := &ManagedSession{key: key}
		old.SetBackend("kiro")
		r.ss.sessions[key] = old
		r.bkStore.backendOverrides[key] = "claude"
		sp := r.resolveSpawnParamsLocked(key, "", AgentOpts{})
		if sp.BackendID != "claude" {
			t.Errorf("BackendID = %q, want claude (override wins over session)", sp.BackendID)
		}
	})
}

// ---------------------------------------------------------------------------
// classifyShimState — R70-ARCH-H4
// ---------------------------------------------------------------------------

func TestClassifyShimState(t *testing.T) {
	cases := []struct {
		name                                            string
		spawning, sessFound, hasLive, wrapperNil, drift bool
		want                                            shimState
	}{
		// spawning wins against every other signal
		{"spawning+everything", true, true, true, true, true, shimStateSkip},
		{"spawning alone", true, false, false, false, false, shimStateSkip},

		// no session → orphan (regardless of wrapper/drift)
		{"orphan clean", false, false, false, false, false, shimStateOrphan},
		{"orphan with wrapper nil", false, false, false, true, false, shimStateOrphan},
		{"orphan with drift flag", false, false, false, false, true, shimStateOrphan},

		// session exists with live process → skip
		{"live process", false, true, true, false, false, shimStateSkip},
		{"live process with drift", false, true, true, false, true, shimStateSkip},
		{"live process with wrapperNil", false, true, true, true, false, shimStateSkip},

		// session exists, no live process, no wrapper → noWrapper
		{"no wrapper", false, true, false, true, false, shimStateNoWrapper},
		{"no wrapper with drift", false, true, false, true, true, shimStateNoWrapper},

		// session exists, wrapper, drift → drift
		{"drift", false, true, false, false, true, shimStateDrift},

		// happy path → reconnect
		{"reconnect", false, true, false, false, false, shimStateReconnect},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyShimState(tc.spawning, tc.sessFound, tc.hasLive,
				tc.wrapperNil, tc.drift)
			if got != tc.want {
				t.Errorf("classifyShimState(spawning=%v, sessFound=%v, hasLive=%v, wrapperNil=%v, drift=%v) = %v, want %v",
					tc.spawning, tc.sessFound, tc.hasLive, tc.wrapperNil, tc.drift, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// collectPreviousHistory — R70-ARCH-H2 paired with resolveSpawnParamsLocked
// ---------------------------------------------------------------------------

// TestCollectPreviousHistory covers the three shapes spawnSession feeds
// in: fresh (no prior session), resume-same-id (no chain growth), and
// respawn-different-id (old ID appended to prevIDs).
func TestCollectPreviousHistory(t *testing.T) {
	t.Run("fresh: nil old session returns empty", func(t *testing.T) {
		entries, prev := collectPreviousHistory(nil, nil, "")
		if entries != nil || prev != nil {
			t.Errorf("collectPreviousHistory(nil) = (%v, %v), want (nil, nil)", entries, prev)
		}
	})

	t.Run("resume same id: chain unchanged, persistedHistory cloned", func(t *testing.T) {
		persisted := []cli.EventEntry{
			{Time: 1000, Type: "user", Summary: "hi"},
		}
		oldPrev := []string{"id-a"}
		s := &ManagedSession{persistedHistory: persisted}
		s.setSessionID("id-b")

		entries, prev := collectPreviousHistory(s, oldPrev, "id-b")

		if len(entries) != 1 || entries[0].Summary != "hi" {
			t.Errorf("entries = %v, want one 'hi' entry", entries)
		}
		if len(prev) != 1 || prev[0] != "id-a" {
			t.Errorf("prev = %v, want [id-a] (same id, no growth)", prev)
		}
	})

	t.Run("respawn new id: old id appended to chain", func(t *testing.T) {
		s := &ManagedSession{}
		s.setSessionID("id-old")

		_, prev := collectPreviousHistory(s, []string{"id-a"}, "id-new")

		if len(prev) != 2 || prev[0] != "id-a" || prev[1] != "id-old" {
			t.Errorf("prev = %v, want [id-a id-old]", prev)
		}
	})

	t.Run("chain cap: bounded at maxPrevSessionIDs", func(t *testing.T) {
		s := &ManagedSession{}
		s.setSessionID("id-old")

		// Seed oldPrev at the cap so appending old.sessionID overflows.
		oldPrev := make([]string, maxPrevSessionIDs)
		for i := range oldPrev {
			oldPrev[i] = fmt.Sprintf("id-%d", i)
		}
		_, prev := collectPreviousHistory(s, oldPrev, "id-new")

		if len(prev) != maxPrevSessionIDs {
			t.Fatalf("prev len = %d, want %d (capped)", len(prev), maxPrevSessionIDs)
		}
		if prev[len(prev)-1] != "id-old" {
			t.Errorf("last entry = %q, want id-old (newest retained)", prev[len(prev)-1])
		}
	})
}

// ---------------------------------------------------------------------------
// snapshotOldSessionLocked — CQ2 (R397) extraction
// ---------------------------------------------------------------------------

// TestSnapshotOldSessionLocked covers the four shapes spawnSession feeds in:
// nil old session (genuinely-new key), populated prevSessionIDs (defensive
// copy), totalCost preference (process value wins over store), and createdAt
// pass-through. Pure read helper — no mutation, lock-free in the test
// (caller documentation pins the r.mu requirement; the helper itself is a
// plain function so it works without a Router).
func TestSnapshotOldSessionLocked(t *testing.T) {
	t.Run("nil returns zero values", func(t *testing.T) {
		prev, cost, created := snapshotOldSessionLocked(nil)
		if prev != nil || cost != 0 || created != 0 {
			t.Errorf("snapshotOldSessionLocked(nil) = (%v, %v, %v), want (nil, 0, 0)",
				prev, cost, created)
		}
	})

	t.Run("prevSessionIDs defensive copy", func(t *testing.T) {
		s := &ManagedSession{prevSessionIDs: []string{"id-a", "id-b"}}
		prev, _, _ := snapshotOldSessionLocked(s)
		if len(prev) != 2 || prev[0] != "id-a" || prev[1] != "id-b" {
			t.Fatalf("prev = %v, want [id-a id-b]", prev)
		}
		// Mutating the returned slice must not affect the source.
		prev[0] = "mutated"
		if s.prevSessionIDs[0] != "id-a" {
			t.Errorf("source mutated through returned slice: %v", s.prevSessionIDs)
		}
	})

	t.Run("totalCost falls back to store when no proc", func(t *testing.T) {
		s := &ManagedSession{}
		storeTotalCost(&s.totalCost, 1.234)
		_, cost, _ := snapshotOldSessionLocked(s)
		if cost != 1.234 {
			t.Errorf("cost = %v, want 1.234 (from store, proc=nil)", cost)
		}
	})

	t.Run("createdAt round-trips", func(t *testing.T) {
		s := &ManagedSession{}
		s.createdAt.Store(123456789)
		_, _, created := snapshotOldSessionLocked(s)
		if created != 123456789 {
			t.Errorf("created = %v, want 123456789", created)
		}
	})

	t.Run("empty prevSessionIDs yields nil (no zero-len alloc)", func(t *testing.T) {
		s := &ManagedSession{}
		prev, _, _ := snapshotOldSessionLocked(s)
		if prev != nil {
			t.Errorf("prev = %v, want nil for empty source", prev)
		}
	})
}

// ---------------------------------------------------------------------------
// installFreshSessionLocked — CQ2 Round 213 extraction
// ---------------------------------------------------------------------------
//
// installFreshSessionLocked takes a concrete *cli.Process (the in-band
// SetOnTurnDone hook is not part of processIface), so a behavior-level
// table test would require spinning a real CLI subprocess. Instead we
// assert the method exists with the expected signature — this guards
// against accidental rename / parameter drift by future refactors and
// confirms the extraction compiles as a pure relocation. The
// underlying behavior is already covered end-to-end by every
// TestSpawnSession* case that exercises the enclosing spawnSession
// path, which now routes through this helper.
func TestInstallFreshSessionLocked_SignatureGuard(t *testing.T) {
	// Compile-time pin: if installFreshSessionLocked's signature drifts,
	// this assignment fails to build. Method values on a concrete type
	// are never nil, so a runtime nil-check here would be vacuous
	// (staticcheck SA4031).
	var _ = func(r *Router) func(
		key string,
		proc *cli.Process,
		workspace string,
		backendID string,
		wrapper *cli.Wrapper,
		resumeID string,
		oldHistory []cli.EventEntry,
		prevIDs []string,
		oldTotalCost float64,
		oldCreatedAt int64,
		exempt bool,
	) *ManagedSession {
		return r.installFreshSessionLocked
	}
	_ = t
}

// ---------------------------------------------------------------------------
// R248-TEST-3 — spawningKeys close-then-delete invariants
// ---------------------------------------------------------------------------

// TestSpawningKeys_FailedSpawnWakesWaiters pins R243-ARCH-4's headline
// guarantee: when the in-flight spawn for a key fails, every concurrent
// GetOrCreate goroutine parked on the same key wakes immediately rather
// than tick-polling. The test simulates the in-flight window by manually
// installing a doneCh in r.pp.spawningKeys (mirroring spawnSession's prologue),
// launches N concurrent GetOrCreate callers, and then performs the failure-
// path defer (close + delete under r.mu) by hand. Every waiter must return
// within 100ms (the historical poll interval was 20ms; instantaneous wakeup
// targets <1ms in practice but 100ms gives ample margin for race-detector
// scheduling on slow CI).
//
// The test runs with -race to catch any forgotten lock around the
// spawningKeys mutation; under -race the goroutines also exercise the
// release-and-reacquire mu pattern in the GetOrCreate retry loop.
func TestSpawningKeys_FailedSpawnWakesWaiters(t *testing.T) {
	r := newTestRouter(5)
	key := "feishu:direct:wakeup-waiters:general"

	// Install the spawningKeys marker so the next GetOrCreate hit takes
	// the inflight wait path. spawnSession uses the same pattern in its
	// prologue (router_lifecycle.go ~line 549).
	r.mu.Lock()
	if r.pp.spawningKeys == nil {
		r.pp.spawningKeys = make(map[string]chan struct{})
	}
	doneCh := make(chan struct{})
	r.pp.spawningKeys[key] = doneCh
	r.mu.Unlock()

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			// All N parked on the same key. Each will see the marker on
			// the first iteration of GetOrCreate's loop, release r.mu,
			// and select on doneCh.
			_, _, _ = r.GetOrCreate(context.Background(), key, AgentOpts{})
		}()
	}

	// Give all N goroutines time to enter the inflight wait. 50ms is well
	// past goroutine-launch overhead under -race; not a correctness
	// requirement, just stabilises the test against scheduler skew.
	time.Sleep(50 * time.Millisecond)

	// Simulate spawnSession's failure-path defer: close BEFORE delete (the
	// order is itself part of the contract — see TEST-3(b) below).
	start := time.Now()
	r.mu.Lock()
	close(doneCh)
	delete(r.pp.spawningKeys, key)
	r.mu.Unlock()

	// All waiters should observe the close + retry the loop. With
	// newTestRouter the second-iteration spawn also fails (binary
	// missing), so each goroutine returns its own error after a fast
	// failed Spawn. The wakeup itself is what we're timing.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		elapsed := time.Since(start)
		// 100ms is generous; observed wakeup is sub-millisecond. A regression
		// to the old 20ms tick-poll would still pass here, so the assertion's
		// real value is catching a deadlock or a missed close.
		if elapsed > 100*time.Millisecond {
			t.Errorf("waiters took %v to drain after close+delete; want <100ms", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiters did not drain within 2s — close(doneCh) likely failed to wake them")
	}
}

// TestSpawningKeys_CloseBeforeDelete_Order pins the documented ordering
// inside markSpawnDoneLocked: `close(ch)` must run BEFORE
// `delete(r.pp.spawningKeys, key)`. The helper godoc above the body in
// router_lifecycle.go (~line 530) explains why: a caller dispatched between
// "lock acquired" and "delete returned" must observe the closed channel from
// the still-present map entry, not a fresh nil from a re-arrived
// spawnSession that read the map after we finished.
//
// This is a source-grep anchor — the runtime test (TEST-3(a) above)
// observes the wakeup but cannot easily distinguish "closed then deleted"
// from "deleted then closed" without injecting a deterministic interleave.
// Pinning the lexical order makes the regression a fast CI failure.
//
// R248-ARCH-10 lifted the close+delete pair into the helper; the lexical
// pin moved with it.
func TestSpawningKeys_CloseBeforeDelete_Order(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("router_lifecycle.go")
	if err != nil {
		t.Fatalf("read router_lifecycle.go: %v", err)
	}
	body := string(src)

	closeIdx := strings.Index(body, "close(ch)")
	if closeIdx < 0 {
		t.Fatal("close(ch) not found in router_lifecycle.go — markSpawnDoneLocked refactored")
	}
	deleteIdx := strings.Index(body, "delete(r.pp.spawningKeys, key)")
	if deleteIdx < 0 {
		t.Fatal("delete(r.pp.spawningKeys, key) not found in router_lifecycle.go — markSpawnDoneLocked refactored")
	}
	if closeIdx >= deleteIdx {
		t.Errorf("close(ch) at byte %d is not before delete(r.pp.spawningKeys, key) at byte %d; "+
			"the order in markSpawnDoneLocked is load-bearing for R243-ARCH-4 / R248-ARCH-10",
			closeIdx, deleteIdx)
	}
}

// TestSpawningKeys_CtxCancelPriorityOverDoneCh pins that GetOrCreate's
// inflight-wait select-arm honours ctx.Done() — when ctx is already
// cancelled, the call must return ctx.Err() rather than retry the spawn
// loop. A request whose context already expired is by definition no longer
// interested in the result; retrying would burn another spawn slot on a
// corpse.
//
// The deterministic case is constructed by installing an in-flight marker
// that is never closed, leaving doneCh's select arm not-ready; the only way
// out is ctx.Done().
func TestSpawningKeys_CtxCancelPriorityOverDoneCh(t *testing.T) {
	r := newTestRouter(5)
	key := "feishu:direct:ctx-cancel:general"

	// Install an in-flight marker that we never close, so the doneCh arm
	// stays not-ready. ctx.Done() must therefore be the first ready arm.
	r.mu.Lock()
	if r.pp.spawningKeys == nil {
		r.pp.spawningKeys = make(map[string]chan struct{})
	}
	doneCh := make(chan struct{})
	r.pp.spawningKeys[key] = doneCh
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		close(doneCh)
		delete(r.pp.spawningKeys, key)
		r.mu.Unlock()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancelled

	startedAt := time.Now()
	_, _, err := r.GetOrCreate(ctx, key, AgentOpts{})
	if err == nil {
		t.Fatal("GetOrCreate returned nil error despite cancelled ctx — must surface ctx.Err()")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("GetOrCreate err = %v, want errors.Is(err, context.Canceled); "+
			"a ctx-cancelled caller must NOT fall through to retry-spawn", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Errorf("ctx-cancelled GetOrCreate took %v; ctx.Done() arm should fire immediately", elapsed)
	}
}
