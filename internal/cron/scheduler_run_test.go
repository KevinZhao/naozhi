package cron

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/sessionkey"
)

// TestRunDeadlineWatchdog_NoIdleGoroutine_R247_GO_12 is the regression test
// for R247-GO-12 (#492). Pre-fix, runDeadlineWatchdog spawned a long-lived
// goroutine waiting on `<-ctx.Done()` for every cron tick — at 50 jobs @ 1Hz
// this held ~50 watchdog goroutines concurrently for the entire Send window.
// The fix uses context.AfterFunc, which only spawns a goroutine when ctx
// actually ends (briefly, to run the callback), shrinking the steady-state
// in-flight watchdog goroutine count to ~0.
//
// The test registers many watchdogs against contexts that have NOT been
// cancelled and asserts the goroutine count remains within a small constant
// of the baseline — proving no per-watchdog goroutine is alive while ctx is
// still live. After cancelling all contexts and draining, the count returns
// to baseline (the AfterFunc callbacks have run and exited).
func TestRunDeadlineWatchdog_NoIdleGoroutine_R247_GO_12(t *testing.T) {
	// NOT t.Parallel() — sensitive to background goroutine churn from
	// other parallel tests in the package.

	const N = 64

	// Drain any goroutines that might still be spinning down from earlier
	// tests in the suite before sampling the baseline.
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	cancels := make([]context.CancelFunc, 0, N)
	channels := make([]<-chan abortResult, 0, N)
	for i := 0; i < N; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		ci := &countingInterrupter{outcome: InterruptSent}
		channels = append(channels, runDeadlineWatchdog(ctx, ci))
	}

	// Give the runtime a moment to schedule any goroutines that the old
	// implementation would have spawned. With AfterFunc registration alone,
	// no per-watchdog goroutines should appear.
	runtime.Gosched()
	time.Sleep(5 * time.Millisecond)

	live := runtime.NumGoroutine()
	// Allow a small slack for unrelated runtime goroutines (GC sweeper,
	// timer dispatch) — but the previous implementation would have spawned
	// at least N goroutines, well beyond a constant slack.
	const slack = 8
	if live > baseline+slack {
		t.Fatalf("watchdog goroutines leaked while ctx live: baseline=%d live=%d (delta=%d, slack=%d, N=%d)",
			baseline, live, live-baseline, slack, N)
	}

	// Cancel all contexts; each AfterFunc callback runs once and publishes
	// abortResult{fired:false} to its channel. Drain to be certain.
	for _, cancel := range cancels {
		cancel()
	}
	for _, ch := range channels {
		select {
		case abort := <-ch:
			if abort.fired {
				t.Fatalf("abort.fired = true on explicit cancel; want false")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("AfterFunc callback never published abort on cancel")
		}
	}
}

// stubRefreshCountingRouter records RegisterCronStubWithChain calls so the
// stubRefresher.run() contract (R249-ARCH-25 #989) can be asserted without a
// real session router.
type stubRefreshCountingRouter struct {
	registers int
	lastKey   string
}

func (r *stubRefreshCountingRouter) RegisterCronStubWithChain(key, _, _ string, _ []string) {
	r.registers++
	r.lastKey = key
}
func (r *stubRefreshCountingRouter) Reset(string) {}
func (r *stubRefreshCountingRouter) GetOrCreate(context.Context, string, AgentOpts) (Session, SessionStatus, error) {
	return nil, SessionExisting, nil
}

// TestStubRefresher_R249_ARCH_25 pins the typed stubRefresher that replaced
// freshContextPreflightP0's bare closure (#989):
//   - the zero value (active=false) is a safe no-op — never touches the router;
//   - an active refresher re-registers the sidebar stub iff the job still
//     exists in s.jobs at run() time;
//   - an active refresher for a job deleted between preflight and run() does
//     NOT re-register (prevents a phantom sidebar row for a gone job).
func TestStubRefresher_R249_ARCH_25(t *testing.T) {
	t.Parallel()

	router := &stubRefreshCountingRouter{}
	s := NewScheduler(SchedulerConfig{MaxJobs: 5, Router: router})

	// Zero value: no-op.
	var zero stubRefresher
	zero.run()
	if router.registers != 0 {
		t.Fatalf("zero-value run() must be a no-op; got %d registers", router.registers)
	}

	// Active + job present: re-registers via the router.
	s.mu.Lock()
	s.jobs["job-a"] = &Job{ID: "job-a", Schedule: "@every 5m"}
	s.mu.Unlock()
	active := stubRefresher{s: s, jobID: "job-a", workDir: "/tmp", prompt: "p", active: true}
	active.run()
	if router.registers != 1 {
		t.Fatalf("active run() with live job: want 1 register, got %d", router.registers)
	}
	if want := sessionkey.CronKey("job-a"); router.lastKey != want {
		t.Fatalf("register key = %q, want %q", router.lastKey, want)
	}

	// Active but job deleted between preflight and run(): no re-register.
	gone := stubRefresher{s: s, jobID: "job-gone", workDir: "/tmp", prompt: "p", active: true}
	gone.run()
	if router.registers != 1 {
		t.Fatalf("run() for a deleted job must not re-register; got %d total", router.registers)
	}
}
