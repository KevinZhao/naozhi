package leakcheck

import (
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitGoroutinesSettle blocks until runtime.NumGoroutine() reports the
// same value across several consecutive reads, i.e. no goroutine from a
// prior test in this package is still winding down. CheckWith captures
// its baseline at call time; if a previous test's goroutine is still
// counted then (the runtime decrement lags goexit by a scheduler tick),
// the baseline is inflated by one. When that straggler then exits while
// our deliberately-leaked goroutine adds one, the net count is flat and
// the detector sees no growth — a false negative that flakes
// TestCheckWith_DetectsLeak. Settling first pins an honest baseline.
func waitGoroutinesSettle(t *testing.T) {
	t.Helper()
	prev := runtime.NumGoroutine()
	stable := 0
	for i := 0; i < 200; i++ {
		time.Sleep(5 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n == prev {
			if stable++; stable >= 3 {
				return
			}
			continue
		}
		prev = n
		stable = 0
	}
}

// fakeTB is a minimal TB implementation that captures Errorf calls so
// the package's own tests can verify the failure path without flunking
// the parent test. We can't reuse a real *testing.T because Errorf on
// a real T would mark the test as failed.
type fakeTB struct {
	helperCalls int
	errors      []string
}

func (f *fakeTB) Helper()                           { f.helperCalls++ }
func (f *fakeTB) Errorf(format string, args ...any) { f.errors = append(f.errors, format) }
func (f *fakeTB) failed() bool                      { return len(f.errors) > 0 }

// TestCheck_NoLeak — a clean test path with no leaked goroutines must
// not fail. Without this baseline a future tightening of the grace
// window could silently break every callsite.
func TestCheck_NoLeak(t *testing.T) {
	defer Check(t)()
	// A goroutine that exits before the deferred Check runs.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
	}()
	wg.Wait()
}

// TestCheckWith_DetectsLeak verifies the failure path. We feed a
// fakeTB into CheckWith so we can observe an Errorf call without
// flunking the surrounding *testing.T. The leaked goroutine receives
// a stop signal so the process exits cleanly when the test binary
// terminates.
func TestCheckWith_DetectsLeak(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	// Pin an honest baseline: if a goroutine from a prior test is still
	// winding down when CheckWith snapshots the count, its later exit
	// cancels out our deliberate +1 leak and the detector sees no growth.
	waitGoroutinesSettle(t)

	fake := &fakeTB{}
	done := CheckWith(fake, 0, 50*time.Millisecond)
	// Deliberately leak a goroutine: it never exits within the settle
	// window, so the deferred check should fail.
	go func() {
		<-stop
	}()
	done()

	if !fake.failed() {
		t.Fatalf("CheckWith with a leaked goroutine should have called Errorf; got no errors")
	}
	if !strings.Contains(fake.errors[0], "leakcheck") {
		t.Errorf("error message should mention package name; got %q", fake.errors[0])
	}
}

// TestCheckWith_RespectsGrace pins the tolerance contract: a single
// extra goroutine that stays parked for the duration of the check
// should not trip the leak detector when grace >= 1.
func TestCheckWith_RespectsGrace(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })

	fake := &fakeTB{}
	done := CheckWith(fake, 5, 50*time.Millisecond)
	go func() {
		<-stop
	}()
	done()

	if fake.failed() {
		t.Errorf("CheckWith with grace=5 should have ignored a single extra goroutine; got errors %v", fake.errors)
	}
}

// TestCheck_AcceptsRealT documents the standard usage form a caller
// outside this package will write. A compile failure here would mean
// the public API was accidentally narrowed during a refactor.
func TestCheck_AcceptsRealT(t *testing.T) {
	defer Check(t)()
}
