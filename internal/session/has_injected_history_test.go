package session

import (
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestHasInjectedHistory_Empty verifies the happy-path-unknown signal: a
// brand-new ManagedSession with no history returns false, so the
// R53-ARCH-001 deferred JSONL backfill path will fire.
func TestHasInjectedHistory_Empty(t *testing.T) {
	s := &ManagedSession{}
	if s.hasInjectedHistory() {
		t.Error("hasInjectedHistory() = true on empty session, want false")
	}
}

// TestHasInjectedHistory_AfterInject verifies that InjectHistory flips the
// flag. This is the signal the R53-ARCH-001 deferred backfill path checks
// after shimReconnectGraceDelay — if it's true, ReconnectShims (or any
// other path) already populated persistedHistory, and the deferred load
// must skip to avoid duplicate entries.
func TestHasInjectedHistory_AfterInject(t *testing.T) {
	s := &ManagedSession{}
	entries := []cli.EventEntry{
		{Type: "user", Summary: "hello", Time: time.Now().UnixMilli()},
	}
	s.InjectHistory(entries)
	if !s.hasInjectedHistory() {
		t.Error("hasInjectedHistory() = false after InjectHistory, want true")
	}
}

// TestHasInjectedHistory_ConcurrentReadWrite stresses the historyMu RWMutex
// interaction: while one goroutine InjectHistory's (write lock), many
// concurrent hasInjectedHistory readers (read lock) must observe the
// transition atomically and never deadlock / race. Run with -race.
func TestHasInjectedHistory_ConcurrentReadWrite(t *testing.T) {
	s := &ManagedSession{}
	done := make(chan struct{})

	// Writer: injects a batch after a tiny delay.
	go func() {
		defer close(done)
		time.Sleep(10 * time.Millisecond)
		s.InjectHistory([]cli.EventEntry{
			{Type: "user", Summary: "x", Time: time.Now().UnixMilli()},
		})
	}()

	// Many concurrent readers: drain until the writer signals done.
	readersDone := make(chan struct{})
	go func() {
		defer close(readersDone)
		for {
			select {
			case <-done:
				return
			default:
			}
			// Just exercise the read lock.
			_ = s.hasInjectedHistory()
		}
	}()

	select {
	case <-readersDone:
	case <-time.After(2 * time.Second):
		t.Fatal("readers goroutine did not finish; possible deadlock")
	}

	// Final state: writer completed, flag must be true.
	if !s.hasInjectedHistory() {
		t.Error("hasInjectedHistory() = false after writer finished, want true")
	}
}
