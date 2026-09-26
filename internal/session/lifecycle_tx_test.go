package session

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/shim"
)

// TestResetAndRecreate_ParksAConcurrentGetOrCreate drives ResetAndRecreate
// itself: while the old process is closing (outside the lock), a concurrent
// GetOrCreate for the key must wait for the recreate instead of spawning a
// session of its own with other opts (#775). One spawn in total.
func TestResetAndRecreate_ParksAConcurrentGetOrCreate(t *testing.T) {
	var spawns atomic.Int32
	r := spawnRouter(t, 4, func(context.Context, cli.SpawnOptions) (processIface, error) {
		spawns.Add(1)
		return newIdleProc(), nil
	})
	const key = "feishu:direct:recreate:general"
	closing := make(chan struct{})
	release := make(chan struct{})
	injectLocked(r, key, newHookCloseProc(func() {
		close(closing)
		<-release
	}))

	recreated := make(chan *ManagedSession, 1)
	go func() {
		s, err := r.ResetAndRecreate(context.Background(), key, AgentOpts{})
		if err != nil {
			t.Errorf("ResetAndRecreate: %v", err)
		}
		recreated <- s
	}()
	<-closing // the old process is being closed, outside the lock
	concurrent := spawnAsync(r, key)
	select {
	case res := <-concurrent:
		t.Fatalf("a concurrent GetOrCreate returned (%v, %v) while the recreate was closing the old process", res.s, res.err)
	case <-time.After(100 * time.Millisecond):
		// Asserting that something does not happen needs a window; a caller
		// that were not parked would have returned within it.
	}
	close(release)

	s := <-recreated
	got := waitResult(t, concurrent)
	if got.err != nil {
		t.Fatalf("concurrent GetOrCreate: %v", got.err)
	}
	if got.s != s {
		t.Error("the concurrent GetOrCreate did not pick up the recreated session")
	}
	if n := spawns.Load(); n != 1 {
		t.Errorf("%d spawns, want 1: the concurrent caller spawned its own session", n)
	}
}

// runningProbe is a running process that reports the first time anyone asks
// whether it is running — Shutdown's wait predicate does, holding the lock
// right before it waits.
type runningProbe struct {
	*fakeProcess
	asked chan struct{}
	once  sync.Once
}

func (p *runningProbe) IsRunning() bool {
	p.once.Do(func() { close(p.asked) })
	return p.fakeProcess.IsRunning()
}

// TestResetChat_ClosesTheChatsProcessesAndWakesShutdown: the chat's live
// processes are closed, and a Shutdown waiting on one of them running is woken
// by the reset rather than by its timeout.
func TestResetChat_ClosesTheChatsProcessesAndWakesShutdown(t *testing.T) {
	r := newTestRouter(4)
	running := &runningProbe{fakeProcess: newRunningProc(), asked: make(chan struct{})}
	injectLocked(r, "feishu:group:chatA:general", running)
	idle := newIdleProc()
	injectLocked(r, "feishu:group:chatA:other", idle)

	shutdownDone := make(chan struct{})
	go func() {
		r.Shutdown()
		close(shutdownDone)
	}()
	// Shutdown has checked the running session and holds the lock into its
	// Wait; the reset's transaction runs only once that Wait releases it.
	<-running.asked

	r.ResetChat("feishu:group:chatA")
	if running.Alive() || idle.Alive() {
		t.Error("ResetChat left a chat process running")
	}
	select {
	case <-shutdownDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown was not woken by the reset closing the running session")
	}
}

// TestReset_FlagsAShimSocketThatOutlivesTheWait: when the key's shim socket
// is still there after Reset's bounded wait, the next spawn error for the key
// is wrapped as ErrShimStuck.
func TestReset_FlagsAShimSocketThatOutlivesTheWait(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	const key = "feishu:direct:stuck:general"
	sock := shim.SocketPath(shim.KeyHash(key))
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("spawn failed")
	r := spawnRouter(t, 4, func(context.Context, cli.SpawnOptions) (processIface, error) { return nil, boom })
	injectLocked(r, key, newIdleProc())

	r.Reset(key) // waits out the 2s socket-gone window

	_, _, err := r.GetOrCreate(context.Background(), key, AgentOpts{})
	if !errors.Is(err, ErrShimStuck) || !errors.Is(err, boom) {
		t.Errorf("GetOrCreate after a Reset that left the socket = %v, want ErrShimStuck wrapping the spawn error", err)
	}
}
