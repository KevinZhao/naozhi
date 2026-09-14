package node

import (
	"fmt"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

// relay_seed_lockscope_test.go — makes Subscribe's lastEvent seed being INSIDE
// r.mu observable to the race detector. Epic I (#2547).
//
// TestWSRelay_Source_SubscribeSeedsLastEvent pinned three things by regexp over
// relay.go: that the seed exists, that it is gated on !alreadySubscribed, and
// that both sit inside the r.mu critical section. Probing each in turn showed the
// first two are already covered by behaviour —
// TestWSRelay_Subscribe_SeedsLastEventForRaceSafeReconnect fails when the seed is
// deleted, TestWSRelay_Subscribe_SeedOnlyOnFirstSubscriber fails when the guard
// is dropped — while the third was caught by nothing but the text scan: hoisting
// the seed past r.mu.Unlock() left the whole package green under -race, because
// no test drove a concurrent writer against it.
//
// This does. Unsubscribe deletes from the same map under r.mu, so concurrent
// subscribe/unsubscribe traffic puts a lock-scope violation in front of TSan,
// which reports on first observation rather than statistically.
func TestWSRelay_SubscribeSeed_StaysUnderTheLock(t *testing.T) {
	srv := wsTestServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		if !authHandshake(t, conn) {
			return
		}
		// Drain whatever the relay sends; the test only cares about relay-internal
		// map access, never the wire.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer srv.Close()

	r := newWSRelay(newRelayNode(srv))
	defer r.Close()

	// One Subscribe up front so ensureConnected has dialled before the workers
	// start — otherwise every goroutine would race on the connect path instead of
	// the map, and a failed dial would make this test pass while touching nothing.
	warmSink := &seedTestSink{}
	r.Subscribe(warmSink, "feishu:direct:warm:general", 1)

	const workers = 8
	const iters = 40
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			sink := &seedTestSink{}
			for i := range iters {
				key := fmt.Sprintf("feishu:direct:u%d-%d:general", w, i)
				// Subscribe writes r.lastEvent[key]; Unsubscribe deletes it. Both
				// must hold r.mu across the whole read-modify-write.
				r.Subscribe(sink, key, int64(i+1))
				r.Unsubscribe(sink, key)
			}
		}(w)
	}
	wg.Wait()

	// PREMISE: if ensureConnected had failed, Subscribe would return before its
	// r.mu section and this test would prove nothing. The warm key is still
	// subscribed, so seeing it means the lock section really ran.
	r.mu.Lock()
	_, warmPresent := r.lastEvent["feishu:direct:warm:general"]
	leftover := len(r.lastEvent)
	r.mu.Unlock()
	if !warmPresent {
		t.Fatal("the warm-up key was never seeded — Subscribe returned before its r.mu section, so this test exercised nothing")
	}
	// Every worker key was unsubscribed, so only the warm key may remain.
	if leftover != 1 {
		t.Errorf("lastEvent holds %d keys, want just the warm-up one; the seed and its cleanup are not paired", leftover)
	}
}
