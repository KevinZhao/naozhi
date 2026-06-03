package cli

// RNEW-PERF-004 (#455) / R20260603000023-PERF-4 (#1647): pin the
// close-vs-notify safety contract. notifySubscribers now snapshots the
// subscribers slice header under a short subMu.RLock and RELEASES subMu
// before the per-subscriber send loop (#1647 — so same-session multi-tab
// dispatch no longer serialises on subMu). The send-on-closed-chan race
// that subMu.RLock used to prevent is now guarded by the per-subscriber
// (*subscriber).mu: signal() observes `closed` under RLock, close() flips it
// under Lock, and unsub removes via copy-on-write so a lock-free snapshot is
// never mutated underneath the reader. This test runs Subscribe → concurrent
// unsub + Append-driven notify under -race so any regression trips either the
// race detector or a real panic on closed-chan send.

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
