package cron

import (
	"sync/atomic"
	"testing"
	"time"
)

// append_single_clock_read_test.go — proves Append reads the clock exactly once
// per trim, replacing a regexp over runstore.go. Epic I #2547.
//
// R20260603-PERF-11's property: `now` is captured once and shared by
// skipAppendTrim's window-cutoff check and trimJobLocked, rather than each
// calling time.Now(). The old pin scanned the function body for one
// `now := time.Now()` and for `now` being passed to both callees — text, so a
// refactor that keeps the shape but adds a third read elsewhere in the block
// passes, and a rename fails for nothing.
//
// countingClock makes the real property observable: the count IS the assertion.
//
// This replaced append_single_now_test.go, whose checks were: one
// `now := time.Now()` in the body, `now` passed to skipAppendTrim, `now` passed
// to trimJobLocked. Verified that the count catches what the text could not:
// changing the first call to skipAppendTrim(run.JobID, s.now()) — which keeps
// every one of those three strings intact — gives 16 reads across 8 Appends.

type countingClock struct {
	reads atomic.Int64
	step  time.Duration
	base  time.Time
}

func (c *countingClock) Now() time.Time {
	n := c.reads.Add(1)
	// Each read advances, so two reads inside one Append would hand
	// skipAppendTrim and trimJobLocked different cutoffs.
	return c.base.Add(time.Duration(n-1) * c.step)
}

// TestAppend_ReadsTheClockOncePerTrim asserts the count directly.
func TestAppend_ReadsTheClockOncePerTrim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, 4, time.Hour)
	clk := &countingClock{step: time.Minute, base: time.Now()}
	s.clock = clk
	jobID := mustGenerateID()

	// The first Append into an empty job dir may skip the trim block entirely
	// (nothing to trim), so append enough to be past keepCount and guarantee the
	// block runs.
	const appends = 8
	for i := range appends {
		s.Append(makeRun(jobID, clk.base.Add(time.Duration(i)*time.Second)))
	}

	got := clk.reads.Load()
	if got == 0 {
		t.Fatal("the clock was never read; Append no longer reads it, so this test proves nothing about sharing one instant")
	}
	// One read per Append that reaches the trim block. Two reads per Append would
	// be the regression: the whole point is that skipAppendTrim and trimJobLocked
	// share one instant.
	if got > appends {
		t.Errorf("clock reads = %d across %d Appends, want <= %d — Append is reading the clock more than once per trim, so skipAppendTrim and trimJobLocked no longer share one instant (R20260603-PERF-11)",
			got, appends, appends)
	}
}

// A second test asserting the CONSEQUENCE (that both consumers saw the same
// cutoff, by checking which records survive) was written and dropped: with a
// stepping clock the steps accumulate across Appends, so after six Appends at two
// hours each every record is legitimately outside keepWindow and the eviction is
// correct behaviour rather than the bug. The count above is the property anyway —
// with exactly one read per trim, sharing the instant is not a separate fact.
