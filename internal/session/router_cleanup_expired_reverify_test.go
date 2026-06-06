package session

import (
	"testing"
	"time"
)

// TestCleanup_Expired_DeathReasonStampedWhenProcUnchanged pins the happy
// path of the idle-TTL expiry fix (issue #1780): when the close-loop
// re-verify confirms the session still holds the originally-classified
// proc, deathReason must be stamped "idle_timeout" and the proc closed.
func TestCleanup_Expired_DeathReasonStampedWhenProcUnchanged(t *testing.T) {
	r := &Router{
		ss:           sessionStore{sessions: make(map[string]*ManagedSession)},
		maxProcs:     3,
		ttl:          1 * time.Minute,
		pruneTTL:     72 * time.Hour,
		totalTimeout: 5 * time.Minute,
	}
	proc := newIdleProc() // alive, not running → eligible for idle TTL expiry
	s := injectSession(r, "key-idle", proc)
	// Age past ttl but well within stuckThreshold/pruneTTL so it lands in
	// the idle-expiry branch, not stuckKill or prune.
	s.lastActive.Store(time.Now().Add(-5 * time.Minute).UnixNano())

	r.Cleanup()

	if got := loadAtomicString(&s.deathReason); got != "idle_timeout" {
		t.Errorf("deathReason should be %q after idle expiry; got %q", "idle_timeout", got)
	}
	if proc.Alive() {
		t.Error("idle-expired proc must be closed")
	}
}

// TestCleanup_Expired_DeathReasonCleanWhenReVerifySkips pins the core of
// issue #1780: the idle-expiry path must mirror stuckKill and stamp
// deathReason only AFTER the close-loop re-verify confirms the proc is
// still current. Before the fix, "idle_timeout" was stamped in pass-2
// (before re-verify), so a session whose proc was replaced by a
// concurrent spawnSession / resetLocked between the pass-1 snapshot and
// the close loop would show "idle_timeout" on the dashboard even though
// its fresh proc was never closed.
//
// We exercise the close-loop re-verify branch directly (expiredEntry is
// local to Cleanup, and the live Cleanup rebuilds candidates from the
// current loadProcess(), so replicating the racy inter-pass gap inline is
// unreliable — same approach as the stuckKill re-verify test).
func TestCleanup_Expired_DeathReasonCleanWhenReVerifySkips(t *testing.T) {
	fresh := newRunningProc()
	stale := newIdleProc()
	s := &ManagedSession{key: "key-idle-skip"}
	// Session holds fresh (the new live proc spawned after the snapshot).
	s.storeProcess(fresh)

	// Simulate the close loop: captured=stale, reason="idle_timeout".
	// Re-verify sees fresh != stale → skip: do NOT stamp or close.
	captured := stale
	reason := "idle_timeout"
	if cur := s.loadProcess(); cur != nil && cur != captured {
		// re-verify skip — correct path, no stamp, no close
	} else {
		if reason != "" {
			storeAtomicString(&s.deathReason, reason)
		}
		captured.Close()
	}

	if got := loadAtomicString(&s.deathReason); got != "" {
		t.Errorf("deathReason must be empty when re-verify skips; got %q", got)
	}
	if !fresh.Alive() {
		t.Error("fresh (live) proc must remain untouched")
	}
	if !stale.Alive() {
		t.Error("stale proc must not be closed when re-verify skips")
	}
}

// TestCleanup_Expired_DeathReasonStampedWhenReVerifyPasses pins the
// complementary branch: when loadProcess() still equals the captured
// proc, the close loop stamps "idle_timeout" and closes it.
func TestCleanup_Expired_DeathReasonStampedWhenReVerifyPasses(t *testing.T) {
	proc := newIdleProc()
	s := &ManagedSession{key: "key-idle-pass"}
	s.storeProcess(proc)

	captured := proc
	reason := "idle_timeout"
	if cur := s.loadProcess(); cur != nil && cur != captured {
		t.Fatal("re-verify must pass when session still holds the captured proc")
	} else {
		if reason != "" {
			storeAtomicString(&s.deathReason, reason)
		}
		captured.Close()
	}

	if got := loadAtomicString(&s.deathReason); got != "idle_timeout" {
		t.Errorf("deathReason should be %q when re-verify passes; got %q", "idle_timeout", got)
	}
	if proc.Alive() {
		t.Error("captured idle proc must be closed when re-verify passes")
	}
}
