package cron

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// withCapturedSlog redirects the package-default slog to an in-memory
// buffer for the duration of the test, returning a func to read what was
// logged. NOT t.Parallel-safe — slog.SetDefault is process-global; tests
// using this helper run serially (no t.Parallel inside the cases below).
func withCapturedSlog(t *testing.T) (read func() string, restore func()) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	return func() string { return buf.String() }, func() { slog.SetDefault(prev) }
}

// TestNewScheduler_NilRouterEmitsError pins R241-ARCH-6 (#510): when
// Router is nil and AllowNilRouter is unset, NewScheduler logs a loud
// slog.Error at boot so the wireup bug surfaces immediately. Without
// this contract the misconfiguration only became visible as a permanently
// empty dashboard sidebar — a silent symptom that's hard to trace back
// to the missing Router config.
func TestNewScheduler_NilRouterEmitsError(t *testing.T) {
	// Not t.Parallel: slog.SetDefault is process-global.
	read, restore := withCapturedSlog(t)
	defer restore()

	_ = NewScheduler(SchedulerConfig{
		MaxJobs: 5,
		// Router intentionally omitted, AllowNilRouter not set.
	})

	got := read()
	if !strings.Contains(got, "cfg.Router is nil") {
		t.Errorf("expected slog.Error containing 'cfg.Router is nil'; got %q", got)
	}
	if !strings.Contains(got, "level=ERROR") {
		t.Errorf("expected level=ERROR (not WARN/INFO); got %q", got)
	}
}

// TestNewScheduler_NilRouterWithAllowNilRouterStaysSilent pins that the
// AllowNilRouter opt-in suppresses the boot-time slog.Error so the test
// suite — which has many fixtures that never reach executeOpt or
// registerStub — can opt into silence without coupling those tests to a
// fakeRouter dependency.
func TestNewScheduler_NilRouterWithAllowNilRouterStaysSilent(t *testing.T) {
	read, restore := withCapturedSlog(t)
	defer restore()

	_ = NewScheduler(SchedulerConfig{
		MaxJobs:        5,
		AllowNilRouter: true,
	})

	got := read()
	if strings.Contains(got, "cfg.Router is nil") {
		t.Errorf("expected silence with AllowNilRouter=true; got %q", got)
	}
}

// TestRegisterStubByValue_NilRouterLogsOnce pins that the per-call
// slog.Error inside registerStubByValue fires at most once per Scheduler
// lifetime, so a router-less fixture (or a deployment that opted into
// AllowNilRouter for whatever reason) doesn't spam the log across N
// ticks. The first call must log; subsequent calls must stay silent.
func TestRegisterStubByValue_NilRouterLogsOnce(t *testing.T) {
	// Construct first (which itself emits the boot-time slog.Error if
	// AllowNilRouter is not set; we set it true to isolate the per-call
	// log).
	s := NewScheduler(SchedulerConfig{
		MaxJobs:        5,
		AllowNilRouter: true,
	})

	read, restore := withCapturedSlog(t)
	defer restore()

	// First call: should log.
	s.registerStubByValue("job1", "/tmp", "p1", "")
	first := read()
	if !strings.Contains(first, "registerStubByValue called without a router") {
		t.Errorf("first call must log slog.Error; got %q", first)
	}

	// Second + third calls: must stay silent (Once gate).
	restore() // pop our capture (re-applies prev)
	read2, restore2 := withCapturedSlog(t)
	defer restore2()
	s.registerStubByValue("job2", "/tmp", "p2", "")
	s.registerStubByValue("job3", "/tmp", "p3", "")
	rest := read2()
	if strings.Contains(rest, "registerStubByValue called without a router") {
		t.Errorf("subsequent calls must stay silent (sync.Once gate); got %q", rest)
	}
}
