package session

import (
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/discovery"
)

// alwaysOnPolicy is the AutoChainPolicy used by integration tests —
// returns enabled=true with a generous window so candidate filtering
// is governed by the test fixture rather than a hard cutoff.
type alwaysOnPolicy struct{}

func (alwaysOnPolicy) Enabled(string) bool         { return true }
func (alwaysOnPolicy) Window(string) time.Duration { return 365 * 24 * time.Hour }
func (alwaysOnPolicy) Cap(string) int              { return 32 }

// staticListJSONL is a deterministic ListWorkspaceJSONL fake. Tests
// inject one of these to drive pickWorkspaceChain without touching
// disk.
func staticListJSONL(byWorkspace map[string][]discovery.WorkspaceJSONL) func(string) []discovery.WorkspaceJSONL {
	return func(ws string) []discovery.WorkspaceJSONL {
		return byWorkspace[ws]
	}
}

// makeRouterForAutoChain builds a minimal *Router suitable for
// auto-chain integration tests. Caller can override autoChainListJSONL
// to inject test data.
func makeRouterForAutoChain(t *testing.T) *Router {
	t.Helper()
	r := newTestRouter(8)
	r.sessionsByChat = make(map[string]map[string]struct{})
	r.workspaceOverrides = make(map[string]string)
	r.backendOverrides = make(map[string]string)
	r.knownIDs = make(map[string]bool)
	r.sessionIDToKey = make(map[string]string)
	r.spawningKeys = make(map[string]struct{})
	r.autoChainPolicy = alwaysOnPolicy{}
	return r
}

func TestRunAutoChainBackfillOnce_FillsEmptyChain(t *testing.T) {
	r := makeRouterForAutoChain(t)

	now := time.Now()
	ws := "/home/test/ws"
	target := "00000000-0000-4000-8000-aaaaaaaaaaaa"
	candidate1 := "00000000-0000-4000-8000-000000000001"
	candidate2 := "00000000-0000-4000-8000-000000000002"

	// One target session with empty prev_session_ids; two candidate
	// JSONL files in its workspace.
	s := &ManagedSession{key: "dashboard:direct:user:general"}
	s.setWorkspace(ws)
	s.setSessionID(target)
	s.lastActive.Store(now.Add(-time.Hour).UnixNano())
	r.sessions[s.key] = s

	r.autoChainListJSONL = staticListJSONL(map[string][]discovery.WorkspaceJSONL{
		ws: {
			{SessionID: candidate1, Mtime: now.Add(-2 * time.Hour).UnixMilli()},
			{SessionID: candidate2, Mtime: now.Add(-1 * time.Hour).UnixMilli()},
			{SessionID: target, Mtime: now.UnixMilli()}, // current — must be excluded
		},
	})

	r.runAutoChainBackfillOnce()

	got := s.SnapshotChainIDs()
	want := []string{candidate1, candidate2, target}
	if len(got) != len(want) {
		t.Fatalf("chain len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("chain[%d] = %s, want %s", i, got[i], want[i])
		}
	}
	origins := s.SnapshotPrevSessionOrigins()
	if len(origins) != 2 || origins[0] != "auto-backfill" || origins[1] != "auto-backfill" {
		t.Errorf("origins = %v, want [auto-backfill auto-backfill]", origins)
	}
}

func TestRunAutoChainBackfillOnce_RespectsDisabled(t *testing.T) {
	r := makeRouterForAutoChain(t)
	r.autoChainPolicy = disabledAutoChainPolicy{}

	ws := "/home/test/ws"
	s := &ManagedSession{key: "k"}
	s.setWorkspace(ws)
	s.setSessionID("00000000-0000-4000-8000-aaaaaaaaaaaa")
	r.sessions["k"] = s

	r.autoChainListJSONL = staticListJSONL(map[string][]discovery.WorkspaceJSONL{
		ws: {{SessionID: "00000000-0000-4000-8000-000000000001", Mtime: time.Now().UnixMilli()}},
	})
	r.runAutoChainBackfillOnce()

	if got := len(s.prevSessionIDs); got != 0 {
		t.Errorf("disabled policy must not fill prev; got len=%d", got)
	}
}

func TestRunAutoChainBackfillOnce_SkipsCronSysScratch(t *testing.T) {
	r := makeRouterForAutoChain(t)

	now := time.Now()
	ws := "/home/test/ws"
	cronSess := &ManagedSession{key: "cron:job1"}
	cronSess.setWorkspace(ws)
	cronSess.setSessionID("00000000-0000-4000-8000-aaaaaaaaaaaa")
	r.sessions["cron:job1"] = cronSess

	sysSess := &ManagedSession{key: "sys:auto-titler"}
	sysSess.setWorkspace(ws)
	sysSess.setSessionID("00000000-0000-4000-8000-bbbbbbbbbbbb")
	r.sessions["sys:auto-titler"] = sysSess

	r.autoChainListJSONL = staticListJSONL(map[string][]discovery.WorkspaceJSONL{
		ws: {{SessionID: "00000000-0000-4000-8000-000000000001", Mtime: now.UnixMilli()}},
	})
	r.runAutoChainBackfillOnce()

	if len(cronSess.prevSessionIDs) != 0 {
		t.Errorf("cron session must not be backfilled; got %v", cronSess.prevSessionIDs)
	}
	if len(sysSess.prevSessionIDs) != 0 {
		t.Errorf("sys session must not be backfilled; got %v", sysSess.prevSessionIDs)
	}
}

func TestRunAutoChainBackfillOnce_NoDoubleAssignment(t *testing.T) {
	r := makeRouterForAutoChain(t)

	now := time.Now()
	ws := "/home/test/ws"

	// Two candidate user sessions, same workspace, both prev=empty.
	// One JSONL candidate. Earliest-active session must claim it.
	earlier := &ManagedSession{key: "k1"}
	earlier.setWorkspace(ws)
	earlier.setSessionID("00000000-0000-4000-8000-aaaaaaaaaaa1")
	earlier.lastActive.Store(now.Add(-2 * time.Hour).UnixNano())
	r.sessions["k1"] = earlier

	later := &ManagedSession{key: "k2"}
	later.setWorkspace(ws)
	later.setSessionID("00000000-0000-4000-8000-aaaaaaaaaaa2")
	later.lastActive.Store(now.Add(-time.Hour).UnixNano())
	r.sessions["k2"] = later

	candidateID := "00000000-0000-4000-8000-000000000001"
	r.autoChainListJSONL = staticListJSONL(map[string][]discovery.WorkspaceJSONL{
		ws: {{SessionID: candidateID, Mtime: now.Add(-30 * time.Minute).UnixMilli()}},
	})

	r.runAutoChainBackfillOnce()

	got1 := earlier.SnapshotChainIDs()
	got2 := later.SnapshotChainIDs()
	hasIn1 := len(earlier.prevSessionIDs) == 1 && earlier.prevSessionIDs[0] == candidateID
	hasIn2 := len(later.prevSessionIDs) == 1 && later.prevSessionIDs[0] == candidateID
	if hasIn1 == hasIn2 {
		t.Errorf("expected exactly one of the two sessions to claim the candidate. earlier=%v later=%v", got1, got2)
	}
	if !hasIn1 {
		t.Errorf("earliest-active session should have priority; got earlier=%v later=%v", got1, got2)
	}
}

func TestRunAutoChainBackfillOnce_SkipsNonEmptyPrev(t *testing.T) {
	r := makeRouterForAutoChain(t)

	now := time.Now()
	ws := "/home/test/ws"
	target := "00000000-0000-4000-8000-aaaaaaaaaaaa"
	preset := "00000000-0000-4000-8000-cccccccccccc"
	candidate := "00000000-0000-4000-8000-000000000001"

	s := &ManagedSession{key: "k"}
	s.setWorkspace(ws)
	s.setSessionID(target)
	s.prevSessionIDs = []string{preset}
	s.prevSessionOrigins = []string{"manual"}
	r.sessions["k"] = s

	r.autoChainListJSONL = staticListJSONL(map[string][]discovery.WorkspaceJSONL{
		ws: {{SessionID: candidate, Mtime: now.UnixMilli()}},
	})
	r.runAutoChainBackfillOnce()

	if len(s.prevSessionIDs) != 1 || s.prevSessionIDs[0] != preset {
		t.Errorf("non-empty prev must be left alone; got %v", s.prevSessionIDs)
	}
}

// scriptedExcluder is a SessionIDExcluder whose IsExcluded set can be
// extended by tests to simulate cron / sysession registering a new
// sessionID mid-test. Used by the New-B1 race tests below.
type scriptedExcluder struct {
	mu  sync.Mutex
	set map[string]bool
}

func (s *scriptedExcluder) IsExcluded(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set[id]
}

func (s *scriptedExcluder) add(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.set == nil {
		s.set = map[string]bool{}
	}
	s.set[id] = true
}

// TestRunAutoChainBackfillOnce_CronStartsMidPhase pins the New-B1 race
// closure (RFC §5.3.1): if cron registers a new sessionID between
// Phase 2 and Phase 3, the candidate that overlaps it MUST be dropped
// at Phase 3 re-validation. Uses testHookBeforeBackfillPhase3 so the
// race is mechanically reproducible — no time.Sleep.
func TestRunAutoChainBackfillOnce_CronStartsMidPhase(t *testing.T) {
	r := makeRouterForAutoChain(t)

	now := time.Now()
	ws := "/home/test/ws"
	target := "00000000-0000-4000-8000-aaaaaaaaaaaa"
	candidate := "00000000-0000-4000-8000-000000000001"

	s := &ManagedSession{key: "k"}
	s.setWorkspace(ws)
	s.setSessionID(target)
	r.sessions["k"] = s

	r.autoChainListJSONL = staticListJSONL(map[string][]discovery.WorkspaceJSONL{
		ws: {{SessionID: candidate, Mtime: now.UnixMilli()}},
	})

	cronExcluder := &scriptedExcluder{}
	r.AddSessionIDExcluder(cronExcluder)

	// Phase 2 finishes with `candidate` selected; the hook fires AFTER
	// Phase 2 and BEFORE Phase 3's re-validation lock. We register the
	// candidate in cron's excluder during the hook so Phase 3 sees it.
	r.testHookBeforeBackfillPhase3 = func() {
		cronExcluder.add(candidate)
	}

	r.runAutoChainBackfillOnce()

	if len(s.prevSessionIDs) != 0 {
		t.Errorf("phase-3 re-check must drop ID claimed by cron mid-flight; got %v", s.prevSessionIDs)
	}
}

// TestMaybeAttachAutoChainOnSpawn_HappyPath pins the spawn-path
// integration: a fresh-key spawn (no prev / no oldHistory) that has a
// candidate JSONL gets the chain returned.
func TestMaybeAttachAutoChainOnSpawn_HappyPath(t *testing.T) {
	r := makeRouterForAutoChain(t)
	now := time.Now()
	ws := "/home/test/ws"
	candidate := "00000000-0000-4000-8000-000000000001"

	r.autoChainListJSONL = staticListJSONL(map[string][]discovery.WorkspaceJSONL{
		ws: {{SessionID: candidate, Mtime: now.UnixMilli()}},
	})

	got := r.maybeAttachAutoChainOnSpawn("dashboard:direct:user:general", ws, nil, nil)
	if len(got) != 1 || got[0] != candidate {
		t.Errorf("got %v, want [%s]", got, candidate)
	}
}

func TestMaybeAttachAutoChainOnSpawn_SkipsCron(t *testing.T) {
	r := makeRouterForAutoChain(t)
	now := time.Now()
	ws := "/home/test/ws"

	r.autoChainListJSONL = staticListJSONL(map[string][]discovery.WorkspaceJSONL{
		ws: {{SessionID: "00000000-0000-4000-8000-000000000001", Mtime: now.UnixMilli()}},
	})

	got := r.maybeAttachAutoChainOnSpawn("cron:job1", ws, nil, nil)
	if got != nil {
		t.Errorf("cron key must skip auto-chain; got %v", got)
	}
}

func TestMaybeAttachAutoChainOnSpawn_SkipsResume(t *testing.T) {
	r := makeRouterForAutoChain(t)
	now := time.Now()
	ws := "/home/test/ws"

	r.autoChainListJSONL = staticListJSONL(map[string][]discovery.WorkspaceJSONL{
		ws: {{SessionID: "00000000-0000-4000-8000-000000000001", Mtime: now.UnixMilli()}},
	})

	// Resume path leaves prevIDs populated (chain rotation) — auto-chain
	// must defer to that decision.
	got := r.maybeAttachAutoChainOnSpawn("dashboard:direct:user:general", ws,
		[]string{"00000000-0000-4000-8000-cccccccccccc"}, nil)
	if got != nil {
		t.Errorf("non-empty prev must skip auto-chain; got %v", got)
	}
}

// TestMaybeAttachAutoChainOnSpawn_CronRegistersBetweenLockWindows
// pins New-B1 closure on the spawn path. Hook fires after Phase 2;
// during the hook we register an excluder claiming the candidate.
// Phase 3 re-validation must then drop the candidate.
func TestMaybeAttachAutoChainOnSpawn_CronRegistersBetweenLockWindows(t *testing.T) {
	r := makeRouterForAutoChain(t)
	now := time.Now()
	ws := "/home/test/ws"
	candidate := "00000000-0000-4000-8000-000000000001"

	r.autoChainListJSONL = staticListJSONL(map[string][]discovery.WorkspaceJSONL{
		ws: {{SessionID: candidate, Mtime: now.UnixMilli()}},
	})

	excl := &scriptedExcluder{}
	r.AddSessionIDExcluder(excl)
	r.testHookBeforeSpawnPhase3 = func() {
		excl.add(candidate)
	}

	got := r.maybeAttachAutoChainOnSpawn("dashboard:direct:user:general", ws, nil, nil)
	if got != nil {
		t.Errorf("phase-3 re-check must drop ID claimed mid-flight; got %v", got)
	}
}

// TestBackfillConcurrentWithSpawn pins the v3 Go-MEDIUM scenario:
// startup backfill is mid-flight (Phase 2 finished, Phase 3 not yet
// started) when a fresh spawn arrives for a different session in
// the same workspace. Both paths must NOT write the same sessionID
// into both chains.
//
// Mechanics:
//
//   - Backfill candidate session (k1) targets candidate ID `cid`.
//   - The hook fires after Phase 2 collects its decision but
//     BEFORE Phase 3 acquires r.mu and applies. Inside the hook,
//     we synchronously call maybeAttachAutoChainOnSpawn for k2
//     in the same workspace — that path runs end-to-end (its own
//     Phase 1-2-3) while backfill is paused at the hook.
//   - When the hook returns, backfill resumes Phase 3. Phase 3's
//     re-validation MUST observe k2 has already taken `cid` (via
//     k2's installFreshSessionLocked equivalent — here we model
//     it by populating r.sessions[k2].prevSessionIDs from the
//     spawn return value before releasing the hook), and drop the
//     duplicate.
//
// Pure unit test using fakes — no goroutines, no time.Sleep.
// Demonstrates the New-B1 closure works under the most contended
// real-world ordering.
func TestBackfillConcurrentWithSpawn(t *testing.T) {
	r := makeRouterForAutoChain(t)
	now := time.Now()
	ws := "/home/test/ws"
	cid := "00000000-0000-4000-8000-000000000001"

	// k1 is the backfill candidate; k2 will be spawned mid-flight.
	k1 := &ManagedSession{key: "dashboard:direct:user1:general"}
	k1.setWorkspace(ws)
	k1.setSessionID("00000000-0000-4000-8000-aaaaaaaaaaa1")
	k1.lastActive.Store(now.Add(-time.Hour).UnixNano())
	r.sessions[k1.key] = k1

	r.autoChainListJSONL = staticListJSONL(map[string][]discovery.WorkspaceJSONL{
		ws: {{SessionID: cid, Mtime: now.Add(-2 * time.Hour).UnixMilli()}},
	})

	r.testHookBeforeBackfillPhase3 = func() {
		// Simulate a concurrent spawn for k2 grabbing the same
		// candidate. maybeAttachAutoChainOnSpawn returns the chain
		// it would assign; we model installFreshSessionLocked by
		// publishing k2 with that chain into r.sessions BEFORE the
		// backfill resumes. This makes the next snapshotRouterExcluded
		// observe cid as occupied.
		got := r.maybeAttachAutoChainOnSpawn("dashboard:direct:user2:general", ws, nil, nil)
		if len(got) != 1 || got[0] != cid {
			t.Errorf("spawn under hook expected to claim %s, got %v", cid, got)
			return
		}
		k2 := &ManagedSession{key: "dashboard:direct:user2:general"}
		k2.setWorkspace(ws)
		k2.setSessionID("00000000-0000-4000-8000-aaaaaaaaaaa2")
		k2.prevSessionIDs = got
		r.mu.Lock()
		r.sessions[k2.key] = k2
		r.mu.Unlock()
	}

	r.runAutoChainBackfillOnce()

	// k1 must NOT have cid (k2 won the race via Phase 3 re-check).
	chain1 := k1.SnapshotChainIDs()
	for _, id := range chain1 {
		if id == cid {
			t.Errorf("k1 chain unexpectedly contains %s after concurrent spawn took it: %v", cid, chain1)
		}
	}

	// k2 must have cid (the spawn-path verification observed an empty
	// excluder set when it ran, so cid was correctly assignable then).
	r.mu.RLock()
	k2 := r.sessions["dashboard:direct:user2:general"]
	r.mu.RUnlock()
	if k2 == nil {
		t.Fatal("k2 vanished")
	}
	chain2 := k2.SnapshotChainIDs()
	hasCid := false
	for _, id := range chain2 {
		if id == cid {
			hasCid = true
			break
		}
	}
	if !hasCid {
		t.Errorf("k2 should have claimed %s; chain = %v", cid, chain2)
	}
}

func TestAddSessionIDExcluder_Aggregates(t *testing.T) {
	r := makeRouterForAutoChain(t)
	a := &scriptedExcluder{set: map[string]bool{"a": true}}
	b := &scriptedExcluder{set: map[string]bool{"b": true}}

	r.AddSessionIDExcluder(a)
	r.AddSessionIDExcluder(b)
	r.AddSessionIDExcluder(nil) // must be a no-op

	extras := r.extraExcluders()
	if len(extras) != 2 {
		t.Fatalf("len(extras) = %d, want 2", len(extras))
	}
	combined := combinedExcluder{inner: extras}
	if !combined.IsExcluded("a") || !combined.IsExcluded("b") {
		t.Errorf("aggregator must include both A and B excluders")
	}
}
