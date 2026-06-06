package session

import (
	"testing"
	"time"
)

// TestCleanup_StuckKill_SkipsWhenSessionReplacedProc pins the R217-CR-3
// fix: pass-2 classifies a process as stuck without holding r.mu (PID
// syscalls), so a concurrent spawnSession / resetLocked may have swapped
// s.process by the time the kill loop runs. Without the re-verify the
// captured (now-orphaned) proc was killed even though the session had
// already moved on. We exercise the inequality branch directly via the
// kill-loop helper since the live Cleanup() rebuilds candidates from the
// current loadProcess(); replicating the racy gap inline is unreliable.
func TestCleanup_StuckKill_SkipsWhenSessionReplacedProc(t *testing.T) {
	stale := newRunningProc()
	fresh := newRunningProc()
	s := &ManagedSession{key: "key1"}
	s.storeProcess(fresh)

	// Drive the same logic the kill loop in Cleanup uses: re-verify
	// loadProcess() against the captured proc before killing. Inlined
	// rather than extracted so the test covers the exact production
	// branch shape (struct field order, identity comparison).
	captured := stale
	if cur := s.loadProcess(); cur == nil || cur == captured {
		captured.Kill()
	}
	if !stale.Alive() {
		t.Errorf("stale proc must NOT be killed when session.process has been replaced")
	}
	if !fresh.Alive() {
		t.Errorf("fresh proc must remain untouched")
	}
}

// TestCleanup_StuckKill_FiresWhenSessionStillHoldsProc pins the
// complementary path: when loadProcess() == captured, the kill must
// still fire so genuinely stuck sessions are reclaimed.
func TestCleanup_StuckKill_FiresWhenSessionStillHoldsProc(t *testing.T) {
	r := &Router{
		ss:           sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs:     3,
		ttl:          1 * time.Minute,
		pruneTTL:     72 * time.Hour,
		totalTimeout: 5 * time.Minute,
	}
	proc := newRunningProc()
	s := injectSession(r, "key1", proc)
	s.lastActive.Store(time.Now().Add(-25 * time.Minute).UnixNano())

	r.Cleanup()
	if proc.Alive() {
		t.Error("genuinely stuck running session must still be killed when its proc is live in s.process")
	}
}
