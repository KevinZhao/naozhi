package server

import (
	"sync"
	"testing"
)

// TestReserveOwnerSlot_HitsCap pins R229-SEC-8 / #1022: a single owner
// can reserve up to maxConnsPerOwner (=20) slots, the (maxConnsPerOwner+1)th
// reservation must fail. This is the budget that prevents one stolen
// token from monopolising the global maxWSConns pool.
func TestReserveOwnerSlot_HitsCap(t *testing.T) {
	h := &Hub{
		connCountByOwner: make(map[string]int),
	}

	const owner = "owner-A"
	for i := 0; i < maxConnsPerOwner; i++ {
		if !h.reserveOwnerSlot(owner) {
			t.Fatalf("reserve %d/%d should have succeeded", i+1, maxConnsPerOwner)
		}
	}
	if h.reserveOwnerSlot(owner) {
		t.Fatalf("reserve %d should have failed (cap=%d)", maxConnsPerOwner+1, maxConnsPerOwner)
	}
}

// TestReserveOwnerSlot_ReleaseFreesSlot pins that releaseOwnerSlot
// returns budget so a reconnect after a tab close immediately succeeds.
// Without this, a flapping client would lock itself out of its own
// budget for the lifetime of the Hub.
func TestReserveOwnerSlot_ReleaseFreesSlot(t *testing.T) {
	h := &Hub{
		connCountByOwner: make(map[string]int),
	}
	const owner = "owner-A"

	for i := 0; i < maxConnsPerOwner; i++ {
		h.reserveOwnerSlot(owner)
	}
	if h.reserveOwnerSlot(owner) {
		t.Fatal("budget should be exhausted")
	}
	h.releaseOwnerSlot(owner)
	if !h.reserveOwnerSlot(owner) {
		t.Fatal("after release, one slot should be available")
	}
}

// TestReserveOwnerSlot_OwnersIndependent pins that two distinct owners
// each get their own maxConnsPerOwner allowance — exhausting owner A
// must NOT block owner B.
func TestReserveOwnerSlot_OwnersIndependent(t *testing.T) {
	h := &Hub{
		connCountByOwner: make(map[string]int),
	}

	for i := 0; i < maxConnsPerOwner; i++ {
		h.reserveOwnerSlot("A")
	}
	if h.reserveOwnerSlot("A") {
		t.Fatal("owner A should be capped")
	}
	if !h.reserveOwnerSlot("B") {
		t.Fatal("owner B should still have its full budget")
	}
}

// TestReserveOwnerSlot_EmptyOwnerSkipsCap pins the legacy single-user /
// anonymous-pre-cookie path: an empty owner key always succeeds and
// does not bump the per-owner counter.
func TestReserveOwnerSlot_EmptyOwnerSkipsCap(t *testing.T) {
	h := &Hub{
		connCountByOwner: make(map[string]int),
	}
	for i := 0; i < maxConnsPerOwner*5; i++ {
		if !h.reserveOwnerSlot("") {
			t.Fatalf("empty owner should never be capped (call %d)", i)
		}
	}
	if got := h.connCountByOwner[""]; got != 0 {
		t.Errorf("empty-owner reserve should not bump map; got count=%d", got)
	}
}

// TestReserveOwnerSlot_NilMapNoCrash covers hand-built Hubs that
// bypass NewHub.
func TestReserveOwnerSlot_NilMapNoCrash(t *testing.T) {
	h := &Hub{}
	if !h.reserveOwnerSlot("anyone") {
		t.Error("nil map should fall through to allow")
	}
	h.releaseOwnerSlot("anyone") // must not panic
}

// TestReserveOwnerSlot_ReleaseDeletesAtZero pins the entry-cleanup
// behaviour: when the last connection for an owner releases, the map
// entry should be removed so the map size stays bounded by the
// active-user set rather than the lifetime-user set.
func TestReserveOwnerSlot_ReleaseDeletesAtZero(t *testing.T) {
	h := &Hub{
		connCountByOwner: make(map[string]int),
	}
	h.reserveOwnerSlot("A")
	h.reserveOwnerSlot("A")
	h.releaseOwnerSlot("A")
	h.releaseOwnerSlot("A")
	if _, ok := h.connCountByOwner["A"]; ok {
		t.Errorf("owner-A entry should be deleted at zero count, map: %#v", h.connCountByOwner)
	}
}

// TestReserveOwnerSlot_Concurrent pins lock correctness. Race-detector
// (`go test -race`) catches missing locks; the post-condition pins that
// the cap is never exceeded under concurrent fire.
func TestReserveOwnerSlot_Concurrent(t *testing.T) {
	h := &Hub{
		connCountByOwner: make(map[string]int),
	}

	const owner = "shared"
	var wg sync.WaitGroup
	var admitted int64
	var mu sync.Mutex

	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if h.reserveOwnerSlot(owner) {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if admitted != int64(maxConnsPerOwner) {
		t.Errorf("expected exactly %d admits under concurrent fire, got %d", maxConnsPerOwner, admitted)
	}
	if h.connCountByOwner[owner] != maxConnsPerOwner {
		t.Errorf("counter drift: expected %d, got %d", maxConnsPerOwner, h.connCountByOwner[owner])
	}
}
