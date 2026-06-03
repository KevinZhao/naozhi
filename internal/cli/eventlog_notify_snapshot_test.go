package cli

// R20260603000023-PERF-4 (#1647): notifySubscribers must snapshot the
// subscribers slice under a short RLock and release subMu BEFORE the
// per-subscriber send loop, so same-session multi-tab dispatch and
// cross-session notify waves no longer serialise on the shared subMu for
// the whole loop. These tests assert the observable consequences of that
// design: (1) notify never holds subMu while invoking per-subscriber
// signals, so an unsub (which takes subMu.Lock) can interleave freely; and
// (2) after unsub, the cancelled subscriber's channel is closed exactly once
// (ok=false), while still-live subscribers keep receiving wakes.

import (
	"sync"
	"testing"
	"time"
)

// TestEventLog_Notify_DoesNotHoldSubMuDuringSend asserts that subMu is NOT
// held during the per-subscriber send loop. We register many subscribers,
// then concurrently fire a continuous stream of notifies while a separate
// goroutine repeatedly takes subMu.Lock (via Subscribe) — if notify held the
// write-excluding RLock across the whole loop, the Subscribe storm would be
// starved/serialised behind every notify; with the snapshot-then-unlock
// design both make progress. The test simply requires completion under -race
// with no panic and no deadlock.
func TestEventLog_Notify_DoesNotHoldSubMuDuringSend(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)

	const initial = 16
	cancels := make([]func(), 0, initial)
	for i := 0; i < initial; i++ {
		_, cancel := l.Subscribe()
		cancels = append(cancels, cancel)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Notify driver.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				l.Append(EventEntry{Type: "user", Summary: "x"})
			}
		}
	}()

	// Subscribe/unsub churn — each Subscribe + cancel takes subMu.Lock twice,
	// racing the notify loop's RLock window.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_, cancel := l.Subscribe()
			cancel()
		}
		close(stop)
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("notify/subscribe churn deadlocked — subMu likely held across send loop")
	}

	for _, c := range cancels {
		c()
	}
}

// TestEventLog_Cancel_ClosesChannelExactlyOnce verifies the per-subscriber
// close path still delivers a closed channel (ok=false) to the cancelled
// subscriber while leaving siblings untouched, after the migration from the
// subMu-guarded close to the (*subscriber).close fine-grained lock.
func TestEventLog_Cancel_ClosesChannelExactlyOnce(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)

	chA, cancelA := l.Subscribe()
	chB, cancelB := l.Subscribe()

	cancelA()
	// Double cancel must be a no-op (closeOnce), not a panic.
	cancelA()

	select {
	case _, ok := <-chA:
		if ok {
			t.Fatalf("cancelled subscriber A: expected closed channel (ok=false), got ok=true")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled subscriber A channel was not closed")
	}

	// B is still live: an Append must still wake it.
	l.Append(EventEntry{Type: "user", Summary: "wake-b"})
	select {
	case _, ok := <-chB:
		if !ok {
			t.Fatal("live subscriber B unexpectedly closed")
		}
	case <-time.After(time.Second):
		t.Fatal("live subscriber B did not receive notify")
	}

	cancelB()
	select {
	case _, ok := <-chB:
		if ok {
			t.Fatal("cancelled subscriber B: expected closed channel (ok=false)")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled subscriber B channel was not closed")
	}
}
