package session

import (
	"testing"
)

// TestTryReattachProcessNoCallback_SendMuGuard pins the R51-CONCUR-002 (#750)
// behaviour: the guarded reattach succeeds only when sendMu is free, and
// returns false (defers the reconnect) when an in-flight Send holds it.
func TestTryReattachProcessNoCallback_SendMuGuard(t *testing.T) {
	t.Parallel()

	s := &ManagedSession{key: "sm750"}
	proc := NewTestProcess()

	// sendMu free → reattach succeeds and the process is published.
	if !s.tryReattachProcessNoCallback(proc, "sid-1") {
		t.Fatal("tryReattachProcessNoCallback returned false with sendMu free")
	}
	if s.loadProcess() != proc {
		t.Error("process was not attached after a successful tryReattach")
	}

	// sendMu held (simulating an in-flight Send unwinding on the dying
	// process) → reattach must defer rather than race the swap.
	s.sendMu.Lock()
	newProc := NewTestProcess()
	if s.tryReattachProcessNoCallback(newProc, "sid-2") {
		s.sendMu.Unlock()
		t.Fatal("tryReattachProcessNoCallback returned true while sendMu was held — " +
			"R51-CONCUR-002 (#750) regression: the swap raced an in-flight Send")
	}
	// The earlier process must remain attached (no partial swap).
	if s.loadProcess() != proc {
		s.sendMu.Unlock()
		t.Error("deferred tryReattach must not have replaced the live process")
	}
	s.sendMu.Unlock()

	// Once sendMu is free again the reattach goes through.
	if !s.tryReattachProcessNoCallback(newProc, "sid-2") {
		t.Fatal("tryReattachProcessNoCallback returned false after sendMu released")
	}
	if s.loadProcess() != newProc {
		t.Error("process was not swapped after sendMu became free")
	}
}
