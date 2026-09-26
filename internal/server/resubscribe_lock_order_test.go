package server

import "testing"

// TestRegistry_SwapAndExpireHandClosuresBack: the two resubscribe-path
// methods return the closure they displace instead of running it, so
// resubscribeEvents runs it after the registry's lock is released (lock order:
// registry → EventLog.subMu). The closure here checks the lock is free when
// it runs.
func TestRegistry_SwapAndExpireHandClosuresBack(t *testing.T) {
	r := newSubscriberRegistry()
	c := &wsClient{done: make(chan struct{})}
	c.authenticated.Store(true)
	r.add(c)

	calls := 0
	probe := func() {
		calls++
		if !r.mu.TryLock() {
			t.Error("a displaced closure ran with the registry's lock held")
			return
		}
		r.mu.Unlock()
	}
	r.reserve(c, "k")
	gen, _ := r.install(c, "k", probe, func() bool { return true })

	old, ok := r.swap(c, "k", gen, probe)
	if !ok {
		t.Fatal("swap at the current generation declined")
	}
	if calls != 0 {
		t.Fatal("swap ran the displaced closure itself")
	}
	old()

	stale, _ := r.expire(c, "k", 0)
	if calls != 1 {
		t.Fatal("expire ran the displaced closure itself")
	}
	stale()
	if calls != 2 {
		t.Fatalf("closures ran %d times, want 2", calls)
	}
}

// TestRegistry_SwapDeclinesAStaleGeneration: a parked loop whose generation
// was taken over by a newer subscribe must not install its unsub.
func TestRegistry_SwapDeclinesAStaleGeneration(t *testing.T) {
	r := newSubscriberRegistry()
	c := &wsClient{done: make(chan struct{})}
	r.add(c)
	admit := func() bool { return true }
	r.reserve(c, "k")
	oldGen, _ := r.install(c, "k", func() {}, admit)
	r.reserve(c, "k") // a newer subscribe
	r.install(c, "k", func() {}, admit)

	if _, ok := r.swap(c, "k", oldGen, func() { t.Error("stale unsub installed and run") }); ok {
		t.Error("swap accepted a stale generation")
	}
	if gen, ok := r.generation(c, "k"); !ok || gen != oldGen+1 {
		t.Errorf("generation = (%d,%v), want (%d,true)", gen, ok, oldGen+1)
	}
	r.remove(c)
	if _, ok := r.generation(c, "k"); ok {
		t.Error("generation still reported for a removed client")
	}
	if _, ok := r.swap(c, "k", oldGen+1, func() {}); ok {
		t.Error("swap installed onto a removed client")
	}
}
