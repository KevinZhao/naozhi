package selfupdate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests swap the package-level latestRelease stub, so per this package's
// convention they must NOT call t.Parallel().

// TestCheckNowThrottle guards the property that makes it safe to call CheckNow
// from a GET handler at all: the throttle is GLOBAL, so N dashboard tabs
// polling simultaneously cannot amplify into N requests against GitHub.
func TestCheckNowThrottle(t *testing.T) {
	var calls atomic.Int32
	orig := latestRelease
	latestRelease = func(context.Context) (*Release, error) {
		calls.Add(1)
		return &Release{Tag: "v1.1.0"}, nil
	}
	defer func() { latestRelease = orig }()

	st := NewStatus("v1.0.0")
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Interval:       time.Hour,
		Status:         st,
	})

	if err := c.CheckNow(context.Background()); err != nil {
		t.Fatalf("first CheckNow: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("after first CheckNow, latestRelease calls = %d, want 1", got)
	}
	if snap := st.Snapshot(); snap.Latest != "v1.1.0" {
		t.Fatalf("Status.Latest = %q, want v1.1.0", snap.Latest)
	}

	// Immediate second call must be declined without touching the network.
	if err := c.CheckNow(context.Background()); !errors.Is(err, ErrCheckThrottled) {
		t.Fatalf("second CheckNow err = %v, want ErrCheckThrottled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("throttled call still hit the network: calls = %d, want 1", got)
	}
}

// TestCheckNowThrottleIsGlobalAcrossConcurrentCallers is the multi-tab case:
// many simultaneous requests, at most one outbound lookup.
func TestCheckNowThrottleIsGlobalAcrossConcurrentCallers(t *testing.T) {
	var calls atomic.Int32
	orig := latestRelease
	latestRelease = func(context.Context) (*Release, error) {
		calls.Add(1)
		// Hold the "connection" briefly so the callers genuinely overlap.
		time.Sleep(10 * time.Millisecond)
		return &Release{Tag: "v1.1.0"}, nil
	}
	defer func() { latestRelease = orig }()

	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Interval:       time.Hour,
		Status:         NewStatus("v1.0.0"),
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.CheckNow(context.Background())
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("8 concurrent CheckNow calls produced %d GitHub lookups, want exactly 1", got)
	}
}

// TestCheckNowStampsBeforeNetworkCall covers a subtle sequencing requirement:
// the throttle timestamp is written BEFORE the lookup, so a slow failure still
// consumes the interval. Stamping afterwards would let every poll during a
// GitHub outage queue up behind the same dead endpoint.
func TestCheckNowStampsBeforeNetworkCall(t *testing.T) {
	var calls atomic.Int32
	orig := latestRelease
	latestRelease = func(context.Context) (*Release, error) {
		calls.Add(1)
		return nil, errors.New("dial tcp: i/o timeout")
	}
	defer func() { latestRelease = orig }()

	st := NewStatus("v1.0.0")
	c := NewChecker(CheckerConfig{
		CurrentVersion: "v1.0.0",
		Interval:       time.Hour,
		Status:         st,
	})

	if err := c.CheckNow(context.Background()); err == nil {
		t.Fatal("CheckNow should surface the lookup error")
	}
	// The failure must still have consumed the throttle window.
	if err := c.CheckNow(context.Background()); !errors.Is(err, ErrCheckThrottled) {
		t.Fatalf("after a FAILED check, next CheckNow err = %v, want ErrCheckThrottled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("latestRelease calls = %d, want 1 (a failed check must still throttle)", got)
	}
	// And the failure has to be visible to the dashboard.
	if snap := st.Snapshot(); snap.CheckErr == "" {
		t.Error("Status.CheckErr empty after a failed on-demand check")
	}
}

// TestCheckNowSkipsDevBuild keeps developer machines from reaching out to
// GitHub at all — there is no meaningful comparison for a local build.
func TestCheckNowSkipsDevBuild(t *testing.T) {
	var calls atomic.Int32
	orig := latestRelease
	latestRelease = func(context.Context) (*Release, error) {
		calls.Add(1)
		return &Release{Tag: "v1.1.0"}, nil
	}
	defer func() { latestRelease = orig }()

	c := NewChecker(CheckerConfig{
		CurrentVersion: "dev",
		Interval:       time.Hour,
		Status:         NewStatus("dev"),
	})
	if err := c.CheckNow(context.Background()); !errors.Is(err, ErrCheckSkippedDev) {
		t.Fatalf("CheckNow on a dev build err = %v, want ErrCheckSkippedDev", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("dev build reached the network: calls = %d, want 0", got)
	}
}

// TestCheckNowNilChecker: the server holds a possibly-nil *Checker (auto-update
// disabled), and the handler calls through without a guard of its own.
func TestCheckNowNilChecker(t *testing.T) {
	var c *Checker
	if err := c.CheckNow(context.Background()); err == nil {
		t.Fatal("CheckNow on a nil Checker should return an error, not panic or succeed")
	}
}
