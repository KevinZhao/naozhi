package cron

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestRingCapZero_WarnsOncePerEntry pins R20260602-CR-4: the cap=0
// self-heal branch in ringRead / ringSnapshot logs exactly once per
// recentCacheEntry (not once per process as the old package-level sync.Once
// did). Verify: (a) the self-heal still returns the empty zero value without
// panicking, (b) a warn line is emitted on the first hit for an entry,
// (c) a second hit on the same entry is suppressed, and (d) a fresh entry
// (capZeroWarned=false) independently fires its own warn — proving that
// parallel tests or multiple entries do not silence each other.
//
// Not parallel: swaps the process-global slog default for deterministic
// log capture.
func TestRingCapZero_WarnsOncePerEntry(t *testing.T) {
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
	// Only one warn per entry: capZeroWarned.CompareAndSwap prevents duplicates.
	if n := strings.Count(out, "ring cap=0"); n != 1 {
		t.Fatalf("expected exactly 1 warn (once-per-entry), got %d: %q", n, out)
	}

	// Reset buffer; a second hit on the same entry must be suppressed.
	buf.Reset()
	_ = e.ringRead(0)
	if strings.Contains(buf.String(), "ring cap=0") {
		t.Fatalf("second hit on same entry must not warn again; got: %q", buf.String())
	}

	// A fresh entry must independently warn on its own first hit, proving
	// different entries don't share a gate (the old sync.Once bug).
	var buf2 bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelWarn})))
	e2 := &recentCacheEntry{count: 1}
	_ = e2.ringRead(0)
	if !strings.Contains(buf2.String(), "ring cap=0") {
		t.Fatalf("fresh entry must warn independently; got: %q", buf2.String())
	}
}

// TestRingCapZero_BenignEmptyStaysSilent guards the negative: a genuinely
// empty entry (count==0) is the normal cold-cache fast path and must NOT emit
// the regression warn — otherwise the metric/log signal is useless noise.
func TestRingCapZero_BenignEmptyStaysSilent(t *testing.T) {
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
