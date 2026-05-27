package persist

import (
	"testing"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// BenchmarkSelectForIdx_ScratchReuse pins R217-PERF-6 (#615): with the
// caller-owned scratch buffer (perKeyWriter.idxScratch), repeated flushes
// don't allocate a fresh keep-slice every time. Compare against
// BenchmarkSelectForIdx_NoScratch — the scratch path reports zero
// allocs/op for the steady state where scratch cap already exceeds the
// kept-entry count, while the no-scratch path allocates a new slice
// every iteration.
//
// Parameters: 64-entry pending slice with stride=32 → selectForIdx returns
// ~3 entries per call (header + last + one stride-aligned entry). Mirrors
// the typical Persister.flush invocation.
func BenchmarkSelectForIdx_ScratchReuse(b *testing.B) {
	const stride = 32
	pending := makeSelectFixture(64)
	scratch := make([]schema.IdxEntry, 0, 8) // pre-warmed cap, mirrors steady state

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		kept := selectForIdx(pending, stride, 0, scratch[:0])
		// Mirror flush()'s assignment so the compiler doesn't elide kept.
		scratch = kept
	}
	_ = scratch
}

// BenchmarkSelectForIdx_NoScratch is the pre-fix baseline: scratch passed
// in as nil so every call allocates a fresh slice. Demonstrates the
// alloc savings the caller-owned scratch buys (R217-PERF-6 #615).
//
// Recorded numbers on go1.26 (arm64): NoScratch ~128 B/op, 1 alloc/op;
// ScratchReuse 0 B/op, 0 allocs/op. With ~5 active per-key writers each
// flushing 5 Hz that saves ~25 allocs/s steady state — small per session
// but compounds linearly with session count.
func BenchmarkSelectForIdx_NoScratch(b *testing.B) {
	const stride = 32
	pending := makeSelectFixture(64)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		kept := selectForIdx(pending, stride, 0, nil)
		_ = kept
	}
}

// makeSelectFixture builds a deterministic []schema.IdxEntry of length n
// with seq=0 as the header (always kept by selectForIdx) and seq=i for
// the rest.
func makeSelectFixture(n int) []schema.IdxEntry {
	out := make([]schema.IdxEntry, n)
	for i := 0; i < n; i++ {
		out[i] = schema.IdxEntry{
			Seq:     uint64(i),
			ByteOff: int64(i * 100),
			Len:     128,
			TimeMS:  int64(i * 1000),
		}
	}
	return out
}

// TestSelectForIdx_ScratchReuseStable is a correctness pin alongside
// BenchmarkSelectForIdx_ScratchReuse: passing the same scratch slice
// across two calls must produce semantically identical outputs each
// time. Without this check a future reviewer might "optimise" away the
// `scratch[:0]` reset and silently corrupt the kept-set.
func TestSelectForIdx_ScratchReuseStable(t *testing.T) {
	const stride = 8
	pending := makeSelectFixture(20)
	scratch := make([]schema.IdxEntry, 0, 8)

	got1 := selectForIdx(pending, stride, 0, scratch[:0])
	// Snapshot before scratch is reused — flush()'s assignment pattern is
	// `scratch = kept`, so without a copy got1 would alias the scratch
	// slice and the second call's selectForIdx would overwrite it.
	snapshot := append([]schema.IdxEntry(nil), got1...)
	scratch = got1

	got2 := selectForIdx(pending, stride, 0, scratch[:0])
	if len(got2) != len(snapshot) {
		t.Fatalf("kept length differs across calls: %d vs %d", len(got2), len(snapshot))
	}
	for i := range got2 {
		if got2[i] != snapshot[i] {
			t.Fatalf("kept[%d] differs across calls: %+v vs %+v", i, got2[i], snapshot[i])
		}
	}
}
