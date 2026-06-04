package cron

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// TestObserveSuccessLatency_SlowCounterFires pins the extraction of the
// success-path observability tail out of executeOpt (#1681 / #423): a run
// whose elapsed exceeds slowThreshold must bump CronExecutionSlowTotal, and a
// fast run must not. Guards against a regression that drops the slow-tail
// signal during a future executeOpt split.
//
// Not t.Parallel: CronExecutionSlowTotal is a process-global expvar counter,
// so the test reads deltas rather than absolutes and must not race other
// tests that bump it.
func TestObserveSuccessLatency_SlowCounterFires(t *testing.T) {
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	snap := jobSnapshot{jobID: "job-slow"}
	res := SendResult{Text: "ok", SessionID: "s1"}

	// Slow run: startedAt well before now with a tiny threshold → counter +1.
	s := &Scheduler{slowThreshold: time.Millisecond}
	before := metrics.CronExecutionSlowTotal.Value()
	s.observeSuccessLatency(time.Now().Add(-50*time.Millisecond), res, snap, lg)
	if got := metrics.CronExecutionSlowTotal.Value() - before; got != 1 {
		t.Errorf("slow run: CronExecutionSlowTotal delta = %d, want 1", got)
	}

	// Fast run: startedAt = now with a large threshold → no increment.
	sFast := &Scheduler{slowThreshold: time.Hour}
	before = metrics.CronExecutionSlowTotal.Value()
	sFast.observeSuccessLatency(time.Now(), res, snap, lg)
	if got := metrics.CronExecutionSlowTotal.Value() - before; got != 0 {
		t.Errorf("fast run: CronExecutionSlowTotal delta = %d, want 0", got)
	}
}

// TestObserveSuccessLatency_DefaultThreshold pins that a zero/unset
// slowThreshold falls back to defaultCronSlowThreshold rather than treating
// every run as slow (a regression would bump the counter on every success).
func TestObserveSuccessLatency_DefaultThreshold(t *testing.T) {
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	snap := jobSnapshot{jobID: "job-default"}
	res := SendResult{Text: "ok"}

	s := &Scheduler{slowThreshold: 0} // unset → defaultCronSlowThreshold
	before := metrics.CronExecutionSlowTotal.Value()
	// A run that completes "now" is far under the default threshold.
	s.observeSuccessLatency(time.Now(), res, snap, lg)
	if got := metrics.CronExecutionSlowTotal.Value() - before; got != 0 {
		t.Errorf("default-threshold fast run: CronExecutionSlowTotal delta = %d, want 0", got)
	}
}
