package persist

import "testing"

// R20260527122801-PERF-5 (#1336 partial): pins the raised default
// channel buffer at 4096. Prior 1024 was undersized for the 50+
// concurrent-session burst case (50 sessions × 50-event batches
// approached the cap and tripped droppedCnt). 4× headroom absorbs the
// burst without changing the singleton-drain architecture; the full
// fan-out fix proposed in #1336 is preserved as follow-up.
//
// Pin via constant comparison so a future "tune down to save memory"
// change has to delete the rationale block above to land — keeping
// the trade-off visible in code review rather than buried in commit
// archaeology.
func TestDefaultChannelBufferAbsorbsBurstSpike(t *testing.T) {
	const want = 4096
	if DefaultChannelBuffer != want {
		t.Fatalf("DefaultChannelBuffer = %d; want %d. Lowering this "+
			"re-opens the #1336 drop-under-burst regression — 50 sessions × "+
			"50-event AppendBatch fan-in saturates the prior 1024 cap and "+
			"the writer goroutine cannot drain fast enough to keep "+
			"droppedCnt at zero. Bumping is fine; lowering needs a "+
			"replacement architectural fix (the fan-out variant proposed "+
			"in #1336).", DefaultChannelBuffer, want)
	}
}
