package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/shim"
)

// startShutdownOn starts Shutdown and returns once it is waiting on probe's
// session running, with the table lock released into that wait.
func startShutdownOn(r *Router, probe *runningProbe) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		r.Shutdown()
		close(done)
	}()
	<-probe.asked
	return done
}

func waitShutdown(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Shutdown was not woken by %s", what)
	}
}

// TestRemove_ReleasesTheActiveSlot: removing a session with a live process
// gives its slot back, so the router does not refuse spawns it has room for.
func TestRemove_ReleasesTheActiveSlot(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	r := newTestRouter(4)
	const key = "feishu:direct:remove-active:general"
	injectLocked(r, key, newIdleProc())
	if got := r.ss.Active(); got != 1 {
		t.Fatalf("active = %d before Remove, want 1", got)
	}
	if !r.Remove(key) {
		t.Fatal("Remove reported the key absent")
	}
	if got := r.ss.Active(); got != 0 {
		t.Errorf("active = %d after Remove, want 0", got)
	}
}

// TestRemove_WakesAShutdownWaitingOnTheRemovedSession: a Shutdown waiting for
// a running session re-checks once that session is removed, instead of
// sleeping until its timeout.
func TestRemove_WakesAShutdownWaitingOnTheRemovedSession(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	r := newTestRouter(4)
	const key = "feishu:direct:remove-running:general"
	probe := &runningProbe{fakeProcess: newRunningProc(), asked: make(chan struct{})}
	injectLocked(r, key, probe)

	done := startShutdownOn(r, probe)
	r.Remove(key)
	waitShutdown(t, done, "the removal of the running session")
}

// TestCleanup_WakesAShutdownWaitingOnTheSessionItKills: Cleanup's stuck-kill
// of a running session wakes a Shutdown waiting on it.
func TestCleanup_WakesAShutdownWaitingOnTheSessionItKills(t *testing.T) {
	r := newTestRouter(4)
	const key = "feishu:direct:cleanup-stuck:general"
	probe := &runningProbe{fakeProcess: newRunningProc(), asked: make(chan struct{})}
	s := injectLocked(r, key, probe)
	s.lastActive.Store(time.Now().Add(-24 * time.Hour).UnixNano()) // running far past 2×totalTimeout

	done := startShutdownOn(r, probe)
	r.Cleanup()
	if probe.Alive() {
		t.Fatal("Cleanup did not kill the stuck running session")
	}
	waitShutdown(t, done, "Cleanup killing the running session")
}

// killHookProc is a fakeProcess whose Kill runs a hook first.
type killHookProc struct {
	*fakeProcess
	onKill func()
}

func (p *killHookProc) Kill() {
	p.onKill()
	p.fakeProcess.Kill()
}

// TestCleanup_PruneRechecksEachCandidate: a prune candidate that gained a
// conversation between Cleanup's snapshot and its prune is kept. Cleanup kills
// stuck sessions before it prunes, so the stuck session's Kill is where the
// candidate gains its session ID.
func TestCleanup_PruneRechecksEachCandidate(t *testing.T) {
	r := newTestRouter(4)
	old := time.Now().Add(-100 * time.Hour).UnixNano() // past pruneTTL and 2×totalTimeout
	const stubKey = "feishu:direct:prune-stub:general"
	stub := injectLocked(r, stubKey, nil)
	stub.lastActive.Store(old)
	stuck := injectLocked(r, "feishu:direct:prune-stuck:general", &killHookProc{
		fakeProcess: newRunningProc(),
		onKill:      func() { stub.setSessionID("sess-gained") },
	})
	stuck.lastActive.Store(old)

	r.Cleanup()
	if r.ss.Load(stubKey) == nil {
		t.Error("Cleanup pruned a session that gained a session ID after its snapshot")
	}
}

// TestSaveDirty_ClearsOnlyWhatDidNotChangeDuringTheWrite: a periodic save
// clears the session and workspace dirty flags when nothing changed since
// its snapshot, and keeps each set when that store changed meanwhile, so the
// change is written by the next save.
func TestSaveDirty_ClearsOnlyWhatDidNotChangeDuringTheWrite(t *testing.T) {
	r := newTestRouter(4)
	r.storePath = filepath.Join(t.TempDir(), "sessions.json")
	injectLocked(r, "feishu:direct:save:general", newIdleProc())
	dirtyBoth := func() {
		r.ss.Update(func(tx sessTx) { tx.MarkChanged() })
		r.SetWorkspace("feishu:direct:save", t.TempDir())
	}
	snapshot := func() (snap saveSnapshot) {
		r.ss.View(func(v sessView) { snap = r.dirtySaveSnapshot(v) })
		return snap
	}
	dirty := func() (sessions, workspaces bool) {
		r.ss.View(func(v sessView) { sessions, workspaces = v.Dirty(), v.Ext().workspaces.Dirty() })
		return sessions, workspaces
	}

	dirtyBoth()
	snap := snapshot()
	dirtyBoth() // both stores change while the snapshot is being written
	r.saveDirty(snap)
	if s, w := dirty(); !s || !w {
		t.Errorf("after a save that raced changes: dirty sessions=%v workspaces=%v, want both still dirty", s, w)
	}

	r.saveDirty(snapshot())
	if s, w := dirty(); s || w {
		t.Errorf("after an uncontended save: dirty sessions=%v workspaces=%v, want both clean", s, w)
	}
}

// TestAdoptShimTarget: a live shim is adopted only when its key still has
// neither a session nor a spawn in flight at adopt time.
func TestAdoptShimTarget(t *testing.T) {
	state := func(key string) shim.State {
		return shim.State{Key: key, SessionID: "sess-" + key, Workspace: "/tmp/ws", Backend: "claude", ShimPID: 4242}
	}

	t.Run("a session installed meanwhile wins", func(t *testing.T) {
		r := newTestRouter(4)
		const key = "feishu:direct:adopt-existing:general"
		existing := injectLocked(r, key, newDeadProc())
		tgt, adopted := r.adoptShimTarget(state(key), "claude")
		if adopted || !tgt.found || tgt.sess != existing {
			t.Errorf("adopted=%v found=%v sess==existing=%v, want the existing session", adopted, tgt.found, tgt.sess == existing)
		}
		if r.ss.Load(key) != existing {
			t.Error("the existing session was replaced by an adopted copy")
		}
	})

	t.Run("a spawn in flight is left to finish", func(t *testing.T) {
		r := newTestRouter(4)
		const key = "feishu:direct:adopt-spawning:general"
		r.ss.Update(func(tx sessTx) { tx.Ext().spawns.BeginSpawn(key) })
		tgt, adopted := r.adoptShimTarget(state(key), "claude")
		if adopted || !tgt.spawning || tgt.found {
			t.Errorf("adopted=%v spawning=%v found=%v, want a skip on the spawn", adopted, tgt.spawning, tgt.found)
		}
		if r.ss.Load(key) != nil {
			t.Error("a session was published over the spawn in flight")
		}
	})

	t.Run("an absent key is adopted", func(t *testing.T) {
		r := newTestRouter(4)
		const key = "feishu:direct:adopt-absent:general"
		tgt, adopted := r.adoptShimTarget(state(key), "claude")
		if !adopted || !tgt.found || tgt.sess == nil || r.ss.Load(key) != tgt.sess {
			t.Errorf("adopted=%v found=%v, want the shim published as the key's session", adopted, tgt.found)
		}
	})
}

// TestCommitShimReattach: a reconnected process attaches only to the session
// reconnect prepared it for, and an attach counts the session as active.
func TestCommitShimReattach(t *testing.T) {
	const key = "feishu:direct:reattach:general"
	st := shim.State{Key: key, SessionID: "sess-reattach"}

	t.Run("attaches and counts", func(t *testing.T) {
		r := newTestRouter(4)
		sess := injectLocked(r, key, newDeadProc())
		proc := newIdleProc()
		if got := r.commitShimReattach(st, sess, proc, "claude", r.bkStore.wrapper); got != reattachDone {
			t.Fatalf("outcome = %d, want reattachDone", got)
		}
		if sess.loadProcess() != proc {
			t.Error("the process was not attached")
		}
		var owner string
		var dirty bool
		r.ss.View(func(v sessView) { owner, _ = v.KeyForID(st.SessionID); dirty = v.Dirty() })
		if got := r.ss.Active(); got != 1 || owner != key || !dirty {
			t.Errorf("active=%d id owner=%q dirty=%v, want 1, %q, true", got, owner, dirty, key)
		}
	})

	t.Run("refuses a replaced session", func(t *testing.T) {
		r := newTestRouter(4)
		stale := injectLocked(r, key, newDeadProc())
		current := injectLocked(r, key, newDeadProc())
		proc := newIdleProc()
		if got := r.commitShimReattach(st, stale, proc, "claude", r.bkStore.wrapper); got != reattachReplaced {
			t.Fatalf("outcome = %d, want reattachReplaced", got)
		}
		if stale.loadProcess() == proc || current.loadProcess() == proc || r.ss.Active() != 0 {
			t.Error("a process was attached to a session reconnect did not prepare it for")
		}
	})

	t.Run("refuses a session that came alive", func(t *testing.T) {
		r := newTestRouter(4)
		live := newIdleProc()
		sess := injectLocked(r, key, live)
		if got := r.commitShimReattach(st, sess, newIdleProc(), "claude", r.bkStore.wrapper); got != reattachReplaced {
			t.Fatalf("outcome = %d, want reattachReplaced", got)
		}
		if sess.loadProcess() != live {
			t.Error("the live process was swapped out")
		}
	})

	t.Run("defers while a send is in flight", func(t *testing.T) {
		r := newTestRouter(4)
		sess := injectLocked(r, key, newDeadProc())
		sess.sendMu.Lock()
		defer sess.sendMu.Unlock()
		proc := newIdleProc()
		if got := r.commitShimReattach(st, sess, proc, "claude", r.bkStore.wrapper); got != reattachSendInFlight {
			t.Fatalf("outcome = %d, want reattachSendInFlight", got)
		}
		if sess.loadProcess() == proc || r.ss.Active() != 0 {
			t.Error("the process was attached under an in-flight send")
		}
	})
}

// TestSettleReconnected_ConvergesTheActiveCount: a reconcile tick that
// reattached sessions recounts the active slots from the table.
func TestSettleReconnected_ConvergesTheActiveCount(t *testing.T) {
	r := newTestRouter(4)
	injectLocked(r, "feishu:direct:settle:general", newIdleProc())
	r.ss.Update(func(tx sessTx) { tx.SetActive(3) }) // drifted
	r.settleReconnected(1)
	if got := r.ss.Active(); got != 1 {
		t.Errorf("active = %d after settling, want 1", got)
	}
}
