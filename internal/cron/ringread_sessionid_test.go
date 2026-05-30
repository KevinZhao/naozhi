package cron

import (
	"testing"
)

// TestRingReadSessionID_ParityWithRingRead pins the in-place SessionID
// reader (#1285 follow-up) to the same value ringRead(i).SessionID
// produces, across the ring wrap boundary, plus the cap==0 self-heal guard.
func TestRingReadSessionID_ParityWithRingRead(t *testing.T) {
	t.Parallel()

	e := &recentCacheEntry{}
	const keep = 3
	e.ringSeed(nil, keep) // allocate ring with cap=keep, count=0

	// Push more than cap so head wraps and the oldest is evicted.
	for _, sid := range []string{"a", "b", "c", "d"} {
		e.ringPushHead(CronRunSummary{SessionID: sid, RunID: sid})
	}

	if e.count != keep {
		t.Fatalf("count=%d want %d", e.count, keep)
	}
	for i := 0; i < e.count; i++ {
		want := e.ringRead(i).SessionID
		if got := e.ringReadSessionID(i); got != want {
			t.Fatalf("ringReadSessionID(%d)=%q want %q", i, got, want)
		}
	}

	// cap==0 guard: a zero-value entry must return "" rather than panic.
	var zero recentCacheEntry
	if got := zero.ringReadSessionID(0); got != "" {
		t.Fatalf("cap==0 ringReadSessionID = %q want empty", got)
	}
}
