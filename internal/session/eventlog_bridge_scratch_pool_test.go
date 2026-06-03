package session

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/history/naozhilog"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// TestEventLogBridge_PooledScratch_MultiBatchPreservesOrderAndContent pins
// R20260602-PERF-2 (#1629): the multi-entry sink path now borrows its out /
// spans / times slices from batchScratchPool and resets them to [:0] on the
// next call. A reuse bug (stale length, leftover entries, or aliasing across
// calls) would corrupt the persisted batch. Drive several multi-entry
// batches of differing widths through ONE sink (so the scratch is recycled)
// and verify every entry persists exactly once, in order, with its own
// summary intact.
func TestEventLogBridge_PooledScratch_MultiBatchPreservesOrderAndContent(t *testing.T) {
	r, dir := newEventLogRouter(t, false)
	key := "scratch-pool-key"
	sink := newEventLogSink(r.eventLogPersister.SinkFor(key), nil, "")

	// Batches of widths 3, 1-via-multi (use 2 so we stay on the multi path),
	// then 5 — recycling the same scratch each time across changing widths.
	widths := []int{3, 2, 5, 4}
	var wantSummaries []string
	ts := int64(1)
	for _, w := range widths {
		batch := make([]cli.EventEntry, 0, w)
		for i := 0; i < w; i++ {
			sum := fmt.Sprintf("e-%d", ts)
			batch = append(batch, cli.EventEntry{
				UUID:    fmt.Sprintf("u-%d", ts),
				Time:    ts,
				Type:    "user",
				Summary: sum,
			})
			wantSummaries = append(wantSummaries, sum)
			ts++
		}
		sink(batch, false)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	testhelper.Eventually(t, func() bool {
		return r.eventLogPersister.Stats().Written >= 1
	}, time.Second, "persister never wrote")
	_ = r.eventLogPersister.Flush(ctx)

	src := naozhilog.New(dir, key)
	got, err := src.LoadLatest(context.Background(), 1000)
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if len(got) != len(wantSummaries) {
		t.Fatalf("persisted %d entries, want %d (pooled scratch must not drop/duplicate)", len(got), len(wantSummaries))
	}
	for i, e := range got {
		if e.Summary != wantSummaries[i] {
			t.Errorf("entry[%d] summary = %q, want %q (scratch reuse corrupted order/content)", i, e.Summary, wantSummaries[i])
		}
	}
}
