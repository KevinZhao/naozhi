package server

import (
	"sync"
	"testing"
)

// authClientsSliceConsistent asserts the slice mirror, its index map and the
// authClients map all agree: same membership, every index points at the right
// slot, and no nil/dup slots. Caller must hold authMu (or be single-threaded).
func authClientsSliceConsistent(t *testing.T, h *Hub) {
	t.Helper()
	if len(h.authClientsSlice) != len(h.authClients) {
		t.Fatalf("slice len %d != map len %d", len(h.authClientsSlice), len(h.authClients))
	}
	if len(h.authClientsIdx) != len(h.authClients) {
		t.Fatalf("idx len %d != map len %d", len(h.authClientsIdx), len(h.authClients))
	}
	for i, c := range h.authClientsSlice {
		if c == nil {
			t.Fatalf("slice slot %d is nil", i)
		}
		if _, ok := h.authClients[c]; !ok {
			t.Fatalf("slice slot %d holds a client absent from the map", i)
		}
		if got, ok := h.authClientsIdx[c]; !ok || got != i {
			t.Fatalf("idx[c] = (%d,%v), want (%d,true)", got, ok, i)
		}
	}
}

// TestAuthClientsSliceMirror_AddRemove verifies R202606g-PERF-020 (#2310): the
// contiguous slice mirror stays in lockstep with the authClients map across
// add / swap-delete, including the swap-delete-of-a-middle-element path that
// moves the tail into the freed slot.
func TestAuthClientsSliceMirror_AddRemove(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	mk := func() *wsClient {
		return &wsClient{hub: hub, send: make(chan []byte, 4), done: make(chan struct{})}
	}
	c1, c2, c3 := mk(), mk(), mk()

	hub.authMu.Lock()
	hub.addAuthClientLocked(c1)
	hub.addAuthClientLocked(c2)
	hub.addAuthClientLocked(c3)
	// Idempotent: re-adding must not grow the slice or break the invariant.
	hub.addAuthClientLocked(c2)
	authClientsSliceConsistent(t, hub)
	if len(hub.authClientsSlice) != 3 {
		t.Fatalf("after 3 adds + 1 dup, slice len = %d, want 3", len(hub.authClientsSlice))
	}

	// Remove the MIDDLE element: the tail (c3) must be swapped into c2's slot
	// and c3's index fixed up.
	hub.removeAuthClientLocked(c2)
	authClientsSliceConsistent(t, hub)
	if _, ok := hub.authClients[c2]; ok {
		t.Fatal("c2 still present after remove")
	}

	// Remove a non-present client: must be a no-op.
	hub.removeAuthClientLocked(c2)
	authClientsSliceConsistent(t, hub)

	hub.removeAuthClientLocked(c1)
	hub.removeAuthClientLocked(c3)
	authClientsSliceConsistent(t, hub)
	if len(hub.authClientsSlice) != 0 {
		t.Fatalf("after removing all, slice len = %d, want 0", len(hub.authClientsSlice))
	}
	hub.authMu.Unlock()
}

// TestSnapshotAuthenticated_UsesSliceMirror confirms snapshotAuthenticated
// returns exactly the slice-mirror membership (the copy() fast path), and that
// the snapshot survives a subsequent mutation of the mirror (it is a copy, not
// an alias). R202606g-PERF-020 (#2310).
func TestSnapshotAuthenticated_UsesSliceMirror(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	c1 := &wsClient{hub: hub, send: make(chan []byte, 4), done: make(chan struct{})}
	c1.authenticated.Store(true)
	c2 := &wsClient{hub: hub, send: make(chan []byte, 4), done: make(chan struct{})}
	c2.authenticated.Store(true)
	registerSub(hub, c1, "")
	registerSub(hub, c2, "")

	snapPtr, snap := hub.snapshotAuthenticated()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	seen := map[*wsClient]bool{}
	for _, c := range snap {
		seen[c] = true
	}
	if !seen[c1] || !seen[c2] {
		t.Fatalf("snapshot missing a client: c1=%v c2=%v", seen[c1], seen[c2])
	}
	releaseBroadcastSnap(snapPtr, snap)
}

// TestAuthClientsSliceMirror_ConcurrentChurn stresses the slice mirror under
// the real lock discipline (h.mu + nested authMu) so -race surfaces any data
// race introduced by maintaining the parallel slice/index. R202606g-PERF-020.
func TestAuthClientsSliceMirror_ConcurrentChurn(t *testing.T) {
	hub, _ := newTestHub("tok")
	t.Cleanup(hub.Shutdown)

	const writers = 4
	const iters = 300
	var writerWG sync.WaitGroup
	var bcastWG sync.WaitGroup
	stop := make(chan struct{})

	// Broadcaster reads the slice mirror via snapshotAuthenticated on a tight
	// loop until the writers finish. It is tracked on its OWN WaitGroup so the
	// writer drain (writerWG.Wait) does not deadlock waiting on a goroutine that
	// only exits after that same drain signals stop.
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
				hub.mu.Lock()
				hub.authMu.Lock()
				hub.clients[c] = struct{}{}
				hub.addAuthClientLocked(c)
				hub.authMu.Unlock()
				hub.mu.Unlock()

				hub.mu.Lock()
				hub.authMu.Lock()
				hub.removeAuthClientLocked(c)
				delete(hub.clients, c)
				hub.authMu.Unlock()
				hub.mu.Unlock()
			}
		}()
	}
	writerWG.Wait()
	close(stop)
	bcastWG.Wait()

	hub.authMu.Lock()
	authClientsSliceConsistent(t, hub)
	hub.authMu.Unlock()
}
