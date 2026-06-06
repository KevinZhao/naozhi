package session

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// countingProc wraps fakeProcess to count GetState/IsRunning invocations so
// the R220-PERF-4 test can assert that pass-2 reads from the cached state
// rather than re-locking proc.mu.
type countingProc struct {
	*fakeProcess
	getStateCalls  atomic.Int64
	isRunningCalls atomic.Int64
}

func newCountingRunningProc() *countingProc {
	return &countingProc{fakeProcess: newRunningProc()}
}

func (c *countingProc) State() cli.ProcessState {
	c.getStateCalls.Add(1)
	return c.fakeProcess.State()
}

func (c *countingProc) IsRunning() bool {
	c.isRunningCalls.Add(1)
	return c.fakeProcess.IsRunning()
}

// TestCleanup_PassTwo_UsesCachedState pins R220-PERF-4: pass-2 must derive
// the running classification from the state snapshot taken once in pass-1
// (under r.mu.RLock) instead of re-acquiring proc.mu.RLock. We verify this
// by counting IsRunning() invocations on a fakeProcess that increments a
// counter on every call. After Cleanup the count must remain zero.
func TestCleanup_PassTwo_UsesCachedState(t *testing.T) {
	r := &Router{
		ss:           sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs:     3,
		ttl:          1 * time.Minute,
		pruneTTL:     72 * time.Hour,
		totalTimeout: 5 * time.Minute,
	}
	proc := newCountingRunningProc()
	s := injectSession(r, "key1", proc)
	// Force the candidate to land in pass-2 by aging lastActive past TTL.
	s.lastActive.Store(time.Now().Add(-25 * time.Minute).UnixNano())

	r.Cleanup()

	if got := proc.isRunningCalls.Load(); got != 0 {
		t.Errorf("IsRunning() called %d times in pass-2; want 0 (state must come from pass-1 GetState cache)", got)
	}
	if got := proc.getStateCalls.Load(); got != 1 {
		t.Errorf("GetState() called %d times; want exactly 1 (pass-1 snapshot)", got)
	}
}

// TestCleanup_PassTwo_RunningSessionFromCache verifies the running path:
// when pass-1 captures StateRunning, pass-2 must classify the session as
// running and (when sufficiently aged) flag it for stuckKill — proving
// the cached state actually drives classification.
func TestCleanup_PassTwo_RunningSessionFromCache(t *testing.T) {
	r := &Router{
		ss:           sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs:     3,
		ttl:          1 * time.Minute,
		pruneTTL:     72 * time.Hour,
		totalTimeout: 5 * time.Minute,
	}
	proc := newCountingRunningProc()
	s := injectSession(r, "key1", proc)
	// Age past 2*totalTimeout so the running-but-stuck branch fires.
	s.lastActive.Store(time.Now().Add(-25 * time.Minute).UnixNano())

	r.Cleanup()

	// Stuck-running classification must have killed the proc.
	if proc.Alive() {
		t.Error("running session aged past 2*totalTimeout must be killed via stuckKill path driven by cached state")
	}
}
