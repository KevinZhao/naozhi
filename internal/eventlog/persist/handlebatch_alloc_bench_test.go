package persist

import (
	"bufio"
	"io"
	"testing"
	"time"
)

// BenchmarkHandleBatch_RecordAlloc pins R20260614-PERF-1 (#2088): handleBatch
// must NOT heap-allocate one *Record per entry. The pre-fix code called
// schema.NewEntry(seq, json) inside the per-entry loop, returning a
// heap-allocated *Record every iteration (50 events/s × N sessions of steady
// allocator pressure). The fix stack-declares a single schema.Record once per
// batch and reuses it, so per-entry allocs drop to zero on this path.
//
// It builds a bare Persister with a stub writer wired to io.Discard (mirrors
// TestDrainInChannel_RefreshesClockMidBurst) so the benchmark measures only
// the marshal/record path without filesystem or run-loop noise.
func BenchmarkHandleBatch_RecordAlloc(b *testing.B) {
	const key = "dashboard:direct:alice:general"
	p := &Persister{
		opts: Options{
			Dir:          b.TempDir(),
			IdxStride:    4,
			MaxFileBytes: 1 << 30, // avoid the post-batch rotate gate (FS ops)
			Observer:     noopObserver{},
			Clock:        func() time.Time { return time.Unix(1700000000, 0) },
		},
		in:      make(chan batchJob, 4),
		writers: make(map[string]*perKeyWriter),
	}
	p.writers[key] = &perKeyWriter{
		key:    key,
		stem:   "stub",
		logBuf: bufio.NewWriter(io.Discard),
	}

	// A representative batch of entries; the pre-fix path allocated one
	// *Record per entry here.
	const entriesPerBatch = 16
	entries := make([]Entry, entriesPerBatch)
	for i := range entries {
		entries[i] = Entry{
			JSON:   []byte(`{"time":1,"uuid":"u","type":"user","summary":"hello world"}`),
			TimeMS: 1,
		}
	}
	now := time.Unix(1700000000, 0)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Fresh slice header each iter; arena is nil (owned-bytes path is
		// nil-safe per putEntryArena), so no pool churn skews the count.
		p.handleBatch(batchJob{Key: key, Stem: "stub", Entries: entries}, now)
	}
}
