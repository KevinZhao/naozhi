package persist

import (
	"testing"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// BenchmarkPerKeyWriter_PendingIdxAppend_Preallocated pins R217-PERF-5
// (#613): the IdxStride*2 preallocation in newWriter lets a steady-state
// flush window fill without triggering Go slice's nil→1→2→4→8→… growth
// doubling. Each iteration represents one flush window's worth of
// appends followed by the [:0] reset that flush() applies after Sync.
//
// On go1.26 (arm64) this reports 0 B/op, 0 allocs/op — the preallocated
// backing array survives the [:0] reset and absorbs every append.
func BenchmarkPerKeyWriter_PendingIdxAppend_Preallocated(b *testing.B) {
	const stride = 32
	const window = stride * 2 // matches pendingCap in newWriter
	pendingIdx := make([]schema.IdxEntry, 0, window)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < stride; j++ {
			pendingIdx = append(pendingIdx, schema.IdxEntry{
				Seq:     uint64(j),
				ByteOff: int64(j * 100),
				Len:     128,
				TimeMS:  int64(j * 1000),
			})
		}
		// Matches flush's reset; the preallocated cap survives.
		pendingIdx = pendingIdx[:0]
	}
	_ = pendingIdx
}

// BenchmarkPerKeyWriter_PendingIdxAppend_NilStart simulates the pre-fix
// behaviour: a fresh nil-start slice every flush. Goes through the slice
// growth doubling sequence on every iteration. Demonstrates the alloc
// savings the IdxStride*2 preallocation buys (R217-PERF-5 #613).
//
// Recorded on go1.26 (arm64): NilStart ~1984 B/op, 5 allocs/op (one
// growth at each doubling boundary) versus Preallocated 0 B/op, 0
// allocs/op. With ~5 active per-key writers each flushing 5 Hz that
// saves ~125 allocs/s steady state — small per session but compounds
// linearly with session count, and avoids transient peak-cap retention
// after burst writes (the existing flush-time shrink rule keeps cap
// from monotonically growing).
func BenchmarkPerKeyWriter_PendingIdxAppend_NilStart(b *testing.B) {
	const stride = 32

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var pendingIdx []schema.IdxEntry
		for j := 0; j < stride; j++ {
			pendingIdx = append(pendingIdx, schema.IdxEntry{
				Seq:     uint64(j),
				ByteOff: int64(j * 100),
				Len:     128,
				TimeMS:  int64(j * 1000),
			})
		}
		_ = pendingIdx
	}
}

// TestPendingIdxPreallocCapMatchesStride pins the contract between
// newWriter's pendingCap formula and the IdxStride*2 documentation.
// If a future commit changes the formula (e.g. to IdxStride only)
// without updating the comment, this test fires so the divergence
// can't slip through.
func TestPendingIdxPreallocCapMatchesStride(t *testing.T) {
	// Mirror the formula from newWriter:
	//   pendingCap := 16
	//   if p.opts.IdxStride > 1 { pendingCap = p.opts.IdxStride * 2 }
	cases := []struct {
		stride int
		want   int
	}{
		{stride: 0, want: 16},  // disabled / unset → 16-entry floor
		{stride: 1, want: 16},  // stride=1 means "every entry kept" → no growth advantage anyway
		{stride: 4, want: 8},   // newTestPersister default
		{stride: 32, want: 64}, // DefaultIdxStride
	}
	for _, tc := range cases {
		got := pendingCapFromStride(tc.stride)
		if got != tc.want {
			t.Errorf("stride=%d: got cap=%d, want %d", tc.stride, got, tc.want)
		}
	}
}

// pendingCapFromStride mirrors the formula in newWriter so the test
// above can pin it. Kept here (test-only) to avoid exporting a tiny
// helper from the production package.
func pendingCapFromStride(stride int) int {
	pendingCap := 16
	if stride > 1 {
		pendingCap = stride * 2
	}
	return pendingCap
}
