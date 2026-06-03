package session

import (
	"sync"
	"testing"
	"time"
)

// blockingCloseProc is a fakeProcess whose Close() blocks on a release
// channel, letting a test prove RemoveAsync returns (and the session is
// gone from the map) BEFORE the slow teardown — proc.Close — finishes.
type blockingCloseProc struct {
	*fakeProcess
	release   chan struct{}
	closeDone chan struct{}
}

func newBlockingCloseProc() *blockingCloseProc {
	return &blockingCloseProc{
		fakeProcess: newIdleProc(),
		release:     make(chan struct{}),
		closeDone:   make(chan struct{}),
	}
}

func (b *blockingCloseProc) Close() {
	<-b.release // block until the test lets the teardown proceed
	b.fakeProcess.Close()
	close(b.closeDone)
}

func installSession(t *testing.T, r *Router, key string, proc processIface) *ManagedSession {
	t.Helper()
	s := &ManagedSession{key: key}
	s.setSessionID("sid-" + key)
	if proc != nil {
		s.storeProcess(proc)
	}
	r.mu.Lock()
	r.sessions[key] = s
	r.mu.Unlock()
	return s
}

// TestRemoveAsync_UnregistersImmediately locks the core contract: the
// session leaves r.sessions synchronously (so the dashboard list / new
// sends stop seeing it) and RemoveAsync returns true even while the slow
// proc.Close teardown is still blocked in its goroutine.
func TestRemoveAsync_UnregistersImmediately(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 4, TTL: time.Hour})
	t.Cleanup(r.Shutdown)

	const key = "test:direct:k1:general"
	proc := newBlockingCloseProc()
	installSession(t, r, key, proc)

	done := make(chan bool, 1)
	go func() { done <- r.RemoveAsync(key) }()

	select {
	case ok := <-done:
		if !ok {
			t.Fatalf("RemoveAsync returned false for present key")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("RemoveAsync blocked on slow teardown; should return immediately")
	}

	// Session must be gone from the map already, even though Close() is
	// still blocked inside the detached teardown goroutine.
	r.mu.RLock()
	_, present := r.sessions[key]
	r.mu.RUnlock()
	if present {
		t.Fatalf("session still in r.sessions after RemoveAsync returned")
	}

	// Release the teardown and let the goroutine finish so Shutdown is clean.
	close(proc.release)
	r.removeWg.Wait()
	select {
	case <-proc.closeDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("teardown goroutine never completed Close after release")
	}
}

// TestRemoveAsync_EventuallyClosesProc proves the detached teardown does
// run proc.Close — the slow work is deferred, not dropped.
func TestRemoveAsync_EventuallyClosesProc(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 4, TTL: time.Hour})
	t.Cleanup(r.Shutdown)

	const key = "test:direct:k2:general"
	proc := newIdleProc()
	installSession(t, r, key, proc)

	if !r.RemoveAsync(key) {
		t.Fatalf("RemoveAsync returned false for present key")
	}
	r.removeWg.Wait() // wait for the detached teardown

	if proc.Alive() {
		t.Fatalf("proc still alive after async teardown; Close was not called")
	}
}

// TestRemoveAsync_AbsentKeyReturnsFalse locks the not-found contract that
// HandleDelete maps to HTTP 404.
func TestRemoveAsync_AbsentKeyReturnsFalse(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 4, TTL: time.Hour})
	t.Cleanup(r.Shutdown)

	if r.RemoveAsync("test:direct:missing:general") {
		t.Fatalf("RemoveAsync returned true for absent key")
	}
}

// TestRemoveAsync_DoubleRemoveSecondReturnsFalse proves two concurrent
// removes of the same key cannot both run the teardown: the lookup+delete
// is atomic under r.mu, so the loser sees the key already gone (review
// M1). HandleDelete turns the second's false into a 404, which the
// frontend already treats as success.
func TestRemoveAsync_DoubleRemoveSecondReturnsFalse(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 4, TTL: time.Hour})
	t.Cleanup(r.Shutdown)

	const key = "test:direct:k3:general"
	installSession(t, r, key, newIdleProc())

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		trueN   int
		results = make([]bool, 2)
	)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			ok := r.RemoveAsync(key)
			mu.Lock()
			results[i] = ok
			if ok {
				trueN++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	r.removeWg.Wait()

	if trueN != 1 {
		t.Fatalf("expected exactly one RemoveAsync to win; got %d (results=%v)", trueN, results)
	}
}

// TestRemove_StillSynchronous is the regression guard for the
// attachment-tracker integration test and any other caller that asserts
// post-teardown state right after Remove returns. Remove MUST block until
// proc.Close completes — only RemoveAsync defers it.
func TestRemove_StillSynchronous(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 4, TTL: time.Hour})
	t.Cleanup(r.Shutdown)

	const key = "test:direct:k4:general"
	proc := newIdleProc()
	installSession(t, r, key, proc)

	if !r.Remove(key) {
		t.Fatalf("Remove returned false for present key")
	}
	// By the time Remove returns, the synchronous teardown is done — no
	// wait, no removeWg. Close must already have run.
	if proc.Alive() {
		t.Fatalf("proc still alive immediately after synchronous Remove returned")
	}
	r.mu.RLock()
	_, present := r.sessions[key]
	r.mu.RUnlock()
	if present {
		t.Fatalf("session still in r.sessions after Remove")
	}
}

// TestRemoveAsync_FiresRetiredCallbackWithSessionID mirrors the existing
// Remove callback contract (router_session_retired_test.go) for the async
// path: the history-drawer subscriber still receives the snapshotted UUID,
// just slightly later (from the teardown goroutine).
func TestRemoveAsync_FiresRetiredCallbackWithSessionID(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 4, TTL: time.Hour})
	t.Cleanup(r.Shutdown)

	const key = "test:direct:k5:general"
	s := installSession(t, r, key, newIdleProc())
	wantSID := s.SessionID()

	gotCh := make(chan string, 1)
	r.SetOnSessionRetired(func(_ string, sessionID string) { gotCh <- sessionID })

	if !r.RemoveAsync(key) {
		t.Fatalf("RemoveAsync returned false")
	}

	select {
	case got := <-gotCh:
		if got != wantSID {
			t.Fatalf("retired callback sessionID = %q, want %q", got, wantSID)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("RemoveAsync did not fire OnSessionRetired within 3s")
	}
	r.removeWg.Wait()
}
