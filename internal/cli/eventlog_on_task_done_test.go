package cli

// R246-ARCH-20 / #802 (P0 subset): OnAgentTaskDone returns a cancel
// func to match the Subscribe() idiom. These tests pin the four
// observable behaviours of the new entry point + the ordering
// guarantees with a racing SetOnAgentTaskDone follow-up call.

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestOnAgentTaskDone_FiresOnTaskDone(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)

	var fired int32
	cancel := l.OnAgentTaskDone(func(taskID, status string) {
		if taskID != "t1" || status != "completed" {
			t.Errorf("got (%q,%q) want (t1, completed)", taskID, status)
		}
		atomic.AddInt32(&fired, 1)
	})
	defer cancel()

	l.Append(EventEntry{Type: "task_done", TaskID: "t1"})

	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Fatalf("callback fired %d times, want 1", got)
	}
}

func TestOnAgentTaskDone_CancelDetaches(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)

	var fired int32
	cancel := l.OnAgentTaskDone(func(string, string) {
		atomic.AddInt32(&fired, 1)
	})

	cancel()

	l.Append(EventEntry{Type: "task_done", TaskID: "t1"})

	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Fatalf("callback fired after Cancel: %d", got)
	}
}

func TestOnAgentTaskDone_CancelIdempotent(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)

	cancel := l.OnAgentTaskDone(func(string, string) {})
	cancel()
	cancel() // must not panic
}

// Pin last-writer-wins: registering a second callback supersedes the
// first. The first cancel must NOT clear the second installation
// (CompareAndSwap guard). This is what makes OnAgentTaskDone safe to
// call from goroutines that may race with a later SetOnAgentTaskDone.
func TestOnAgentTaskDone_StaleCancelDoesNotClearLaterRegistration(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)

	var firedA, firedB int32
	cancelA := l.OnAgentTaskDone(func(string, string) { atomic.AddInt32(&firedA, 1) })
	// Second registration replaces the first; cancelA is now stale.
	l.SetOnAgentTaskDone(func(string, string) { atomic.AddInt32(&firedB, 1) })

	// Calling the stale cancel must NOT clear the live B callback.
	cancelA()

	l.Append(EventEntry{Type: "task_done", TaskID: "t1"})

	if got := atomic.LoadInt32(&firedA); got != 0 {
		t.Errorf("stale callback A fired: %d", got)
	}
	if got := atomic.LoadInt32(&firedB); got != 1 {
		t.Errorf("live callback B did not fire: %d", got)
	}
}

func TestOnAgentTaskDone_NilFnReturnsNoopCancel(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)

	// Pre-install a real callback.
	var fired int32
	l.SetOnAgentTaskDone(func(string, string) { atomic.AddInt32(&fired, 1) })

	// nil registration must not clobber the existing callback...
	cancel := l.OnAgentTaskDone(nil)
	// ...and the returned cancel must be a no-op.
	cancel()

	l.Append(EventEntry{Type: "task_done", TaskID: "t1"})

	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Fatalf("pre-existing callback was clobbered by nil registration: fired=%d", got)
	}
}

// R246-ARCH-20 follow-up: the OnAgentTaskDone cancel func is racing-safe
// with concurrent Append paths -- once cancel returns, no new fires can
// be observed beyond the in-flight one. This pins that contract for the
// dashboard-tab teardown path that fires Cancel concurrently with the
// CLI's stream-event ingest.
func TestOnAgentTaskDone_CancelRacingAppend(t *testing.T) {
	t.Parallel()
	l := NewEventLog(64)

	var fired int32
	cancel := l.OnAgentTaskDone(func(string, string) {
		atomic.AddInt32(&fired, 1)
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 32; i++ {
			l.Append(EventEntry{Type: "task_done", TaskID: "t"})
		}
	}()
	cancel()
	wg.Wait()

	// Allow ANY count between 0 and 32 (Append racing Cancel can fire
	// the callback before the CompareAndSwap clears the pointer).
	// What we MUST NOT see is fires after wg.Wait() -- a follow-up
	// Append below confirms cancel really took effect.
	atomic.StoreInt32(&fired, 0)
	l.Append(EventEntry{Type: "task_done", TaskID: "after"})
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Fatalf("callback fired after Cancel returned + wait completed: %d", got)
	}
}
