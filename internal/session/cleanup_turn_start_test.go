package session

import (
	"testing"
	"time"
)

// Cleanup classifies a session idle from pass 1's snapshot; a message that
// reached it since (a Send queued or holding sendMu) must keep its process,
// or the turn fails with "process exited" and idle_timeout on the session.
func TestCleanup_IdleCloseSkipsAStartingTurn(t *testing.T) {
	cases := map[string]func(s *ManagedSession) (undo func()){
		"send queued": func(s *ManagedSession) func() {
			s.turnWaiters.Add(1)
			return func() { s.turnWaiters.Add(-1) }
		},
		"send holding sendMu": func(s *ManagedSession) func() {
			s.sendMu.Lock()
			return s.sendMu.Unlock
		},
	}
	for name, start := range cases {
		t.Run(name, func(t *testing.T) {
			r := newTestRouter(4)
			proc := newIdleProc()
			s := injectSession(r, "feishu:direct:idle-start:general", proc)
			s.lastActive.Store(time.Now().Add(-2 * time.Hour).UnixNano()) // past the 30m TTL
			undo := start(s)
			r.Cleanup()
			undo()
			if !proc.Alive() {
				t.Fatal("Cleanup closed the process of a session whose turn was starting")
			}
			if dr := loadAtomicString(&s.deathReason); dr != "" {
				t.Errorf("deathReason = %q, want none", dr)
			}
		})
	}

	// Control: the same idle session with no turn is still closed.
	r := newTestRouter(4)
	proc := newIdleProc()
	s := injectSession(r, "feishu:direct:idle-plain:general", proc)
	s.lastActive.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	r.Cleanup()
	if proc.Alive() {
		t.Fatal("Cleanup kept an idle session past its TTL")
	}
}

// evictOldest frees a slot from an idle CLI; one with a Send already queued
// for it is about to run a turn and is not idle.
func TestEvictOldest_SkipsAQueuedTurn(t *testing.T) {
	r := newTestRouter(2)
	queuedProc, otherProc := newIdleProc(), newIdleProc()
	queued := injectSession(r, "dashboard:direct:user:queued", queuedProc)
	queued.lastActive.Store(1) // oldest
	queued.turnWaiters.Add(1)
	other := injectSession(r, "dashboard:direct:user:other", otherProc)
	other.lastActive.Store(2)

	var evicted bool
	r.ss.Update(func(tx sessTx) { evicted = r.evictOldest(tx) })
	if !evicted {
		t.Fatal("evictOldest returned false, want the idle session evicted")
	}
	if !queuedProc.Alive() {
		t.Error("evictOldest closed a session with a turn queued")
	}
	if otherProc.Alive() {
		t.Error("evictOldest kept the idle session")
	}
}
