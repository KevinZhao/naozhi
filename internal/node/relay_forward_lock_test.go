package node

import (
	"sync"
	"testing"
	"time"
)

// blockingRawSink is an EventSink whose SendRaw blocks until released, used to
// prove wsRelay.forwardEvent fans out OUTSIDE r.mu (R202606b-GO-002, #2187).
type blockingRawSink struct {
	entered chan struct{} // closed when SendRaw is first entered
	release chan struct{} // SendRaw returns once this is closed
	once    sync.Once
}

func (b *blockingRawSink) SendJSON(any) {}

func (b *blockingRawSink) SendRaw([]byte) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
}

// TestForwardEvent_FanOutReleasesLock verifies that while a subscriber's
// SendRaw is blocked, lock-taking operations (Subscribe / Unsubscribe /
// RemoveClient / Close) still complete promptly — i.e. the fan-out no longer
// holds r.mu. Before the fix, SendRaw ran under r.mu and any of these would
// deadlock for the duration of the blocked send.
func TestForwardEvent_FanOutReleasesLock(t *testing.T) {
	r := newWSRelay(&HTTPClient{ID: "n1"})

	blk := &blockingRawSink{entered: make(chan struct{}), release: make(chan struct{})}
	const key = "feishu:p2p:u1"

	// Seed a subscriber directly (bypass the WS connect path).
	r.mu.Lock()
	r.subs[key] = []EventSink{blk}
	r.mu.Unlock()

	// Drive the fan-out in a goroutine; it will block inside SendRaw.
	go r.forwardEvent([]byte(`{"type":"event","key":"feishu:p2p:u1","event":{"time":1}}`))

	select {
	case <-blk.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("SendRaw was never entered")
	}

	// While SendRaw is blocked, taking r.mu must NOT block — the same lock
	// every Subscribe / Unsubscribe / RemoveClient / Close acquires.
	done := make(chan struct{})
	go func() {
		r.mu.Lock()
		_ = r.subs[key]
		r.mu.Unlock()
		close(done)
	}()

	select {
	case <-done:
		// Good: lock was free while SendRaw blocked.
	case <-time.After(2 * time.Second):
		close(blk.release)
		t.Fatal("lock-taking op blocked while SendRaw was in-flight — fan-out still holds r.mu")
	}

	close(blk.release) // let the blocked SendRaw finish
}
