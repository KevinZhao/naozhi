package session

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// A session idle past its TTL whose background tasks still run keeps its
// CLI: closing it kills them (11-08/11-09 a 40-minute trn2 job died with
// the idle expiry). Once they no longer run it expires as before.
func TestCleanupKeepsSessionWithBackgroundTasks(t *testing.T) {
	r := &Router{ss: newSessionTable(), maxProcs: 3, ttl: time.Minute, pruneTTL: 72 * time.Hour}
	proc := newIdleProc()
	s := injectSession(r, "key1", proc)
	s.lastActive.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	open := true
	r.SetBackgroundCheck(func(got *ManagedSession) bool { return got == s && open })

	r.Cleanup()
	if !proc.Alive() {
		t.Fatal("idle expiry closed the CLI of a session whose background tasks still run")
	}
	open = false
	r.Cleanup()
	if proc.Alive() {
		t.Fatal("an idle session with no background task left was not expired")
	}
	if got := loadAtomicString(&s.deathReason); got != DeathReasonIdleTimeout {
		t.Errorf("deathReason = %q, want %q", got, DeathReasonIdleTimeout)
	}
}

// The eviction for max_procs passes over a session whose background tasks
// still run, and evicts none when every idle one has them.
func TestEvictOldestSkipsBackgroundTasks(t *testing.T) {
	r := newTestRouter(2)
	olderProc, newerProc := newIdleProc(), newIdleProc()
	older := injectSession(r, "dashboard:direct:user:a", olderProc)
	older.lastActive.Store(1)
	newer := injectSession(r, "dashboard:direct:user:b", newerProc)
	newer.lastActive.Store(2)
	bg := map[*ManagedSession]bool{older: true}
	r.SetBackgroundCheck(func(s *ManagedSession) bool { return bg[s] })

	var evicted bool
	r.ss.Update(func(tx sessTx) {
		if !takeoverHasSlot(tx.View, "x", 2, r.backgroundOpen) {
			t.Error("takeoverHasSlot: the session without background tasks is evictable")
		}
		evicted = r.evictOldest(tx)
	})
	if !evicted || !olderProc.Alive() || newerProc.Alive() {
		t.Fatalf("evicted %v, older alive %v, newer alive %v; want the newer one evicted", evicted, olderProc.Alive(), newerProc.Alive())
	}

	r2 := newTestRouter(1)
	p := newIdleProc()
	only := injectSession(r2, "dashboard:direct:user:c", p)
	r2.SetBackgroundCheck(func(s *ManagedSession) bool { return s == only })
	r2.ss.Update(func(tx sessTx) {
		if takeoverHasSlot(tx.View, "x", 1, r2.backgroundOpen) {
			t.Error("takeoverHasSlot: a session with background tasks is not evictable")
		}
		evicted = r2.evictOldest(tx)
	})
	if evicted || !p.Alive() {
		t.Fatalf("evicted %v alive %v; want none evicted", evicted, p.Alive())
	}
}

// A process's end reaches the end sink with the session's death reason; a
// CLI naozhi detached, or whose shim outlived the socket, did not end.
func TestEndSinkGetsDeathReason(t *testing.T) {
	r := wfRouter(t)
	type got struct{ key, reason string }
	var ends []got
	r.SetEndSink(func(key, reason string) { ends = append(ends, got{key, reason}) })
	for _, c := range []struct {
		end  cli.ProcessEnd
		want bool
	}{
		{cli.ProcessEnd{Detached: true}, false},
		{cli.ProcessEnd{ShimLive: true}, false},
		{cli.ProcessEnd{}, true},
	} {
		ends = nil
		s := injectSession(r, "feishu:direct:alice:general", nil)
		proc := newWFProc()
		storeAtomicString(&s.deathReason, DeathReasonIdleTimeout)
		bookProcessEnd(s, proc, "", r.endFor(s, proc))
		proc.finish(c.end)
		if !c.want {
			if len(ends) != 0 {
				t.Errorf("%+v: ends = %+v, want none", c.end, ends)
			}
			continue
		}
		if len(ends) != 1 || ends[0] != (got{"feishu:direct:alice:general", DeathReasonIdleTimeout}) {
			t.Errorf("%+v: ends = %+v", c.end, ends)
		}
	}
}
