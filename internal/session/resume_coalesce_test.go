package session

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestGetOrCreate_ResumeCoalesceNoDoubleClose pins #2221: concurrent
// GetOrCreate on a DEAD session (isAlive()==false — a process attached but
// exited, the StateSuspended mid-state) must not race two spawnSession calls
// onto the same in-flight done-channel.
//
// Before the fix the resume branch called spawnSession directly, bypassing the
// spawningKeys coalesce guard the fresh-spawn path uses. The first caller
// installed a per-spawn done-channel (reused=false); a concurrent caller's
// spawnSession prologue then reused that same channel (reused=true) and both
// defers ran markSpawnDoneLocked → close of an already-closed channel → panic
// ("close of closed channel"). This is a process-killing panic, so the only
// assertion needed is "the concurrent storm completes without panicking".
//
// The test uses newTestRouter whose wrapper points at a nonexistent binary, so
// every spawn attempt fails fast — that is fine: the double-close was in the
// defer, independent of spawn success/failure. High fan-out plus repeated
// iterations widen the (A installed channel) → (B reuses it) window that the
// bug needs.
func TestGetOrCreate_ResumeCoalesceNoDoubleClose(t *testing.T) {
	const iterations = 100
	const fanout = 16
	for iter := 0; iter < iterations; iter++ {
		r := newTestRouter(fanout * 2)
		key := "feishu:direct:resume-coalesce:general"
		injectSession(r, key, newDeadProc())

		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < fanout; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				// Errors are expected (failing wrapper); a panic here would
				// crash the test binary, which is exactly the regression.
				_, _, _ = r.GetOrCreate(context.Background(), key, AgentOpts{})
			}()
		}
		close(start)
		wg.Wait()
	}
}

// TestGetOrCreate_DeadSessionParksOnInflightGuard pins the coalesce contract
// for #2221 deterministically: when a spawn is already in flight for a dead
// session's key (spawningKeys[key] holds an unclosed channel), concurrent
// GetOrCreate callers hitting the resume branch must PARK on that channel
// rather than each call spawnSession. Parking is what guarantees a single
// creator owns the done-channel close.
//
// We simulate the in-flight window the way ResetAndRecreate does: inject a
// dead session, pre-install a guard channel in spawningKeys, then launch
// concurrent GetOrCreate. None may return before the guard is closed; if one
// did, it broke out of the wait loop into its own spawnSession — the exact
// foot-gun that produced the double-close.
func TestGetOrCreate_DeadSessionParksOnInflightGuard(t *testing.T) {
	r := newTestRouter(5)
	key := "feishu:direct:dead-parks:general"
	injectSession(r, key, newDeadProc())

	guardCh := make(chan struct{})
	r.mu.Lock()
	if r.pp.spawningKeys == nil {
		r.pp.spawningKeys = make(map[string]chan struct{})
	}
	r.pp.spawningKeys[key] = guardCh
	r.mu.Unlock()

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

	// Give the goroutines time to reach the resume-branch coalesce park.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-returned:
		t.Fatal("a concurrent GetOrCreate on a DEAD session returned BEFORE " +
			"the in-flight guard was closed; the resume branch did not honour " +
			"the spawningKeys coalesce guard (#2221 — it would race a second " +
			"spawnSession onto the in-flight done-channel and double-close it)")
	default:
		// ok — all parked on guardCh
	}

	// Release the guard. Waiters wake, re-evaluate the loop, and (the dead
	// session is still present, no in-flight marker) fall through to their own
	// resume spawnSession, which fails fast against the nonexistent binary.
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
		t.Fatal("waiters did not drain after closing the guard; resume-branch " +
			"coalesce park may be broken")
	}
}
