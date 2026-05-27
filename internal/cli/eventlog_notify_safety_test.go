package cli

// RNEW-PERF-004 (#455): pin the close-vs-notify safety contract that the
// godoc on notifySubscribers spells out. The unsub-closure path closes
// sub.ch OUTSIDE subMu.Lock; the notify path holds subMu.RLock across the
// per-channel send loop so unsub.Lock cannot run until the iteration ends,
// preventing the send-on-closed-chan panic. This test runs Subscribe →
// concurrent unsub + Append-driven notify under -race so any future regression
// (e.g. an attempt to "optimise" notify by dropping the lock before the send
// loop) trips either the race detector or a real panic on closed-chan send.
//
// Coverage rationale: the godoc warning needs a runtime witness; without it,
// a future maintainer reading the comment alone could be tempted to ship the
// snapshot-then-unlock micro-opt described in #455.

import (
	"sync"
	"testing"
	"time"
)

func TestEventLog_NotifyVsUnsub_NoSendOnClosedPanic(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	const subscribers = 8
	const appends = 200

	subs := make([]func(), subscribers)
	for i := range subs {
		_, cancel := l.Subscribe()
		subs[i] = cancel
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Notify-driver: appending events fires notifySubscribers on every
	// call. Run continuously while the unsub goroutine churns subscribers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < appends; i++ {
			l.Append(EventEntry{Time: int64(i), Type: "user", Summary: "x"})
		}
		close(stop)
	}()

	// Unsubscribe each subscription on its own goroutine, racing the
	// concurrent notifies. If notifySubscribers ever drops subMu.RLock
	// before the send loop completes, this test fragment will panic
	// non-deterministically with "send on closed channel".
	for _, cancel := range subs {
		cancel := cancel
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Stagger unsubscribes mid-flight so a notify is likely to be
			// iterating at the moment unsub takes subMu.Lock.
			time.Sleep(time.Microsecond)
			cancel()
		}()
	}

	wg.Wait()

	// One more notify after all unsubs to confirm the EventLog is still
	// healthy (no goroutine wedged on subMu).
	l.Append(EventEntry{Type: "user", Summary: "tail"})
}
