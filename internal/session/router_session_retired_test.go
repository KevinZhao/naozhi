package session

import (
	"sync"
	"testing"
	"time"
)

// TestRouter_OnSessionRetired_RemoveCarriesSessionID locks the contract
// that Router.Remove fires the SetOnSessionRetired callback with the
// session UUID captured before unregister cleared r.ss.sessions[key].
// The history-drawer wiring depends on this — without it the dashboard
// would have no UUID to stamp retired_at against.
func TestRouter_OnSessionRetired_RemoveCarriesSessionID(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 4, TTL: time.Hour})
	t.Cleanup(r.Shutdown)

	const (
		key = "test:direct:k1:general"
		sid = "11111111-2222-3333-4444-555555555555"
	)
	s := &ManagedSession{key: key}
	s.setSessionID(sid)
	r.mu.Lock()
	r.ss.sessions[key] = s
	r.mu.Unlock()

	var (
		mu       sync.Mutex
		gotKey   string
		gotSID   string
		gotCount int
	)
	r.SetOnSessionRetired(func(k, sessionID string) {
		mu.Lock()
		gotKey, gotSID = k, sessionID
		gotCount++
		mu.Unlock()
	})

	if !r.Remove(key) {
		t.Fatalf("Remove returned false")
	}

	mu.Lock()
	defer mu.Unlock()
	if gotCount != 1 {
		t.Fatalf("retired callback fired %d times, want 1", gotCount)
	}
	if gotKey != key {
		t.Fatalf("callback key = %q, want %q", gotKey, key)
	}
	if gotSID != sid {
		t.Fatalf("callback sessionID = %q, want %q", gotSID, sid)
	}
}

// TestRouter_OnSessionRetired_ResetCarriesSessionID mirrors the Remove
// case for Router.Reset, the /new code path. resetLocked drops
// r.ss.sessions[key] before notifyKeyRetired runs, so the callback must
// receive the snapshotted UUID rather than reading from the (now
// missing) session entry.
func TestRouter_OnSessionRetired_ResetCarriesSessionID(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 4, TTL: time.Hour})
	t.Cleanup(r.Shutdown)

	const (
		key = "test:direct:k1:general"
		sid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	)
	s := &ManagedSession{key: key}
	s.setSessionID(sid)
	r.mu.Lock()
	r.ss.sessions[key] = s
	r.mu.Unlock()

	gotCh := make(chan string, 1)
	r.SetOnSessionRetired(func(_ string, sessionID string) {
		gotCh <- sessionID
	})

	r.Reset(key)

	select {
	case got := <-gotCh:
		if got != sid {
			t.Fatalf("callback sessionID = %q, want %q", got, sid)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Reset did not fire OnSessionRetired callback within 2s")
	}
}

// TestRouter_OnSessionRetired_NilFnSafe locks the contract that
// SetOnSessionRetired(nil) clears the callback without panicking the
// next teardown — used by tests/teardown that swap callbacks midway.
func TestRouter_OnSessionRetired_NilFnSafe(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 4, TTL: time.Hour})
	t.Cleanup(r.Shutdown)

	called := false
	r.SetOnSessionRetired(func(_, _ string) { called = true })
	r.SetOnSessionRetired(nil) // clear

	const key = "k"
	s := &ManagedSession{key: key}
	s.setSessionID("sid-x")
	r.mu.Lock()
	r.ss.sessions[key] = s
	r.mu.Unlock()
	r.Remove(key)

	if called {
		t.Fatalf("nil-cleared callback was still invoked")
	}
}

// TestRouter_OnKeyRetired_StillFiresAlongsideSessionRetired locks the
// contract that the existing SetOnKeyRetired wiring (used by
// dispatch.MessageQueue.Cleanup) is not disturbed when SetOnSessionRetired
// is also registered: both callbacks must fire on the same teardown.
func TestRouter_OnKeyRetired_StillFiresAlongsideSessionRetired(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 4, TTL: time.Hour})
	t.Cleanup(r.Shutdown)

	const key = "k"
	s := &ManagedSession{key: key}
	s.setSessionID("sid-x")
	r.mu.Lock()
	r.ss.sessions[key] = s
	r.mu.Unlock()

	var (
		mu       sync.Mutex
		keyHits  int
		sessHits int
	)
	r.SetOnKeyRetired(func(string) {
		mu.Lock()
		keyHits++
		mu.Unlock()
	})
	r.SetOnSessionRetired(func(string, string) {
		mu.Lock()
		sessHits++
		mu.Unlock()
	})

	r.Remove(key)

	mu.Lock()
	defer mu.Unlock()
	if keyHits != 1 || sessHits != 1 {
		t.Fatalf("expected both callbacks to fire once each; got keyHits=%d sessHits=%d", keyHits, sessHits)
	}
}
