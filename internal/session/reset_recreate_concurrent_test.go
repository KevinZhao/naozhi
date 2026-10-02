package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// Two ResetAndRecreate calls on one key, the second landing while the first
// has released the table lock to close the old process. The second must not
// spawn alongside the first — one key, one spawn in flight — and the guard
// both see must be closed exactly once.
func TestResetAndRecreate_ConcurrentSameKey_OneSpawnAtATime(t *testing.T) {
	r := newTestRouter(5)
	key := "feishu:direct:reset-twice:general"
	old := newBlockingCloseProc()
	injectSession(r, key, old)

	var inSpawn, maxInSpawn atomic.Int32
	spawnEntered := make(chan struct{}, 4)
	releaseSpawn := make(chan struct{})
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) {
		n := inSpawn.Add(1)
		for {
			m := maxInSpawn.Load()
			if n <= m || maxInSpawn.CompareAndSwap(m, n) {
				break
			}
		}
		spawnEntered <- struct{}{}
		<-releaseSpawn
		inSpawn.Add(-1)
		return newIdleProc(), nil
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(1)
	go func() { defer wg.Done(); _, errs[0] = r.ResetAndRecreate(context.Background(), key, AgentOpts{}) }()

	// A is closing the old process with the table lock released and its guard up.
	testhelper.Eventually(t, func() bool {
		var inflight, present bool
		r.ss.Update(func(tx sessTx) {
			_, inflight = tx.Ext().spawns.SpawnInFlight(key)
			_, present = tx.Lookup(key)
		})
		return inflight && !present
	}, 3*time.Second, "first ResetAndRecreate closing the old process with its guard up")
	wg.Add(1)
	go func() { defer wg.Done(); _, errs[1] = r.ResetAndRecreate(context.Background(), key, AgentOpts{}) }()

	// Let B run as far as it will before A's close finishes.
	select {
	case <-spawnEntered:
	case <-time.After(300 * time.Millisecond):
	}
	close(old.release)
	select {
	case <-spawnEntered:
	case <-time.After(300 * time.Millisecond):
	}
	close(releaseSpawn)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ResetAndRecreate calls did not return")
	}
	if m := maxInSpawn.Load(); m > 1 {
		t.Errorf("%d spawns ran for one key at once", m)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("ResetAndRecreate #%d: %v", i, err)
		}
	}
}

// A takeover that lands while another spawn for the key is in flight refuses
// with ErrSpawnInFlight, and the spawn in flight finishes untouched — before,
// the takeover joined it and the two ended its guard twice.
func TestTakeover_DuringInFlightSpawn_Refuses(t *testing.T) {
	r := newTestRouter(5)
	key := "feishu:direct:takeover-inflight:general"
	entered := make(chan struct{})
	release := make(chan struct{})
	r.spawn.hook = func(context.Context, cli.SpawnOptions) (processIface, error) {
		close(entered)
		<-release
		return newIdleProc(), nil
	}
	var created *ManagedSession
	var createErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		created, _, createErr = r.GetOrCreate(context.Background(), key, AgentOpts{})
	}()
	<-entered

	if _, err := r.Takeover(context.Background(), key, "external-sess", t.TempDir(), AgentOpts{}); !errors.Is(err, ErrSpawnInFlight) {
		t.Errorf("Takeover during a spawn = %v, want ErrSpawnInFlight", err)
	}
	close(release)
	<-done
	if createErr != nil || created == nil {
		t.Fatalf("the in-flight spawn: %v", createErr)
	}
	r.ss.Update(func(tx sessTx) {
		if _, inflight := tx.Ext().spawns.SpawnInFlight(key); inflight {
			t.Error("marker left behind")
		}
	})
}

// Waiting out another spawn is bounded by the caller's context.
func TestResetAndRecreate_WaitHonoursContext(t *testing.T) {
	r := newTestRouter(5)
	key := "feishu:direct:reset-wait-ctx:general"
	r.ss.Update(func(tx sessTx) { tx.Ext().spawns.BeginSpawn(key) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.ResetAndRecreate(ctx, key, AgentOpts{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ResetAndRecreate behind a spawn that never ends = %v, want the context's error", err)
	}
}
