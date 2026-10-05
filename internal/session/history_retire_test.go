package session

import (
	"context"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

// retireTestHistory is a HistoryIO that persists, so beginRetire raises a
// barrier; the persister is never started because the barrier does no I/O.
func retireTestHistory() *HistoryIO {
	return &HistoryIO{persister: &persist.Persister{}}
}

// awaitAsync runs awaitRetire in a goroutine and returns its result channel.
func awaitAsync(h *HistoryIO, key string, limit time.Duration) <-chan bool {
	got := make(chan bool, 1)
	go func() { got <- h.awaitRetire(key, limit) }()
	return got
}

func TestAwaitRetire_NoBarrierReturnsAtOnce(t *testing.T) {
	h := retireTestHistory()
	if !h.awaitRetire("k", time.Hour) {
		t.Fatal("awaitRetire with no barrier reported a timeout")
	}
}

func TestBeginRetire_NothingPersistedRaisesNoBarrier(t *testing.T) {
	h := &HistoryIO{}
	if h.beginRetire("k") {
		t.Fatal("beginRetire raised a barrier with no persister and no tracker")
	}
	if !h.awaitRetire("k", time.Hour) {
		t.Fatal("awaitRetire waited on a barrier that was never raised")
	}
}

// TestRetireBarrier_OverlappingRemovalsReleaseAfterBoth: two removals of one
// key hold the barrier until the second of them ends.
func TestRetireBarrier_OverlappingRemovalsReleaseAfterBoth(t *testing.T) {
	h := retireTestHistory()
	for range 2 {
		if !h.beginRetire("k") {
			t.Fatal("beginRetire did not raise the barrier")
		}
	}
	h.beginRetire("other")
	got := awaitAsync(h, "k", time.Hour)
	h.endRetire("k")
	select {
	case <-got:
		t.Fatal("awaitRetire returned with one removal of the key still dropping")
	case <-time.After(50 * time.Millisecond):
	}
	h.endRetire("k")
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("awaitRetire reported a timeout after the barrier fell")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("awaitRetire still waiting after both removals ended")
	}
	if !h.awaitRetire("k", time.Hour) {
		t.Fatal("a later removal-free wait blocked")
	}
}

func TestAwaitRetire_TimesOut(t *testing.T) {
	h := retireTestHistory()
	h.beginRetire("k")
	if h.awaitRetire("k", 20*time.Millisecond) {
		t.Fatal("awaitRetire on a held barrier reported it released")
	}
}

// TestAwaitRetire_ShutdownReleasesWaiter: a cancelled history ctx releases the
// wait without a timeout report.
func TestAwaitRetire_ShutdownReleasesWaiter(t *testing.T) {
	h := retireTestHistory()
	h.ctx, h.cancel = context.WithCancel(context.Background())
	h.beginRetire("k")
	got := awaitAsync(h, "k", time.Hour)
	h.cancelTasks()
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("awaitRetire reported a timeout on shutdown")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("awaitRetire still waiting after shutdown")
	}
}
