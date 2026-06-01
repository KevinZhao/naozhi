package persist

import (
	"bufio"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// TestDrainInChannel_RefreshesClockMidBurst pins R20260531A-PERF-9
// (#1525): a long drain must NOT stamp every writer's lastActivity with
// the single pre-drain Clock() reading. If it did, a writer touched late
// in a 50+ batch burst would carry a stale lastActivity and tickIdleClose
// could close it prematurely. drainInChannel re-reads the clock every
// drainClockRefreshEvery batches; this test drives more than that many
// batches through a clock that advances on every read and asserts the
// writer touched last sees a fresher timestamp than the first.
//
// It builds a bare Persister (no run goroutine) and pre-seeds a stub
// writer wired to io.Discard so handleBatch's per-record write succeeds
// without touching the filesystem and without racing the run loop's
// tickers.
func TestDrainInChannel_RefreshesClockMidBurst(t *testing.T) {
	t.Parallel()

	base := time.Unix(1700000000, 0)
	var clockReads atomic.Int64
	p := &Persister{
		opts: Options{
			Dir:          t.TempDir(),
			IdxStride:    4,
			MaxFileBytes: 1 << 30, // avoid the post-batch rotate gate (FS ops)
			Observer:     noopObserver{},
			// Each read advances 1s so we can detect refreshes by the
			// timestamp delta on the writer's lastActivity.
			Clock: func() time.Time {
				n := clockReads.Add(1)
				return base.Add(time.Duration(n) * time.Second)
			},
		},
		in:      make(chan batchJob, 4096),
		writers: make(map[string]*perKeyWriter),
	}

	const key = "dashboard:direct:alice:general"
	w := &perKeyWriter{
		key:          key,
		stem:         "stub",
		logBuf:       bufio.NewWriter(io.Discard),
		lastActivity: base,
	}
	p.writers[key] = w

	// Enqueue more than one refresh window of batches.
	const batches = drainClockRefreshEvery*2 + 3
	for i := 0; i < batches; i++ {
		p.in <- batchJob{
			Key:     key,
			Stem:    "stub",
			Entries: []Entry{{JSON: []byte(`{"time":1,"uuid":"u","type":"user","summary":"x"}`), TimeMS: 1}},
		}
	}

	p.drainInChannel()

	// The clock must have been read at least twice (one per refresh window)
	// but far fewer than once-per-batch.
	reads := clockReads.Load()
	wantMinReads := int64(batches / drainClockRefreshEvery) // at least one per window
	if reads < wantMinReads {
		t.Errorf("clock read %d times, want >= %d (refresh-per-window)", reads, wantMinReads)
	}
	if reads >= int64(batches) {
		t.Errorf("clock read %d times for %d batches; refresh must be far cheaper than per-batch", reads, batches)
	}

	// The writer touched throughout the burst must end with a lastActivity
	// strictly later than the pre-drain instant — proving the refresh
	// advanced `now` rather than freezing it at the first reading.
	if !w.lastActivity.After(base.Add(time.Second)) {
		t.Errorf("writer lastActivity = %v did not advance past the first clock reading %v; clock was not refreshed mid-drain",
			w.lastActivity, base.Add(time.Second))
	}

	// lastDrainNS must reflect the final captured `now`.
	if got := p.lastDrainNS.Load(); got != w.lastActivity.UnixNano() {
		t.Errorf("lastDrainNS = %d, want %d (final captured now)", got, w.lastActivity.UnixNano())
	}
}

// TestDrainInChannel_SingleClockReadForSmallBurst pins the optimisation
// floor: a burst smaller than one refresh window must still read the
// clock exactly once (R216-PERF-7 / R222-PERF-12 preserved). #1525 only
// adds a staleness cap; it must not regress the common small-drain case
// into multiple vDSO calls.
func TestDrainInChannel_SingleClockReadForSmallBurst(t *testing.T) {
	t.Parallel()

	var clockReads atomic.Int64
	p := &Persister{
		opts: Options{
			Dir:          t.TempDir(),
			IdxStride:    4,
			MaxFileBytes: 1 << 30, // avoid the post-batch rotate gate (FS ops)
			Observer:     noopObserver{},
			Clock: func() time.Time {
				clockReads.Add(1)
				return time.Unix(1700000000, 0)
			},
		},
		in:      make(chan batchJob, 64),
		writers: make(map[string]*perKeyWriter),
	}

	const key = "dashboard:direct:bob:general"
	p.writers[key] = &perKeyWriter{key: key, stem: "stub", logBuf: bufio.NewWriter(io.Discard)}

	// Fewer than one refresh window.
	for i := 0; i < drainClockRefreshEvery-1; i++ {
		p.in <- batchJob{
			Key:     key,
			Stem:    "stub",
			Entries: []Entry{{JSON: []byte(`{"time":1,"uuid":"u","type":"user","summary":"x"}`), TimeMS: 1}},
		}
	}

	p.drainInChannel()

	if got := clockReads.Load(); got != 1 {
		t.Errorf("small burst read clock %d times, want exactly 1", got)
	}
}
