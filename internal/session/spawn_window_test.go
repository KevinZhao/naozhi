package session

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/costledger"
	"github.com/naozhi/naozhi/internal/shim"
)

// gatedSpawn is a spawn.hook the test controls: each call signals entered,
// then waits for release before handing back a process of its own. It
// records every call's resume ID and process, in call order.
type gatedSpawn struct {
	entered chan struct{}
	release chan struct{}
	err     error

	mu        sync.Mutex
	resumeIDs []string
	procs     []*fakeProcess
}

func newGatedSpawn() *gatedSpawn {
	return &gatedSpawn{entered: make(chan struct{}, 4), release: make(chan struct{})}
}

func (g *gatedSpawn) hook(ctx context.Context, opts cli.SpawnOptions) (processIface, error) {
	proc := newIdleProc()
	g.mu.Lock()
	g.resumeIDs = append(g.resumeIDs, opts.ResumeID)
	g.procs = append(g.procs, proc)
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
	return proc, nil
}

// calls returns the resume ID and process of every call so far.
func (g *gatedSpawn) calls() ([]string, []*fakeProcess) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.resumeIDs), slices.Clone(g.procs)
}

// spawnRouter is a real Router whose spawns go through the hook.
func spawnRouter(t *testing.T, maxProcs int, hook func(context.Context, cli.SpawnOptions) (processIface, error)) *Router {
	t.Helper()
	// A wrapper the spawn never runs (spawn.hook replaces it), for the CLI
	// name and version the new session records.
	r := NewRouter(RouterConfig{MaxProcs: maxProcs, Wrapper: cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")})
	r.spawn.hook = hook
	t.Cleanup(r.Shutdown)
	return r
}

type spawnResult struct {
	s   *ManagedSession
	st  SessionStatus
	err error
}

func spawnAsync(r *Router, key string) <-chan spawnResult {
	out := make(chan spawnResult, 1)
	go func() {
		s, st, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
		out <- spawnResult{s, st, err}
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
	var (
		pending  int
		inFlight bool
	)
	r.ss.Update(func(v sessTx) {
		pending = v.Ext().spawns.PendingSpawns()
		_, inFlight = v.Ext().spawns.SpawnInFlight(key)
	})
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
	winner := injectSession(r, key, newIdleProc())
	close(g.release)

	got := waitResult(t, res)
	if got.err != nil {
		t.Fatalf("GetOrCreate: %v", got.err)
	}
	if got.s != winner || r.SessionFor(key) != winner {
		t.Fatal("the spawn replaced the live session installed meanwhile")
	}
	if _, procs := g.calls(); len(procs) != 1 || procs[0].Alive() {
		t.Errorf("the losing spawn's process was left running (%d spawns)", len(procs))
	}
}

// idResolves reports whether sid still routes to a key.
func idResolves(r *Router, sid string) bool {
	var ok bool
	r.ss.View(func(v sessView) { _, ok = v.KeyForID(sid) })
	return ok
}

// TestSpawnSession_RemovedMeanwhileStartsFresh: a dead session removed or
// reset (/new) while its resume spawn is outside the lock is not brought
// back. The process started with --resume of its ID is closed and a fresh
// one installed: not its ID, its idToKey entry, its chain or its cost.
func TestSpawnSession_RemovedMeanwhileStartsFresh(t *testing.T) {
	for _, tc := range []struct {
		name   string
		retire func(r *Router, key string)
	}{
		{"Remove", func(r *Router, key string) {
			if !r.Remove(key) {
				t.Fatal("Remove found no session")
			}
		}},
		{"Reset", func(r *Router, key string) { r.Reset(key) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGatedSpawn()
			r := spawnRouter(t, 4, g.hook)
			const key = "feishu:direct:spawn-removed:general"
			old := injectSession(r, key, newDeadProc())
			old.setSessionID("sid-old")
			storeTotalCost(&old.costSpent, 3.5)

			res := spawnAsync(r, key)
			waitEntered(t, g)
			tc.retire(r, key)
			close(g.release)

			got := waitResult(t, res)
			if got.err != nil {
				t.Fatalf("GetOrCreate: %v", got.err)
			}
			ids, procs := g.calls()
			if !slices.Equal(ids, []string{"sid-old", ""}) {
				t.Fatalf("spawn resume IDs = %q, want the stale resume then a fresh spawn", ids)
			}
			if procs[0].Alive() {
				t.Error("the process resuming the retired session was left running")
			}
			if got.s.loadProcess() != procs[1] || r.SessionFor(key) != got.s {
				t.Error("the installed session does not hold the fresh spawn's process")
			}
			if got.st != SessionNew {
				t.Errorf("status = %d, want SessionNew", got.st)
			}
			if sid := got.s.SessionID(); sid == "sid-old" {
				t.Error("the new session carries the retired session's ID")
			}
			if idResolves(r, "sid-old") {
				t.Error("the retired session's ID routes to the key again")
			}
			if ids := got.s.SnapshotPrevSessionIDs(); slices.Contains(ids, "sid-old") {
				t.Errorf("the new session continues the retired one's chain: %v", ids)
			}
			if c := loadTotalCost(&got.s.costSpent); c != 0 {
				t.Errorf("the new session inherited the retired one's spend: %v", c)
			}
		})
	}
}

// TestSpawnSession_StaleSpawnFlagsAShimSocketThatOutlivesTheWait: when the
// discarded spawn's shim socket is still there after the bounded wait, the
// retry's spawn error is wrapped as ErrShimStuck, as after a Reset.
func TestSpawnSession_StaleSpawnFlagsAShimSocketThatOutlivesTheWait(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	const key = "feishu:direct:spawn-stale-stuck:general"
	if err := os.WriteFile(shim.SocketPath(shim.KeyHash(key)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	g := newGatedSpawn()
	boom := errors.New("spawn failed")
	r := spawnRouter(t, 4, func(ctx context.Context, opts cli.SpawnOptions) (processIface, error) {
		if opts.ResumeID == "" {
			return nil, boom
		}
		return g.hook(ctx, opts)
	})
	injectSession(r, key, newDeadProc()).setSessionID("sid-old")

	res := spawnAsync(r, key)
	waitEntered(t, g)
	if !r.Remove(key) {
		t.Fatal("Remove found no session")
	}
	close(g.release)

	got := waitResult(t, res) // waits out the 2s socket-gone window
	if !errors.Is(got.err, ErrShimStuck) || !errors.Is(got.err, boom) {
		t.Errorf("GetOrCreate = %v, want ErrShimStuck wrapping the retry's spawn error", got.err)
	}
	if shimStuckLeft(r, key) {
		t.Error("the key is still flagged shim-stuck after the call")
	}
}

func shimStuckLeft(r *Router, key string) bool {
	var stuck bool
	r.ss.Update(func(tx sessTx) { stuck = tx.Ext().spawns.ShimStuck(key) })
	return stuck
}

// TestSpawnSession_StaleSpawnReplacedWithABoundSocketWrapsTheRetry: the
// retry after a replacement resumes the replacement, and its spawn error is
// still wrapped as ErrShimStuck without leaving the key flagged for a later
// call.
func TestSpawnSession_StaleSpawnReplacedWithABoundSocketWrapsTheRetry(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	const key = "feishu:direct:spawn-stale-replaced-stuck:general"
	if err := os.WriteFile(shim.SocketPath(shim.KeyHash(key)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	g := newGatedSpawn()
	boom := errors.New("spawn failed")
	r := spawnRouter(t, 4, func(ctx context.Context, opts cli.SpawnOptions) (processIface, error) {
		if opts.ResumeID == "sid-second" {
			return nil, boom
		}
		return g.hook(ctx, opts)
	})
	injectSession(r, key, newDeadProc()).setSessionID("sid-first")

	res := spawnAsync(r, key)
	waitEntered(t, g)
	second := &ManagedSession{key: key}
	second.storeProcess(newDeadProc())
	second.setSessionID("sid-second")
	r.ss.Update(func(tx sessTx) { tx.Put(key, second) })
	close(g.release)

	got := waitResult(t, res) // waits out the 2s socket-gone window
	if !errors.Is(got.err, ErrShimStuck) || !errors.Is(got.err, boom) {
		t.Errorf("GetOrCreate = %v, want ErrShimStuck wrapping the retry's spawn error", got.err)
	}
	if shimStuckLeft(r, key) {
		t.Error("the key is still flagged shim-stuck after the call")
	}
}

// retryGateCtx holds GetOrCreate at its post-stale ctx check once armed,
// then reports the next Done call: the round that parks on a spawn.
type retryGateCtx struct {
	context.Context
	armed   atomic.Bool
	held    chan struct{}
	resume  chan struct{}
	waiting chan struct{}
	passed  atomic.Bool
}

func (c *retryGateCtx) Err() error {
	if c.armed.CompareAndSwap(true, false) {
		close(c.held)
		<-c.resume
		c.passed.Store(true)
	}
	return c.Context.Err()
}

func (c *retryGateCtx) Done() <-chan struct{} {
	if c.passed.CompareAndSwap(true, false) {
		close(c.waiting)
	}
	return c.Context.Done()
}

// TestGetOrCreate_BoundStaleSocketWrapsOnlyTheNextRound: a round that parks
// on another caller's spawn consumes the stale round's bound socket, so a
// later spawn failure is not reported as ErrShimStuck.
func TestGetOrCreate_BoundStaleSocketWrapsOnlyTheNextRound(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	const key = "feishu:direct:spawn-stale-then-wait:general"
	if err := os.WriteFile(shim.SocketPath(shim.KeyHash(key)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	g := newGatedSpawn()
	boom := errors.New("spawn failed")
	otherEntered, otherRelease := make(chan struct{}), make(chan struct{})
	var fresh atomic.Int32
	r := spawnRouter(t, 4, func(ctx context.Context, opts cli.SpawnOptions) (processIface, error) {
		if opts.ResumeID != "" {
			return g.hook(ctx, opts)
		}
		if fresh.Add(1) == 1 {
			close(otherEntered)
			<-otherRelease
		}
		return nil, boom
	})
	injectSession(r, key, newDeadProc()).setSessionID("sid-old")

	ctx := &retryGateCtx{Context: context.Background(), held: make(chan struct{}),
		resume: make(chan struct{}), waiting: make(chan struct{})}
	out := make(chan spawnResult, 1)
	go func() {
		s, st, err := r.GetOrCreate(ctx, key, AgentOpts{})
		out <- spawnResult{s, st, err}
	}()
	waitEntered(t, g)
	if !r.Remove(key) {
		t.Fatal("Remove found no session")
	}
	ctx.armed.Store(true)
	close(g.release)
	waitClosed(t, ctx.held, "the stale round never reached its ctx check") // after the 2s socket-gone window

	other := spawnAsync(r, key)
	waitClosed(t, otherEntered, "the other caller never spawned")
	close(ctx.resume)
	waitClosed(t, ctx.waiting, "the retry never parked on the other spawn")
	close(otherRelease)

	if got := waitResult(t, other); !errors.Is(got.err, boom) {
		t.Fatalf("other GetOrCreate = %v, want its spawn error", got.err)
	}
	got := waitResult(t, out)
	if !errors.Is(got.err, boom) || errors.Is(got.err, ErrShimStuck) {
		t.Errorf("GetOrCreate = %v, want the spawn error without ErrShimStuck", got.err)
	}
	if n := fresh.Load(); n != 2 {
		t.Errorf("fresh spawns = %d, want the other caller's and the parked caller's", n)
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}

// TestGetOrCreate_CancelledDuringAStaleSpawnDoesNotRetry: a caller whose ctx
// ends while its spawn goes stale gets ctx's error instead of another spawn,
// and leaves no shim-stuck flag behind for the removed key.
func TestGetOrCreate_CancelledDuringAStaleSpawnDoesNotRetry(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	const key = "feishu:direct:spawn-stale-cancelled:general"
	if err := os.WriteFile(shim.SocketPath(shim.KeyHash(key)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	g := newGatedSpawn()
	// The spawn ignores ctx, so the cancel lands on the retry decision.
	r := spawnRouter(t, 4, func(_ context.Context, opts cli.SpawnOptions) (processIface, error) {
		return g.hook(context.Background(), opts)
	})
	injectSession(r, key, newDeadProc()).setSessionID("sid-old")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan spawnResult, 1)
	go func() {
		s, st, err := r.GetOrCreate(ctx, key, AgentOpts{})
		out <- spawnResult{s, st, err}
	}()
	waitEntered(t, g)
	cancel()
	if !r.Remove(key) {
		t.Fatal("Remove found no session")
	}
	close(g.release)

	got := waitResult(t, out) // waits out the 2s socket-gone window
	if !errors.Is(got.err, context.Canceled) {
		t.Errorf("GetOrCreate = %v, %v, want context.Canceled", got.s, got.err)
	}
	if ids, _ := g.calls(); len(ids) != 1 {
		t.Errorf("spawn calls = %q, want only the stale one", ids)
	}
	if shimStuckLeft(r, key) {
		t.Error("the removed key is left flagged shim-stuck")
	}
}

// TestSpawnSession_ReplacedMeanwhileResumesTheReplacement: when the key's
// dead session is replaced by another dead one during its resume spawn, the
// process resuming the first is closed and the replacement is resumed, so
// the conversation, ID and chain all describe the session in the table.
func TestSpawnSession_ReplacedMeanwhileResumesTheReplacement(t *testing.T) {
	g := newGatedSpawn()
	r := spawnRouter(t, 4, g.hook)
	const key = "feishu:direct:spawn-replaced:general"
	first := injectSession(r, key, newDeadProc())
	first.setSessionID("sid-first")

	res := spawnAsync(r, key)
	waitEntered(t, g)
	second := &ManagedSession{key: key}
	second.storeProcess(newDeadProc())
	second.setSessionID("sid-second")
	second.prevSessionIDs = []string{"sid-zero"}
	r.ss.Update(func(tx sessTx) {
		tx.Put(key, second)
	})
	close(g.release)

	got := waitResult(t, res)
	if got.err != nil {
		t.Fatalf("GetOrCreate: %v", got.err)
	}
	ids, procs := g.calls()
	if !slices.Equal(ids, []string{"sid-first", "sid-second"}) {
		t.Fatalf("spawn resume IDs = %q, want the stale resume then the replacement's", ids)
	}
	if procs[0].Alive() {
		t.Error("the process resuming the replaced session was left running")
	}
	if got.s.loadProcess() != procs[1] {
		t.Error("the installed session does not hold the second spawn's process")
	}
	if sid := got.s.SessionID(); sid != "sid-second" {
		t.Errorf("session ID = %q, want the replacement's sid-second", sid)
	}
	if idResolves(r, "sid-first") {
		t.Error("the replaced session's ID routes to the key")
	}
	if chain := got.s.SnapshotPrevSessionIDs(); !slices.Equal(chain, []string{"sid-zero"}) {
		t.Errorf("chain = %v, want the replacement's own [sid-zero]", chain)
	}
}

// TestSpawnSession_DroppedResumeRemovedMeanwhileInstalls: a resume the
// resume guard already turned into a fresh spawn is not tied to the entry it
// came from, so removing that entry during the spawn does not respawn.
func TestSpawnSession_DroppedResumeRemovedMeanwhileInstalls(t *testing.T) {
	g := newGatedSpawn()
	r := spawnRouter(t, 4, g.hook)
	const key = "feishu:direct:spawn-dropped:general"
	old := injectSession(r, key, newDeadProc())
	old.setSessionID("malformed/id") // the resume guard drops it

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
	ids, procs := g.calls()
	if !slices.Equal(ids, []string{""}) {
		t.Fatalf("spawn resume IDs = %q, want one fresh spawn", ids)
	}
	if got.s.loadProcess() != procs[0] || !procs[0].Alive() {
		t.Error("the fresh spawn's process was not installed")
	}
}

// TestSpawnSession_TakeoverIsNotRespawned: a takeover resumes the ID its
// caller supplied, not one read from the key's entry, so an entry appearing
// for the key during its spawn neither fails nor respawns it.
func TestSpawnSession_TakeoverIsNotRespawned(t *testing.T) {
	g := newGatedSpawn()
	r := spawnRouter(t, 4, g.hook)
	const key = "feishu:direct:spawn-takeover:general"

	type result struct {
		s   *ManagedSession
		err error
	}
	out := make(chan result, 1)
	go func() {
		s, err := r.Takeover(context.Background(), key, "sid-external", t.TempDir(), AgentOpts{})
		out <- result{s, err}
	}()
	waitEntered(t, g)
	other := injectSession(r, key, newDeadProc())
	other.setSessionID("sid-other")
	close(g.release)

	var got result
	select {
	case got = <-out:
	case <-time.After(5 * time.Second):
		t.Fatal("Takeover did not return")
	}
	if got.err != nil {
		t.Fatalf("Takeover: %v", got.err)
	}
	ids, procs := g.calls()
	if !slices.Equal(ids, []string{"sid-external"}) {
		t.Fatalf("spawn resume IDs = %q, want the one takeover spawn", ids)
	}
	if got.s.loadProcess() != procs[0] || got.s.SessionID() != "sid-external" {
		t.Errorf("takeover installed %q, want sid-external on its own process", got.s.SessionID())
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
	var (
		pt       pendingTuning
		ok       bool
		pending  int
		inFlight bool
	)
	r.ss.Update(func(v sessTx) {
		pt, ok = v.Ext().picks.tuning[key]
		pending = v.Ext().spawns.PendingSpawns()
		_, inFlight = v.Ext().spawns.SpawnInFlight(key)
	})
	if !ok || pt.Model != "claude-opus-5" {
		t.Errorf("the tuning pick was consumed by a failed spawn (ok=%v, %+v)", ok, pt)
	}
	if pending != 0 || inFlight {
		t.Errorf("after a failed spawn: pending=%d inFlight=%v, want 0/false", pending, inFlight)
	}

	// The retry consumes it onto the session.
	proc := newIdleProc()
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) { return proc, nil }
	s, _, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if s.TuningModel() != "claude-opus-5" {
		t.Errorf("the session's tuning model = %q, want the pick", s.TuningModel())
	}
	var still bool
	r.ss.Update(func(v sessTx) {
		_, still = v.Ext().picks.tuning[key]
	})
	if still {
		t.Error("the pick outlived the spawn that consumed it")
	}
}

// TestSpawnSession_ShutdownGateRefusesLateSpawns: once Shutdown has taken
// its snapshot no spawn starts a process.
func TestSpawnSession_ShutdownGateRefusesLateSpawns(t *testing.T) {
	called := false
	r := NewRouter(RouterConfig{MaxProcs: 4, Wrapper: cli.NewWrapper("/nonexistent/cli", &cli.ClaudeProtocol{}, "claude")})
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) {
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
	old := injectSession(r, key, newDeadProc())
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
	var stale bool
	r.ss.View(func(v sessView) {
		_, stale = v.KeyForID("sid-old")
	})
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
	victim := injectSession(r, "feishu:direct:victim:general", newHookCloseProc(func() { panic("close failed") }))
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
	r.ss.Update(func(v sessTx) {
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

// TestSpawnSession_LabelAndTuningWrittenInWindowSurvive: a label and a
// model/effort pick written to the dead session while its respawn is outside
// the lock are carried onto the new session, not dropped for the reserve-time
// values.
func TestSpawnSession_LabelAndTuningWrittenInWindowSurvive(t *testing.T) {
	g := newGatedSpawn()
	r := spawnRouter(t, 4, g.hook)
	const key = "feishu:direct:spawn-label:general"
	old := injectSession(r, key, newDeadProc())
	old.SetUserLabel("before")
	old.setLabelOrigin("user")

	res := spawnAsync(r, key)
	waitEntered(t, g)
	if !r.SetUserLabel(key, "after") {
		t.Fatal("SetUserLabel found no session")
	}
	model, effort := "claude-opus-5", "high"
	mode, err := r.SetSessionTuning(context.Background(), key, &model, &effort)
	if err != nil || mode != TuningAppliedDeferred {
		t.Fatalf("SetSessionTuning = %q, %v; want %q", mode, err, TuningAppliedDeferred)
	}
	close(g.release)

	got := waitResult(t, res)
	if got.err != nil {
		t.Fatalf("GetOrCreate: %v", got.err)
	}
	if r.SessionFor(key) != got.s {
		t.Fatal("the respawned session is not in the table")
	}
	if l, o := got.s.UserLabel(), got.s.LabelOrigin(); l != "after" || o != "user" {
		t.Errorf("label = %q (origin %q), want \"after\" (user)", l, o)
	}
	if m := got.s.TuningModel(); m != model {
		t.Errorf("tuning model = %q, want the pick made during the spawn %q", m, model)
	}
	if e := got.s.TuningEffort(); e != effort {
		t.Errorf("tuning effort = %q, want the pick made during the spawn %q", e, effort)
	}
}

// TestSpawnSession_LabelClearedInWindowStaysCleared: a label cleared while the
// respawn is outside the lock stays cleared, so AutoTitler can retake it.
func TestSpawnSession_LabelClearedInWindowStaysCleared(t *testing.T) {
	g := newGatedSpawn()
	r := spawnRouter(t, 4, g.hook)
	const key = "feishu:direct:spawn-label-clear:general"
	old := injectSession(r, key, newDeadProc())
	old.SetUserLabel("pinned")
	old.setLabelOrigin("user")

	res := spawnAsync(r, key)
	waitEntered(t, g)
	if !r.ClearUserLabelOrigin(key) {
		t.Fatal("ClearUserLabelOrigin found no session")
	}
	close(g.release)

	got := waitResult(t, res)
	if got.err != nil {
		t.Fatalf("GetOrCreate: %v", got.err)
	}
	if l, o := got.s.UserLabel(), got.s.LabelOrigin(); l != "" || o != "" {
		t.Errorf("label = %q (origin %q), want both cleared", l, o)
	}
}

// TestSpawnSession_ChainRefreshedInWindowSurvives: a cron stub whose chain is
// refreshed while its first spawn is outside the lock starts with the
// refreshed chain.
func TestSpawnSession_ChainRefreshedInWindowSurvives(t *testing.T) {
	g := newGatedSpawn()
	r := spawnRouter(t, 4, g.hook)
	const key = "cron:spawn-chain"
	ws := t.TempDir()
	r.RegisterCronStubWithChain(key, ws, "", []string{"a"})

	out := make(chan spawnResult, 1)
	go func() {
		s, st, err := r.GetOrCreate(context.Background(), key, AgentOpts{Exempt: true})
		out <- spawnResult{s, st, err}
	}()
	waitEntered(t, g)
	r.RegisterCronStubWithChain(key, ws, "", []string{"a", "b"})
	close(g.release)

	got := waitResult(t, out)
	if got.err != nil {
		t.Fatalf("GetOrCreate: %v", got.err)
	}
	if ids := got.s.SnapshotPrevSessionIDs(); !slices.Equal(ids, []string{"a", "b"}) {
		t.Errorf("chain = %v, want the refreshed [a b]", ids)
	}
}

func TestRespawnChain(t *testing.T) {
	long := make([]string, maxPrevSessionIDs)
	for i := range long {
		long[i] = "p" + string(rune('A'+i%26)) + string(rune('a'+i/26))
	}
	cases := []struct {
		name            string
		prev            []string
		oldID, resumeID string
		want            []string
	}{
		{"appends a rotated ID", []string{"a"}, "b", "", []string{"a", "b"}},
		{"same-ID resume does not append", []string{"a"}, "b", "b", []string{"a"}},
		{"empty old ID does not append", []string{"a"}, "", "", []string{"a"}},
		{"empty chain", nil, "", "", nil},
		{"capped to the most recent", long, "new", "", append(slices.Clone(long[1:]), "new")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := respawnChain(tc.prev, tc.oldID, tc.resumeID)
			if !slices.Equal(got, tc.want) {
				t.Errorf("respawnChain = %v, want %v", got, tc.want)
			}
			if len(got) > 0 && len(tc.prev) > 0 && &got[0] == &tc.prev[0] {
				t.Error("respawnChain aliases its input")
			}
		})
	}
}
