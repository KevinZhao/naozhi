package server

import (
	"sync"
	"testing"
)

// authSetConsistent asserts the authenticated slice and its index agree: same
// size, every index points at the right slot, no nil or duplicate slots.
// Caller holds authMu or is single-threaded.
func authSetConsistent(t *testing.T, r *subscriberRegistry) {
	t.Helper()
	if len(r.authIdx) != len(r.authSlice) {
		t.Fatalf("idx len %d != slice len %d", len(r.authIdx), len(r.authSlice))
	}
	for i, c := range r.authSlice {
		if c == nil {
			t.Fatalf("slice slot %d is nil", i)
		}
		if got, ok := r.authIdx[c]; !ok || got != i {
			t.Fatalf("idx[c] = (%d,%v), want (%d,true)", got, ok, i)
		}
	}
	// Slots past the length must not pin removed clients for the GC.
	for i, c := range r.authSlice[len(r.authSlice):cap(r.authSlice)] {
		if c != nil {
			t.Fatalf("slot %d past the length still holds a removed client", len(r.authSlice)+i)
		}
	}
}

// TestAuthSet_AddRemove: the slice and its index stay in step across add and
// swap-delete, including the middle-element delete that moves the tail into
// the freed slot.
func TestAuthSet_AddRemove(t *testing.T) {
	r := newSubscriberRegistry()
	mk := func() *wsClient {
		c := &wsClient{send: make(chan []byte, 4), done: make(chan struct{})}
		c.authenticated.Store(true)
		return c
	}
	c1, c2, c3 := mk(), mk(), mk()
	r.add(c1)
	r.add(c2)
	r.add(c3)
	r.markAuthenticated(c2) // idempotent
	authSetConsistent(t, r)
	if len(r.authSlice) != 3 {
		t.Fatalf("after 3 adds + 1 re-mark, slice len = %d, want 3", len(r.authSlice))
	}

	r.remove(c2) // the middle one
	authSetConsistent(t, r)
	if _, ok := r.authIdx[c2]; ok {
		t.Fatal("c2 still authenticated after remove")
	}
	r.remove(c2) // not present: a no-op
	authSetConsistent(t, r)

	r.remove(c1)
	r.remove(c3)
	authSetConsistent(t, r)
	if len(r.authSlice) != 0 {
		t.Fatalf("after removing all, slice len = %d, want 0", len(r.authSlice))
	}
}

// TestAuthSet_MembershipFollowsRegistration: only a registered client can be
// authenticated, and an unauthenticated one joins on markAuthenticated.
func TestAuthSet_MembershipFollowsRegistration(t *testing.T) {
	r := newSubscriberRegistry()
	c := &wsClient{done: make(chan struct{})}
	r.markAuthenticated(c) // not registered: stays out
	if len(r.authSlice) != 0 {
		t.Fatal("markAuthenticated admitted an unregistered client")
	}
	r.add(c) // not yet authenticated
	if len(r.authSlice) != 0 {
		t.Fatal("add admitted an unauthenticated client to the authenticated set")
	}
	c.authenticated.Store(true)
	r.markAuthenticated(c)
	if got := r.authenticated(nil); len(got) != 1 || got[0] != c {
		t.Fatalf("authenticated = %v, want [c]", got)
	}
	r.remove(c)
	r.markAuthenticated(c) // a delayed auth after teardown
	if len(r.authSlice) != 0 {
		t.Fatal("a delayed markAuthenticated reinserted a removed client")
	}
}

// TestSnapshotAuthenticated_ReturnsTheAuthenticatedSet: the snapshot is
// exactly the authenticated clients, and a copy rather than an alias.
func TestSnapshotAuthenticated_ReturnsTheAuthenticatedSet(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	c1 := &wsClient{hub: hub, send: make(chan []byte, 4), done: make(chan struct{})}
	c2 := &wsClient{hub: hub, send: make(chan []byte, 4), done: make(chan struct{})}
	registerSub(hub, c1, "")
	registerSub(hub, c2, "")
	pending := &wsClient{hub: hub, send: make(chan []byte, 4), done: make(chan struct{})}
	hub.register(pending) // handshake still pending

	snapPtr, snap := hub.snapshotAuthenticated()
	seen := map[*wsClient]bool{}
	for _, c := range snap {
		seen[c] = true
	}
	if len(snap) != 2 || !seen[c1] || !seen[c2] {
		t.Fatalf("snapshot = %d clients (c1=%v c2=%v), want exactly c1 and c2", len(snap), seen[c1], seen[c2])
	}
	hub.unregister(c1)
	if snap[0] == nil || snap[1] == nil {
		t.Fatal("removing a client mutated an existing snapshot")
	}
	releaseBroadcastSnap(snapPtr, snap)
}

// TestAuthSet_ConcurrentChurn: register / unregister churn beside a
// broadcaster reading the set; -race surfaces any unguarded access.
func TestAuthSet_ConcurrentChurn(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	const writers = 4
	const iters = 300
	var writerWG, bcastWG sync.WaitGroup
	stop := make(chan struct{})

	// The broadcaster has its own WaitGroup: it only exits after the writers'
	// drain signals stop.
	bcastWG.Add(1)
	go func() {
		defer bcastWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				hub.BroadcastSessionReady("k")
			}
		}
	}()

	for i := 0; i < writers; i++ {
		writerWG.Add(1)
		go func() {
			defer writerWG.Done()
			for j := 0; j < iters; j++ {
				c := &wsClient{hub: hub, send: make(chan []byte, 64), done: make(chan struct{})}
				c.authenticated.Store(true)
				hub.subs.add(c)
				hub.subs.remove(c)
			}
		}()
	}
	writerWG.Wait()
	close(stop)
	bcastWG.Wait()

	hub.subs.authMu.Lock()
	authSetConsistent(t, hub.subs)
	hub.subs.authMu.Unlock()
	if n := authCount(hub); n != 0 {
		t.Errorf("%d clients left authenticated after balanced churn", n)
	}
}
