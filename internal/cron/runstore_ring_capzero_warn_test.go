package cron

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// TestRingCapZero_WarnsOncePerProcess pins R249-ARCH-13 (#979): the cap=0
// self-heal branch in ringRead / ringSnapshot used to return silently, so a
// regression that left count>0 while cap(ring)==0 (a ringSeed bypass) would
// surface only as mysteriously-empty dashboard lists. The guard now logs
// exactly once. Verify: (a) the self-heal still returns the empty zero value
// without panicking, and (b) a warn line is emitted on the first hit and
// suppressed on subsequent hits (once-per-process).
//
// Not parallel: swaps the process-global slog default + resets the package
// sync.Once so the assertion is deterministic.
func TestRingCapZero_WarnsOncePerProcess(t *testing.T) {
	// Reset the once so this test observes the first-fire regardless of
	// whatever ran before it in the same binary.
	ringCapZeroWarnOnce = sync.Once{}
	t.Cleanup(func() { ringCapZeroWarnOnce = sync.Once{} })

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Entry with count>0 but cap(ring)==0 — the regression shape ringSeed
	// would never produce, but defensive code must tolerate.
	e := &recentCacheEntry{count: 3}

	// ringRead must self-heal to the zero value, not panic on % by zero.
	if got := e.ringRead(0); got != (CronRunSummary{}) {
		t.Fatalf("ringRead cap=0 = %+v, want zero CronRunSummary", got)
	}
	// ringSnapshot must self-heal to nil, not panic.
	if got := e.ringSnapshot(0); got != nil {
		t.Fatalf("ringSnapshot cap=0 = %v, want nil", got)
	}

	out := buf.String()
	if !strings.Contains(out, "ring cap=0") {
		t.Fatalf("expected a cap=0 warn line, got: %q", out)
	}
	if n := strings.Count(out, "ring cap=0"); n != 1 {
		t.Fatalf("expected exactly 1 warn (once-per-process), got %d: %q", n, out)
	}
}

// TestRingCapZero_BenignEmptyStaysSilent guards the negative: a genuinely
// empty entry (count==0) is the normal cold-cache fast path and must NOT emit
// the regression warn — otherwise the metric/log signal is useless noise.
func TestRingCapZero_BenignEmptyStaysSilent(t *testing.T) {
	ringCapZeroWarnOnce = sync.Once{}
	t.Cleanup(func() { ringCapZeroWarnOnce = sync.Once{} })

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := &recentCacheEntry{} // count==0, cap==0 — benign empty cache
	if got := e.ringSnapshot(0); got != nil {
		t.Fatalf("ringSnapshot empty = %v, want nil", got)
	}
	if strings.Contains(buf.String(), "ring cap=0") {
		t.Fatalf("benign empty cache must not warn, got: %q", buf.String())
	}
}
