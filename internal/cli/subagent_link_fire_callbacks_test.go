package cli

import (
	"context"
	"testing"
	"time"
)

// TestFireCallbacksDropLock_NoCallbacks pins [R112714-PERF-5]: when no
// OnResolve callbacks are registered, fireCallbacksDropLock must return early
// without allocating the copy slice. We verify there is no panic and the
// Resolve path succeeds (i.e. l.mu is correctly re-locked on return).
func TestFireCallbacksDropLock_NoCallbacks(t *testing.T) {
	t.Parallel()
	const sessionID = "deadbeef-0000-1111-2222-333344445555"
	l, subagentDir := newLinkerForTest(t, sessionID)
	// No OnResolve callback registered — fireCallbacksDropLock must not
	// panic and must leave l.mu properly Unlocked so the deferred Unlock
	// in Resolve's step 7 runs without double-unlocking.
	now := time.Now()
	writeAgentFiles(t, subagentDir, "ffeeddccbbaa00112", "no-cb-worker", sessionID, "p-nocb", now)
	toolUseTime := now.Add(-50 * time.Millisecond).UnixMilli()
	info, resolved := l.Resolve(context.Background(), "t_nocb", "toolu_NB", "no-cb-worker", "", toolUseTime)
	if !resolved {
		t.Fatalf("Resolve should succeed even with no callbacks: %+v", info)
	}
}

// TestFireCallbacksDropLock_WithCallback verifies that registering a callback
// still causes it to fire after the PERF-5 early-exit guard.
func TestFireCallbacksDropLock_WithCallback(t *testing.T) {
	t.Parallel()
	const sessionID = "12345678-aaaa-bbbb-cccc-ddddeeeeffff"
	l, subagentDir := newLinkerForTest(t, sessionID)

	fired := make(chan string, 1)
	l.OnResolve(func(taskID, toolUseID, internalAgentID string) {
		fired <- internalAgentID
	})

	now := time.Now()
	writeAgentFiles(t, subagentDir, "1122334455667788a", "cb-worker", sessionID, "p-cb", now)
	toolUseTime := now.Add(-50 * time.Millisecond).UnixMilli()
	info, resolved := l.Resolve(context.Background(), "t_cb", "toolu_CB", "cb-worker", "", toolUseTime)
	if !resolved {
		t.Fatalf("Resolve should succeed: %+v", info)
	}

	select {
	case got := <-fired:
		if got != "agent-1122334455667788a" {
			t.Errorf("callback got internalAgentID=%q, want agent-1122334455667788a", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("OnResolve callback was never fired")
	}
}
