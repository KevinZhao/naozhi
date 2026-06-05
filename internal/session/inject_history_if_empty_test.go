package session

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
)

func mkEntries(prefix string, n int) []cli.EventEntry {
	out := make([]cli.EventEntry, n)
	base := time.Now().UnixMilli()
	for i := range out {
		out[i] = cli.EventEntry{Type: "user", Summary: prefix, Time: base + int64(i)}
	}
	return out
}

// TestInjectHistoryIfEmpty_FirstWins verifies the atomic claim: the first call
// injects and returns true; a second call on the now-populated session is a
// no-op and returns false. This is the contract the Tier1/Tier2 startup
// loaders rely on (#1812) instead of the old check-then-act
// hasInjectedHistory()+InjectHistory pair.
func TestInjectHistoryIfEmpty_FirstWins(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{}

	if !s.InjectHistoryIfEmpty(mkEntries("a", 3)) {
		t.Fatal("first InjectHistoryIfEmpty returned false, want true")
	}
	if got := len(s.EventEntries()); got != 3 {
		t.Fatalf("after first inject len=%d, want 3", got)
	}
	if s.InjectHistoryIfEmpty(mkEntries("b", 5)) {
		t.Fatal("second InjectHistoryIfEmpty returned true on populated session, want false")
	}
	if got := len(s.EventEntries()); got != 3 {
		t.Fatalf("second inject must be a no-op; len=%d, want 3", got)
	}
}

// TestInjectHistoryIfEmpty_ConcurrentSingleInject is the regression test for
// #1812: two startup loaders (e.g. Tier 1 event-log + Tier 2 JSONL) running
// concurrently for the SAME session must result in exactly one batch being
// appended — never both, which previously produced duplicated turns in the
// dashboard sidebar. Run with -race.
func TestInjectHistoryIfEmpty_ConcurrentSingleInject(t *testing.T) {
	t.Parallel()
	for iter := 0; iter < 200; iter++ {
		s := &ManagedSession{}
		var wins int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if s.InjectHistoryIfEmpty(mkEntries("x", 4)) {
					atomic.AddInt32(&wins, 1)
				}
			}()
		}
		close(start)
		wg.Wait()

		if w := atomic.LoadInt32(&wins); w != 1 {
			t.Fatalf("iter %d: %d goroutines injected, want exactly 1 (double-inject regression)", iter, w)
		}
		if got := len(s.EventEntries()); got != 4 {
			t.Fatalf("iter %d: persistedHistory len=%d, want 4 (no duplication)", iter, got)
		}
	}
}

// TestInjectHistory_StillAppends ensures the unconditional InjectHistory path
// is unchanged by the #1812 refactor — it always appends, even on a populated
// session (used by live Send/result forwarding, not the startup guard).
func TestInjectHistory_StillAppends(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{}
	s.InjectHistory(mkEntries("a", 2))
	s.InjectHistory(mkEntries("b", 3))
	if got := len(s.EventEntries()); got != 5 {
		t.Fatalf("InjectHistory should append unconditionally; len=%d, want 5", got)
	}
}
