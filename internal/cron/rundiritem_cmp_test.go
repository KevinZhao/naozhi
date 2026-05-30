package cron

import (
	"slices"
	"testing"
	"time"
)

// TestRunDirItemNewestFirst pins the hoisted package-level comparator
// (#1361) to the same order the prior inline closure produced: mtime DESC
// with a runID DESC tie-break on equal mtimes.
func TestRunDirItemNewestFirst(t *testing.T) {
	t.Parallel()

	base := time.Unix(1_700_000_000, 0)
	older := runDirItem{runID: "aaaa", mtime: base}
	newer := runDirItem{runID: "bbbb", mtime: base.Add(time.Second)}

	if got := runDirItemNewestFirst(newer, older); got >= 0 {
		t.Fatalf("newer should sort before older, got cmp=%d", got)
	}
	if got := runDirItemNewestFirst(older, newer); got <= 0 {
		t.Fatalf("older should sort after newer, got cmp=%d", got)
	}

	// Equal mtime: runID DESC tie-break (larger runID first).
	hi := runDirItem{runID: "ffff", mtime: base}
	lo := runDirItem{runID: "0000", mtime: base}
	if got := runDirItemNewestFirst(hi, lo); got >= 0 {
		t.Fatalf("equal mtime: larger runID should sort first, got cmp=%d", got)
	}

	// Full sort produces newest-first, runID-DESC-on-tie ordering.
	// newer(bbbb) has the latest mtime; older(aaaa), hi(ffff) and lo(0000)
	// share base mtime so tie-break by runID DESC orders them ffff>aaaa>0000.
	items := []runDirItem{older, lo, hi, newer}
	slices.SortFunc(items, runDirItemNewestFirst)
	want := []string{"bbbb", "ffff", "aaaa", "0000"}
	for i, w := range want {
		if items[i].runID != w {
			t.Fatalf("sorted[%d] = %q, want %q (order=%v)", i, items[i].runID, w, items)
		}
	}
}
