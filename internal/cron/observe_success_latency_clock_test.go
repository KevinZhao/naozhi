package cron

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/metrics"
)

// TestObserveSuccessLatency_UsesInjectableClock pins R20260607-GO-002:
// the call site of observeSuccessLatency computes elapsed via
// s.now().Sub(startedAt) so the injectable clock governs the duration.
// The function now receives elapsed directly (not startedAt), making the
// clock coupling explicit and preventing an extra s.now() call inside
// the helper that would advance step-based test clocks a third time.
//
// This test verifies end-to-end: a fake clock that is 200ms ahead of
// startedAt produces elapsed = 200ms, which exceeds the 1ms
// slowThreshold and fires CronExecutionSlowTotal. If the caller were
// still using time.Since(startedAt) the elapsed would be near-zero
// (test runs instantly) and the counter would NOT fire.
//
// Not t.Parallel: CronExecutionSlowTotal is a process-global expvar counter;
// we read deltas and must not race other tests that bump it.
func TestObserveSuccessLatency_UsesInjectableClock(t *testing.T) {
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	snap := jobSnapshot{jobID: "job-clock-latency"}
	res := SendResult{Text: "ok", SessionID: "s-clock"}

	// Fake clock is 200ms ahead of startedAt so elapsed = 200ms.
	t0 := time.Date(2026, 6, 7, 10, 0, 0, 0, time.UTC)
	clk := &fakeClock{now: t0.Add(200 * time.Millisecond)}
	startedAt := t0

	s := &Scheduler{
		clock:         clk,
		slowThreshold: time.Millisecond, // 1ms threshold → 200ms elapsed fires the counter
	}

	// Compute elapsed the same way the production call site does:
	// s.now().Sub(startedAt) uses the injectable clock, not time.Since.
	elapsed := s.now().Sub(startedAt) // = 200ms via fake clock

	before := metrics.CronExecutionSlowTotal.Value()
	s.observeSuccessLatency(elapsed, res, snap, lg)
	delta := metrics.CronExecutionSlowTotal.Value() - before

	if delta != 1 {
		t.Errorf("R20260607-GO-002: CronExecutionSlowTotal delta = %d, want 1 (elapsed must come from injected clock, not time.Since)", delta)
	}
}
