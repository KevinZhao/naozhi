package session

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/costledger"
)

// gatedSpawn is a spawnHook the test controls: each call signals entered,
// then waits for release before handing back its process.
type gatedSpawn struct {
	entered chan struct{}
	release chan struct{}
	proc    *fakeProcess
	err     error

	mu    sync.Mutex
	calls int
}

func newGatedSpawn() *gatedSpawn {
	return &gatedSpawn{entered: make(chan struct{}, 4), release: make(chan struct{}), proc: newIdleProc()}
}

func (g *gatedSpawn) hook(ctx context.Context, _ cli.SpawnOptions) (processIface, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	g.entered <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if g.err != nil {
		return nil, g.err
	}
	return g.proc, nil
}

// spawnRouter is a real Router whose spawns go through the hook.
func spawnRouter(t *testing.T, maxProcs int, hook func(context.Context, cli.SpawnOptions) (processIface, error)) *Router {
	t.Helper()
	// A wrapper the spawn never runs (spawnHook replaces it), for the CLI
	// name and version the new session records.
	r := NewRouter(RouterConfig{MaxProcs: maxProcs, Wrapper: cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")})
	r.spawnHook = hook
	t.Cleanup(r.Shutdown)
	return r
}

type spawnResult struct {
	s   *ManagedSession
	err error
}

func spawnAsync(r *Router, key string) <-chan spawnResult {
	out := make(chan spawnResult, 1)
	go func() {
		s, _, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
		out <- spawnResult{s, err}
	}()
	return out
}

func waitEntered(t *testing.T, g *gatedSpawn) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("spawn never reached the hook")
	}
}

func waitResult(t *testing.T, ch <-chan spawnResult) spawnResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("GetOrCreate did not return")
		return spawnResult{}
	}
}

// spawnIn runs one whole spawn for key: before (when set) and the reserve in
// one transaction, then the rest outside it.
func spawnIn(r *Router, key string, before func(tx sessTx)) (*ManagedSession, error) {
	var res spawnReservation
	var err error
	r.ss.Update(func(tx sessTx) {
		if before != nil {
			before(tx)
		}
		err = r.reserveSpawn(tx, &res, key, "", AgentOpts{})
	})
	if err != nil {
		return nil, err
	}
	return r.completeSpawn(context.Background(), &res)
}

// injectLocked installs a session for key under the table lock.
func injectLocked(r *Router, key string, proc processIface) *ManagedSession {
	r.ss.Lock()
	defer r.ss.Unlock()
	return injectSession(r, key, proc)
}

func TestSpawnSession_InstallsTheSpawnedProcess(t *testing.T) {
	proc := newIdleProc()
	r := spawnRouter(t, 4, func(context.Context, cli.SpawnOptions) (processIface, error) { return proc, nil })
	const key = "feishu:direct:spawn-ok:general"

	s, _, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if s.loadProcess() != proc {
		t.Fatal("the session does not hold the spawned process")
	}
	if r.SessionFor(key) != s {
		t.Fatal("the session is not in the table")
	}
	r.ss.RLock()
	pending := r.ss.Ext().spawns.PendingSpawns()
	_, inFlight := r.ss.Ext().spawns.SpawnInFlight(key)
	r.ss.RUnlock()
	if pending != 0 || inFlight {
		t.Errorf("after the spawn: pending=%d inFlight=%v, want 0/false", pending, inFlight)
	}
}

// TestSpawnSession_LiveSessionInstalledMeanwhileWins: a live session that
// lands for the key while this spawn is outside the lock is the one returned;
// the process this spawn started is closed, not installed over it.
func TestSpawnSession_LiveSessionInstalledMeanwhileWins(t *testing.T) {
	g := newGatedSpawn()
	r := spawnRouter(t, 4, g.hook)
	const key = "feishu:direct:spawn-race:general"

	res := spawnAsync(r, key)
	waitEntered(t, g)
	winner := injectLocked(r, key, newIdleProc())
	close(g.release)

	got := waitResult(t, res)
	if got.err != nil {
		t.Fatalf("GetOrCreate: %v", got.err)
	}
	if got.s != winner || r.SessionFor(key) != winner {
		t.Fatal("the spawn replaced the live session installed meanwhile")
	}
	if g.proc.Alive() {
		t.Error("the losing spawn's process was left running")
	}
}

// TestSpawnSession_RemovedMeanwhileStartsFresh: a session removed while the
// spawn is outside the lock is not resurrected into the new one — the new
// session does not continue its session-ID chain or its cost.
func TestSpawnSession_RemovedMeanwhileStartsFresh(t *testing.T) {
	g := newGatedSpawn()
	r := spawnRouter(t, 4, g.hook)
	const key = "feishu:direct:spawn-removed:general"
	old := injectLocked(r, key, newDeadProc())
	old.setSessionID("sid-old")
	storeTotalCost(&old.costSpent, 3.5)

	res := spawnAsync(r, key)
	waitEntered(t, g)
	if !r.Remove(key) {
		t.Fatal("Remove found no session")
	}
	close(g.release)

	got := waitResult(t, res)
	if got.err != nil {
		t.Fatalf("GetOrCreate: %v", got.err)
	}
	if ids := got.s.SnapshotPrevSessionIDs(); slices.Contains(ids, "sid-old") {
		t.Errorf("the new session continues the removed one's chain: %v", ids)
	}
	if c := loadTotalCost(&got.s.costSpent); c != 0 {
		t.Errorf("the new session inherited the removed one's spend: %v", c)
	}
}

// TestSpawnSession_ReplacedMeanwhileContinuesTheReplacement: when the key's
// dead session is replaced by another dead one during the spawn, the new
// session continues the one that is in the table at install time.
func TestSpawnSession_ReplacedMeanwhileContinuesTheReplacement(t *testing.T) {
	g := newGatedSpawn()
	r := spawnRouter(t, 4, g.hook)
	const key = "feishu:direct:spawn-replaced:general"
	first := injectLocked(r, key, newDeadProc())
	first.setSessionID("sid-first")

	res := spawnAsync(r, key)
	waitEntered(t, g)
	second := &ManagedSession{key: key}
	second.storeProcess(newDeadProc())
	second.setSessionID("sid-second")
	r.ss.Lock()
	r.ss.Put(key, second)
	r.ss.Unlock()
	close(g.release)

	got := waitResult(t, res)
	if got.err != nil {
		t.Fatalf("GetOrCreate: %v", got.err)
	}
	ids := got.s.SnapshotPrevSessionIDs()
	if !slices.Contains(ids, "sid-second") {
		t.Errorf("the new session does not continue the session in the table (%v)", ids)
	}
	if slices.Contains(ids, "sid-first") {
		t.Errorf("the new session continues a session that was no longer in the table (%v)", ids)
	}
}

// TestSpawnSession_FailedSpawnKeepsTheTuningPick: a model/effort picked for a
// key before its first message survives a spawn that fails, so the retry
// still runs with it.
func TestSpawnSession_FailedSpawnKeepsTheTuningPick(t *testing.T) {
	boom := errors.New("spawn failed")
	r := spawnRouter(t, 4, func(context.Context, cli.SpawnOptions) (processIface, error) { return nil, boom })
	const key = "feishu:direct:spawn-fail:general"
	model := "claude-opus-5"
	if _, err := r.SetSessionTuning(context.Background(), key, &model, nil); err != nil {
		t.Fatalf("SetSessionTuning: %v", err)
	}

	if _, _, err := r.GetOrCreate(context.Background(), key, AgentOpts{}); !errors.Is(err, boom) {
		t.Fatalf("GetOrCreate = %v, want the spawn error", err)
	}
	r.ss.RLock()
	pt, ok := r.ss.Ext().picks.tuning[key]
	pending := r.ss.Ext().spawns.PendingSpawns()
	_, inFlight := r.ss.Ext().spawns.SpawnInFlight(key)
	r.ss.RUnlock()
	if !ok || pt.Model != "claude-opus-5" {
		t.Errorf("the tuning pick was consumed by a failed spawn (ok=%v, %+v)", ok, pt)
	}
	if pending != 0 || inFlight {
		t.Errorf("after a failed spawn: pending=%d inFlight=%v, want 0/false", pending, inFlight)
	}

	// The retry consumes it onto the session.
	proc := newIdleProc()
	r.spawnHook = func(context.Context, cli.SpawnOptions) (processIface, error) { return proc, nil }
	s, _, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if s.TuningModel() != "claude-opus-5" {
		t.Errorf("the session's tuning model = %q, want the pick", s.TuningModel())
	}
	r.ss.RLock()
	_, still := r.ss.Ext().picks.tuning[key]
	r.ss.RUnlock()
	if still {
		t.Error("the pick outlived the spawn that consumed it")
	}
}

// TestSpawnSession_ShutdownGateRefusesLateSpawns: once Shutdown has taken
// its snapshot no spawn starts a process.
func TestSpawnSession_ShutdownGateRefusesLateSpawns(t *testing.T) {
	called := false
	r := NewRouter(RouterConfig{MaxProcs: 4, Wrapper: cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")})
	r.spawnHook = func(context.Context, cli.SpawnOptions) (processIface, error) {
		called = true
		return newIdleProc(), nil
	}
	r.Shutdown()
	if _, _, err := r.GetOrCreate(context.Background(), "feishu:direct:late:general", AgentOpts{}); !errors.Is(err, ErrRouterStopped) {
		t.Fatalf("GetOrCreate after Shutdown = %v, want ErrRouterStopped", err)
	}
	if called {
		t.Error("a spawn started after Shutdown")
	}
}

// TestSpawnSession_RespawnCarriesSpendAndRetiresTheOldID: respawning a dead
// session keeps its monotonic spend, and when the session ID rotates the old
// ID stops resolving to the key.
func TestSpawnSession_RespawnCarriesSpendAndRetiresTheOldID(t *testing.T) {
	r := spawnRouter(t, 4, func(context.Context, cli.SpawnOptions) (processIface, error) { return newIdleProc(), nil })
	const key = "feishu:direct:respawn:general"
	old := injectLocked(r, key, newDeadProc())
	old.setSessionID("sid-old")
	old.costMu.Lock()
	old.spent = costledger.Totals{Metered: map[costledger.Unit]float64{"requests": 3}}
	old.costMu.Unlock()
	s, err := spawnIn(r, key, func(tx sessTx) { tx.SetID("sid-old", key) })
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if got := s.CostTotals().Metered["requests"]; got != 3 {
		t.Errorf("respawned session's metered spend = %v, want the replaced session's 3", got)
	}
	r.ss.RLock()
	_, stale := r.ss.KeyForID("sid-old")
	r.ss.RUnlock()
	if stale {
		t.Error("the replaced session's ID still resolves to the key after the ID rotated")
	}
}

// TestGetOrCreate_PanicInsideTheReserveLeavesNothingHeld: a panic in the
// reserve transaction (here: the evicted victim's Close) reaches the caller,
// and leaves neither the table lock held nor the key marked in flight — the
// next GetOrCreate for the key spawns instead of parking forever.
func TestGetOrCreate_PanicInsideTheReserveLeavesNothingHeld(t *testing.T) {
	proc := newIdleProc()
	r := spawnRouter(t, 1, func(context.Context, cli.SpawnOptions) (processIface, error) { return proc, nil })
	victim := injectLocked(r, "feishu:direct:victim:general", newHookCloseProc(func() { panic("close failed") }))
	victim.lastActive.Store(1)
	const key = "feishu:direct:after-panic:general"

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not reach the caller")
			}
		}()
		_, _, _ = r.GetOrCreate(context.Background(), key, AgentOpts{})
	}()

	if !r.HealthCheck() {
		t.Fatal("the table lock is still held after the panic")
	}
	var inFlight bool
	var pending int
	r.ss.View(func(v sessView) {
		_, inFlight = v.Ext().spawns.SpawnInFlight(key)
		pending = v.Ext().spawns.PendingSpawns()
	})
	if inFlight || pending != 0 {
		t.Fatalf("after the panic: inFlight=%v pending=%d, want false/0", inFlight, pending)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the next GetOrCreate: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the next GetOrCreate parked on a marker the panic left behind")
	}
}
