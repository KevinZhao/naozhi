package session

import (
	"testing"
	"time"
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

// TestReattachKeepsKnownSessionID: a shim that has not seen an init or
// result frame since a --resume spawn reports no session ID; reattaching
// must keep the stored one, or the next save drops the session.
func TestReattachKeepsKnownSessionID(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "planner:main"}
	s.setSessionID("e6252386")
	s.ReattachProcessNoCallback(NewTestProcess(), "")
	if got := s.getSessionID(); got != "e6252386" {
		t.Fatalf("session ID = %q, want the stored one", got)
	}
	s.ReattachProcessNoCallback(NewTestProcess(), "f00d")
	if got := s.getSessionID(); got != "f00d" {
		t.Fatalf("session ID = %q, want the shim's", got)
	}
}

// Reattaching a shim after a restart is not activity: the last activity
// restored from the store stands; a session with none gets the attach time.
func TestReattachKeepsLastActive(t *testing.T) {
	t.Parallel()
	stored := time.Now().Add(-3 * time.Hour).UnixNano()
	tests := []struct {
		name   string
		stored int64
		keep   bool
	}{
		{"restored from the store", stored, true},
		{"none stored", 0, false},
	}
	for _, tt := range tests {
		s := &ManagedSession{key: "dashboard:direct:a:general"}
		s.lastActive.Store(tt.stored)
		s.ReattachProcessNoCallback(NewTestProcess(), "")
		if got := s.lastActive.Load(); (got == tt.stored) != tt.keep || got == 0 {
			t.Errorf("%s: last active = %d, stored %d", tt.name, got, tt.stored)
		}
	}
}

// The idle TTL counts from the reattach at the latest: the stored last
// activity can be past the TTL while the CLI only just got taken back.
func TestReattachedNotReapedByIdleTTL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		attachedAt time.Duration // before now
		reaped     bool
	}{
		{"just reattached", 0, false},
		{"reattached past the TTL", 40 * time.Minute, true},
	}
	for _, tt := range tests {
		r := wfRouter(t)
		r.ttl = 30 * time.Minute
		s := injectSession(r, "dashboard:direct:a:general", nil)
		s.lastActive.Store(time.Now().Add(-40 * time.Minute).UnixNano())
		proc := newIdleProc()
		proc.startedAt = time.Now().Add(-tt.attachedAt)
		r.ss.Update(func(tx sessTx) {
			s.ReattachProcessNoCallback(proc, "")
			tx.AddActive(1)
		})
		r.Cleanup()
		if got := !proc.Alive(); got != tt.reaped {
			t.Errorf("%s: reaped = %v, want %v", tt.name, got, tt.reaped)
		}
	}
}
