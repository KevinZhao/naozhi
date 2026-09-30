package session

import (
	"sync"
	"testing"
	"time"
)

// TestRouter_OnSessionRetired_RemoveCarriesSessionID locks the contract
// that Router.Remove fires the observer's KeyRetired with the
// session UUID captured before unregister cleared r.ss.Load(key).
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
	r.ss.Update(func(tx sessTx) {
		tx.Put(key, s)
	})

	var (
		mu       sync.Mutex
		gotKey   string
		gotSID   string
		gotCount int
	)
	observe(r).retired = func(k, sessionID string) {
		mu.Lock()
		gotKey, gotSID = k, sessionID
		gotCount++
		mu.Unlock()
	}

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
// case for Router.Reset, the /new code path. resetEntry drops
// r.ss.Load(key) before notifyKeyRetired runs, so the callback must
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
	r.ss.Update(func(tx sessTx) {
		tx.Put(key, s)
	})

	gotCh := make(chan string, 1)
	observe(r).retired = func(_ string, sessionID string) {
		gotCh <- sessionID
	}

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

// TestRouter_NilObserverSafe locks the contract that a router built without
// an Observer retires sessions without panicking: tests and tools that never
// wire the dashboard construct routers this way.
func TestRouter_NilObserverSafe(t *testing.T) {
	r := NewRouter(RouterConfig{MaxProcs: 4, TTL: time.Hour})
	t.Cleanup(r.Shutdown)

	const key = "k"
	s := &ManagedSession{key: key}
	s.setSessionID("sid-x")
	r.ss.Update(func(tx sessTx) {
		tx.Put(key, s)
	})
	r.Remove(key)
	if got := r.ss.Load(key); got != nil {
		t.Fatal("Remove left the key registered")
	}
}
