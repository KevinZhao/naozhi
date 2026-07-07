package session

import (
	"os"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/shim"
)

// TestFinishRemoveCleanup_DoesNotReinsertShimStuck is the R20260622-LB-3
// (#2261) regression guard. Remove is a TERMINAL operation: the key is never
// recreated with the same key, and unregisterSessionLocked already delete()d
// the shimStuckOnReset entry as a leak-prevention clear (R090031-CR-5). The
// shimStuckOnReset flag is consumed ONLY by a subsequent same-key
// GetOrCreate, so re-inserting it inside finishRemoveCleanup (when the 2s
// socket-gone wait times out for a still-Alive proc) left a permanent map
// entry for every one-shot key (dashboard:direct:*, scratch, planner, cron) —
// a slow unbounded leak.
//
// This test drives the exact timeout branch: an Alive proc plus a socket file
// that never disappears (so waitSocketGoneForKey returns false), then asserts
// the flag is NOT present afterwards.
func TestFinishRemoveCleanup_DoesNotReinsertShimStuck(t *testing.T) {
	// Point the shim socket dir at a temp dir we control, then plant a file at
	// the computed socket path so WaitSocketGone (a pure os.Stat poll) keeps
	// finding it and times out after the 2s wait.
	runDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runDir)

	const key = "dashboard:direct:one-shot:general"
	sockPath := shim.SocketPath(shim.KeyHash(key))
	if err := os.WriteFile(sockPath, []byte("stuck"), 0600); err != nil {
		t.Fatalf("plant socket file: %v", err)
	}
	defer os.Remove(sockPath)

	r := NewRouter(RouterConfig{MaxProcs: 4, TTL: time.Hour})
	t.Cleanup(r.Shutdown)

	// Install a session whose proc reports Alive so finishRemoveCleanup enters
	// the proc.Close + socket-wait branch.
	installSession(t, r, key, newIdleProc())

	// Pre-condition: not flagged.
	r.mu.RLock()
	_, before := r.pp.shimStuckOnReset[key]
	r.mu.RUnlock()
	if before {
		t.Fatal("precondition: key should not be flagged before Remove")
	}

	// Synchronous Remove runs finishRemoveCleanup inline, including the full
	// 2s socket-gone timeout, so the assertion below is race-free.
	if !r.Remove(key) {
		t.Fatal("Remove returned false for present key")
	}

	r.mu.RLock()
	_, after := r.pp.shimStuckOnReset[key]
	r.mu.RUnlock()
	if after {
		t.Error("#2261: shimStuckOnReset[key] re-inserted by terminal Remove — unbounded map leak for one-shot keys")
	}
}
