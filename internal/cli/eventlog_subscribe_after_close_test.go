package cli

// R247-PERF-14 (#553): post-close Subscribe short-circuit. Two pin tests:
//   1. SubscribeAfterClose returns the shared singleton closed channel
//      (pointer parity), so two callers do not each allocate fresh
//      channel + subscriber pairs only to close them.
//   2. The returned channel still satisfies the close contract: a select
//      arm fires immediately with ok=false.

import (
	"reflect"
	"testing"
	"time"
)

func TestEventLog_SubscribeAfterClose_SharesSingletonChannel(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	l.CloseSubscribers()

	ch1, cancel1 := l.Subscribe()
	defer cancel1()
	ch2, cancel2 := l.Subscribe()
	defer cancel2()

	// Both calls should return the same underlying channel value — the
	// process-wide pre-closed singleton. Use reflect.ValueOf(ch).Pointer()
	// to compare channel headers (identical to comparing the runtime hchan
	// pointers; channel == channel is also defined for typed channels but
	// the typed `<-chan struct{}` does not allow direct comparison through
	// `==` when both are receive-only).
	p1 := reflect.ValueOf(ch1).Pointer()
	p2 := reflect.ValueOf(ch2).Pointer()
	if p1 != p2 {
		t.Errorf("post-close Subscribe returned distinct channels (%x != %x); singleton sharing broken", p1, p2)
	}
}

func TestEventLog_SubscribeAfterClose_ChannelIsClosed(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	l.CloseSubscribers()

	ch, cancel := l.Subscribe()
	defer cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("post-close Subscribe channel delivered a value; want closed")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("post-close Subscribe channel still open after 500ms")
	}

	// Cancel must remain a no-op (closeOnce contract preserved by the
	// no-op cancel func returned alongside the singleton).
	cancel()
}

func TestEventLog_SubscribeAfterClose_NoSubscriberAlloc(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	l.CloseSubscribers()

	// Take a baseline subscribers count; post-close Subscribe must not
	// register a fresh *subscriber into l.subscribers (subsClosed gate
	// returns the singleton before the append).
	l.subMu.Lock()
	pre := len(l.subscribers)
	l.subMu.Unlock()

	for i := 0; i < 4; i++ {
		_, cancel := l.Subscribe()
		cancel()
	}

	l.subMu.Lock()
	post := len(l.subscribers)
	l.subMu.Unlock()

	if pre != post {
		t.Errorf("post-close Subscribe leaked subscribers into slice: pre=%d post=%d (gate broken)", pre, post)
	}
}
