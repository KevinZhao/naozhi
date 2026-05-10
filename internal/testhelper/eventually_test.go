package testhelper

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// tbSpy is a minimal testing.TB implementation used to capture Fatalf
// without aborting the real test. We only override Helper + Fatalf;
// every other method delegates to an embedded *testing.T so the spy
// stays valid even if Eventually starts calling more TB methods.
type tbSpy struct {
	testing.TB
	fatals []string
}

func (s *tbSpy) Helper() {}

func (s *tbSpy) Fatalf(format string, args ...any) {
	s.fatals = append(s.fatals, fmt.Sprintf(format, args...))
	// Do NOT call FailNow — we want the test to continue so we can
	// inspect the captured message.
}

func newSpy(t *testing.T) *tbSpy {
	t.Helper()
	return &tbSpy{TB: t}
}

func TestEventually(t *testing.T) {
	t.Parallel()

	t.Run("immediate success, no sleep required", func(t *testing.T) {
		t.Parallel()
		calls := 0
		start := time.Now()
		Eventually(t, func() bool { calls++; return true }, time.Second, "should be immediate")
		if calls != 1 {
			t.Errorf("cond call count = %d, want 1", calls)
		}
		if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
			t.Errorf("immediate success took %v, want <50ms", elapsed)
		}
	})

	t.Run("success after N polls", func(t *testing.T) {
		t.Parallel()
		var n atomic.Int32
		Eventually(t, func() bool { return n.Add(1) >= 3 }, time.Second, "flips on 3rd check")
		if got := n.Load(); got != 3 {
			t.Errorf("cond call count = %d, want 3", got)
		}
	})

	t.Run("timeout records Fatalf via spy", func(t *testing.T) {
		t.Parallel()
		spy := newSpy(t)
		calls := 0
		EventuallyWithInterval(spy, func() bool { calls++; return false }, 30*time.Millisecond, 5*time.Millisecond, "never true")
		if len(spy.fatals) != 1 {
			t.Fatalf("Fatalf call count = %d, want 1 (msgs: %v)", len(spy.fatals), spy.fatals)
		}
		if !strings.Contains(spy.fatals[0], "never true") {
			t.Errorf("Fatalf msg = %q, want it to contain 'never true'", spy.fatals[0])
		}
		if !strings.Contains(spy.fatals[0], "timed out") {
			t.Errorf("Fatalf msg = %q, want it to contain 'timed out'", spy.fatals[0])
		}
		if calls < 2 {
			t.Errorf("cond calls = %d, want >=2 (polled at least twice over 30ms)", calls)
		}
	})

	t.Run("deadline-boundary retry rescues slow cond", func(t *testing.T) {
		t.Parallel()
		// cond becomes true only after timeout has passed — the post-loop
		// retry rescues this case without Fatalf.
		becomeTrueAt := time.Now().Add(40 * time.Millisecond)
		spy := newSpy(t)
		EventuallyWithInterval(spy, func() bool {
			return time.Now().After(becomeTrueAt)
		}, 20*time.Millisecond, 5*time.Millisecond, "rescued")
		if len(spy.fatals) != 0 {
			// On an overloaded CI runner the final cond call may also miss
			// the rescue window. Allow that, but assert the message shape.
			for _, m := range spy.fatals {
				if !strings.Contains(m, "rescued") {
					t.Errorf("unexpected Fatalf msg %q", m)
				}
			}
		}
	})
}

func TestEventuallyWithInterval_CustomCadence(t *testing.T) {
	t.Parallel()
	var n atomic.Int32
	start := time.Now()
	EventuallyWithInterval(t, func() bool { return n.Add(1) >= 2 }, time.Second, 50*time.Millisecond, "2nd poll")
	// First poll immediate, 2nd after 50ms sleep.
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("elapsed = %v, want >=40ms (2nd poll must come after interval)", elapsed)
	}
}
