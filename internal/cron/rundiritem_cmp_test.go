package cron

import (
	"slices"
	"testing"
	"time"
)

// TestRunDirItemNewestFirst pins the shared comparator's ordering contract:
// mtime DESC, then runID DESC on equal mtime (R20260527122801-PERF-2 / #1340
// lifted it from a per-scan closure literal). Both trimJobLocked and
// diskListNewestFirst depend on this exact total order, so a drift would
// desync the cap cutoff (i < keepCount) from the list cutoff.
func TestRunDirItemNewestFirst(t *testing.T) {
	t.Parallel()
	base := time.Unix(1_700_000_000, 0)
	items := []runDirItem{
		{runID: "aaaa1111", mtime: base},                      // oldest
		{runID: "bbbb2222", mtime: base.Add(2 * time.Minute)}, // newest
		{runID: "cccc3333", mtime: base.Add(time.Minute)},     // equal-mtime pair A
		{runID: "dddd4444", mtime: base.Add(time.Minute)},     // equal-mtime pair B
	}
	slices.SortFunc(items, runDirItemNewestFirst)

	want := []string{
		"bbbb2222", // newest mtime
		"dddd4444", // tie at base+1m, runID DESC → dddd before cccc
		"cccc3333",
		"aaaa1111", // oldest
	}
	for i, w := range want {
		if items[i].runID != w {
			t.Fatalf("items[%d].runID = %q want %q (order: %+v)", i, items[i].runID, w, items)
		}
	}
}
