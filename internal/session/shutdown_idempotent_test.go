package session

import (
	"sync"
	"testing"
)

// TestShutdown_ConcurrentCallersRunTheBodyOnce pins the Once gate:
// R49-REL-SHUTDOWN-ONCE was a race between broadcast-timer teardown and a
// second caller re-entering. Concurrent callers must run the body exactly once
// (which also covers serial idempotency).
func TestShutdown_ConcurrentCallersRunTheBodyOnce(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{MaxProcs: 1})

	var mu sync.Mutex
	calls := 0
	r.hist.cancel = func() {
		mu.Lock()
		calls++
		mu.Unlock()
	}

	const callers = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r.Shutdown()
		}()
	}
	close(start)
	wg.Wait()

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("%d concurrent Shutdown calls ran the body %d times, want 1", callers, got)
	}
}
