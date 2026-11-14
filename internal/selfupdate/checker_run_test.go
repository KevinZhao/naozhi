package selfupdate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// runHarness drives Checker.Run against a fake wall clock. Run's poll timer
// still runs on real (monotonic) time, shortened to a few milliseconds, which
// is exactly the split a sleeping Mac produces: the wall clock jumps while the
// monotonic clock stood still.
type runHarness struct {
	t *testing.T

	mu       sync.Mutex
	now      time.Time
	nowCalls int
	checks   int
	// mark is nowCalls when the last lookup ran; Run reads the clock twice
	// more (schedule the next check, compute the next wait) once it is done
	// with that check, which is how waitChecks knows the schedule is set.
	mark int
	fail bool
}

func newRunHarness(t *testing.T, cfg CheckerConfig) (*runHarness, *Checker) {
	t.Helper()
	h := &runHarness{t: t, now: time.Date(2026, 11, 12, 23, 8, 0, 0, time.UTC)}
	withStubbedLatest(t, func(context.Context) (*Release, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.checks++
		h.mark = h.nowCalls
		if h.fail {
			return nil, errors.New("lookup github.com: no such host")
		}
		return &Release{Tag: cfg.CurrentVersion}, nil
	})
	c := NewChecker(cfg)
	c.now = h.clock
	c.pollEvery = 2 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	// Run reads the clock to set the first check and again to wait for it;
	// advancing before that would move the first check along with the clock.
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		started := h.nowCalls >= 2
		h.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run never read the clock")
		}
		time.Sleep(time.Millisecond)
	}
	return h, c
}

func (h *runHarness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nowCalls++
	return h.now
}

func (h *runHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func (h *runHarness) setFail(fail bool) {
	h.mu.Lock()
	h.fail = fail
	h.mu.Unlock()
}

// waitChecks waits until n lookups ran and Run has scheduled the next one.
func (h *runHarness) waitChecks(n int, why string) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		ok := h.checks >= n && h.nowCalls >= h.mark+2
		got := h.checks
		h.mu.Unlock()
		if ok {
			if got > n {
				h.t.Fatalf("%s: %d checks, want %d", why, got, n)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.mu.Lock()
	got := h.checks
	h.mu.Unlock()
	h.t.Fatalf("%s: %d checks after 5s, want %d", why, got, n)
}

// expectChecks asserts the count stays at n over many poll periods.
func (h *runHarness) expectChecks(n int, why string) {
	h.t.Helper()
	time.Sleep(60 * time.Millisecond) // ~30 poll periods
	h.mu.Lock()
	got := h.checks
	h.mu.Unlock()
	if got != n {
		h.t.Fatalf("%s: %d checks, want %d", why, got, n)
	}
}

// A wall-clock jump past the interval — the machine slept through it — must
// bring the check forward at once, not after another interval of awake time.
func TestRun_ChecksOnceWallClockPassesInterval(t *testing.T) {
	h, _ := newRunHarness(t, CheckerConfig{CurrentVersion: "v1.0.0", Mode: ModeNotify, Interval: 6 * time.Hour})

	h.expectChecks(0, "before the interval")
	h.advance(6*time.Hour - time.Minute)
	h.expectChecks(0, "a minute short of the interval")
	h.advance(14 * time.Hour) // asleep overnight
	h.waitChecks(1, "after the wall clock passed the interval")
}

// A failed lookup is retried after 5, 15, 30 minutes… rather than a whole
// interval; the first success puts the schedule back on the interval.
func TestRun_FailedCheckRetriesWithBackoffThenReturnsToInterval(t *testing.T) {
	h, _ := newRunHarness(t, CheckerConfig{CurrentVersion: "v1.0.0", Mode: ModeNotify, Interval: 6 * time.Hour})
	h.setFail(true)

	h.advance(6 * time.Hour)
	h.waitChecks(1, "first check")

	h.advance(4 * time.Minute)
	h.expectChecks(1, "4 minutes after the first failure")
	h.advance(time.Minute)
	h.waitChecks(2, "5 minutes after the first failure")

	h.advance(15 * time.Minute)
	h.waitChecks(3, "15 minutes after the second failure")

	h.setFail(false)
	h.advance(29 * time.Minute)
	h.expectChecks(3, "29 minutes after the third failure")
	h.advance(time.Minute)
	h.waitChecks(4, "30 minutes after the third failure")

	h.advance(time.Hour)
	h.expectChecks(4, "an hour after a success: the backoff must be reset")
	h.advance(5*time.Hour - time.Minute)
	h.expectChecks(4, "a minute short of the interval after a success")
	h.advance(time.Minute)
	h.waitChecks(5, "an interval after the success")
}

// CheckOnStart still checks shortly after start, by the same wall clock.
func TestRun_CheckOnStart(t *testing.T) {
	h, _ := newRunHarness(t, CheckerConfig{CurrentVersion: "v1.0.0", Mode: ModeNotify, Interval: 6 * time.Hour, CheckOnStart: true})
	h.expectChecks(0, "before the start delay")
	h.advance(checkOnStartDelay)
	h.waitChecks(1, "after the start delay")
	h.advance(checkOnStartDelay)
	h.expectChecks(1, "the next check waits the interval")
}

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		failures int
		interval time.Duration
		want     time.Duration
	}{
		{1, 6 * time.Hour, 5 * time.Minute},
		{2, 6 * time.Hour, 15 * time.Minute},
		{3, 6 * time.Hour, 30 * time.Minute},
		{4, 6 * time.Hour, time.Hour},
		{5, 6 * time.Hour, 2 * time.Hour},
		{6, 6 * time.Hour, 4 * time.Hour},
		{7, 6 * time.Hour, 6 * time.Hour},
		{1000, 6 * time.Hour, 6 * time.Hour},
		{5, time.Hour, time.Hour},
		{2, 10 * time.Minute, 10 * time.Minute},
		{0, 6 * time.Hour, 5 * time.Minute},
	}
	for _, tc := range cases {
		if got := retryDelay(tc.failures, tc.interval); got != tc.want {
			t.Errorf("retryDelay(%d, %s) = %s, want %s", tc.failures, tc.interval, got, tc.want)
		}
	}
}

// The schedule's comparisons must not fall back to the monotonic reading,
// which on macOS does not count the time asleep.
func TestWallNowHasNoMonotonicReading(t *testing.T) {
	if s := wallNow().String(); strings.Contains(s, "m=") {
		t.Fatalf("wallNow() = %s carries a monotonic reading", s)
	}
	if NewChecker(CheckerConfig{Interval: time.Hour}).now == nil {
		t.Fatal("NewChecker must set the clock")
	}
}

// A wall clock set backwards (a manual change, an NTP step) must not push the
// next check further out than one interval.
func TestRun_ClockSetBackwardsKeepsTheIntervalBound(t *testing.T) {
	h, _ := newRunHarness(t, CheckerConfig{CurrentVersion: "v1.0.0", Mode: ModeNotify, Interval: 6 * time.Hour})
	h.advance(-48 * time.Hour)
	h.expectChecks(0, "right after the clock went back")
	h.advance(6 * time.Hour)
	h.waitChecks(1, "an interval after the clock went back")
}
