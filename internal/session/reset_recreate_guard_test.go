package session

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestSpawnSession_ReusesPreInstalledSpawningKey pins the #775 (R62-GO-3)
// fix invariant: when r.pp.spawningKeys[key] already contains a channel
// (typically pre-installed by ResetAndRecreate before releasing r.mu for
// proc.Close), spawnSession's prologue MUST reuse that channel rather than
// overwrite it. Overwriting would orphan any GetOrCreate goroutine parked on
// the original channel — they'd never wake — and would also reopen the race
// window where a concurrent GetOrCreate observes "no inflight marker" and
// spawns its own session with mismatched opts.
//
// This is a structural test: we install a guardCh, call spawnSession (which
// fails because the wrapper points at a nonexistent binary), and assert that
// the channel that was closed during teardown is the SAME channel we
// installed. If the prologue ever drifts back to unconditional overwrite,
// this test fails immediately.
func TestSpawnSession_ReusesPreInstalledSpawningKey(t *testing.T) {
	r := newTestRouter(5)
	key := "feishu:direct:reset-recreate-guard:general"

	// Mirror ResetAndRecreate: install a guardCh in spawningKeys before
	// the spawn call. Caller of spawnSession is required to enter with
	// r.mu held.
	guardCh := make(chan struct{})
	r.mu.Lock()
	if r.pp.spawningKeys == nil {
		r.pp.spawningKeys = make(map[string]chan struct{})
	}
	r.pp.spawningKeys[key] = guardCh

	// spawnSession will fail because newTestRouter's wrapper points at
	// /nonexistent/cli-binary, but its defer (markSpawnDoneLocked) still
	// runs and closes whichever channel it captured into doneCh. With the
	// fix, that channel IS our guardCh.
	_, _ = r.spawnSession(context.Background(), key, "", AgentOpts{})
	// spawnSession unlocks on error; ensure unlocked before re-locking.

	// Verify the guardCh we installed was the one that got closed.
	// A closed channel returns immediately on receive; an unclosed one
	// blocks forever, so a short timeout disambiguates.
	select {
	case <-guardCh:
		// ok — fix is in place
	case <-time.After(2 * time.Second):
		t.Fatal("guardCh was not closed by spawnSession's defer; " +
			"prologue likely overwrote r.pp.spawningKeys[key] with a fresh channel " +
			"(regression of #775 / R62-GO-3 fix)")
	}

	// And the map entry should be cleared so the next caller can spawn.
	r.mu.Lock()
	if _, stillPresent := r.pp.spawningKeys[key]; stillPresent {
		r.mu.Unlock()
		t.Fatal("r.pp.spawningKeys[key] still present after spawnSession returned; " +
			"markSpawnDoneLocked failed to delete the (possibly reused) entry")
	}
	r.mu.Unlock()
}

// TestSpawnSession_FreshKeyInstallsOwnChannel pins the symmetric invariant:
// when no caller pre-installed an entry, spawnSession's prologue creates
// and installs its own channel (the original behaviour). Without this,
// GetOrCreate's inflight-wait path would never see a marker and would
// stack duplicate spawns on the shim socket.
func TestSpawnSession_FreshKeyInstallsOwnChannel(t *testing.T) {
	r := newTestRouter(5)
	key := "feishu:direct:fresh-key-installs:general"

	// No pre-installed entry. spawnSession must create one.
	r.mu.Lock()
	_, _ = r.spawnSession(context.Background(), key, "", AgentOpts{})

	// On error path spawnSession unlocks; relock to inspect map state.
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, present := r.pp.spawningKeys[key]; present {
		t.Fatal("r.pp.spawningKeys[key] leaked after spawnSession failed " +
			"(defer should close+delete)")
	}
}

// TestResetAndRecreate_ConcurrentGetOrCreateBlocksOnGuard exercises the
// end-to-end #775 race: ResetAndRecreate is mid-tear-down with r.mu
// released, and a concurrent GetOrCreate must NOT race in to spawn its own
// session before ResetAndRecreate's spawnSession runs. With the guardCh in
// place the concurrent caller observes (no session, but inflight marker)
// and parks; without it the concurrent caller would observe (no session,
// no marker) and break out of the wait loop into its own spawnSession
// invocation — which is exactly the foot-gun the issue called out.
//
// The test simulates the unlock window deterministically: it pre-installs
// a guardCh exactly the way ResetAndRecreate does, launches concurrent
// GetOrCreate goroutines, and asserts that they ALL park on guardCh
// (rather than break the loop and spawn). We check by counting how many
// goroutines have entered the inflight-wait path via the spawningKeys
// map after a brief delay.
func TestResetAndRecreate_ConcurrentGetOrCreateBlocksOnGuard(t *testing.T) {
	r := newTestRouter(5)
	key := "feishu:direct:concurrent-blocks:general"

	// Stage: install guardCh as ResetAndRecreate does just before
	// proc.Close.
	guardCh := make(chan struct{})
	r.mu.Lock()
	if r.pp.spawningKeys == nil {
		r.pp.spawningKeys = make(map[string]chan struct{})
	}
	r.pp.spawningKeys[key] = guardCh
	r.mu.Unlock()

	// Launch N concurrent GetOrCreate. With the guard installed, none
	// should successfully break out of the inflight-wait loop before we
	// close the guard.
	const N = 5
	var wg sync.WaitGroup
	wg.Add(N)
	returned := make(chan struct{}, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _, _ = r.GetOrCreate(context.Background(), key, AgentOpts{})
			returned <- struct{}{}
		}()
	}

	// Give the goroutines time to enter the wait. None should have
	// returned yet — they must all be parked on guardCh.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-returned:
		t.Fatal("a concurrent GetOrCreate returned BEFORE the inflight " +
			"guardCh was closed; #775 race is open — the caller raced in " +
			"and spawned its own session with potentially mismatched opts")
	default:
		// ok — all parked
	}

	// Now release the guard. Goroutines wake, retry the loop, and (since
	// no session exists) fall through to their own spawnSession which
	// fails fast against newTestRouter's nonexistent binary.
	r.mu.Lock()
	close(guardCh)
	delete(r.pp.spawningKeys, key)
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// ok
	case <-time.After(3 * time.Second):
		t.Fatal("waiters did not drain after closing guardCh; close+delete may be broken")
	}
}
