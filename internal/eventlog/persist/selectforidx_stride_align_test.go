package persist

import (
	"testing"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// TestSelectForIdx_StrideAlignmentAcrossBatches pins R20260608-LB-6 (#1948):
// the entriesSinceIdxWrite cursor records the absolute-stream phase of
// pendingIdx[0]. flush() reads it as selectForIdx's start phase and then
// advances it by len(pendingIdx) modulo stride after a durable idx sync.
//
// The invariant the godoc claims (persister.go: "successive batches stay
// aligned"): the stride-aligned entries kept across consecutive batches must
// land on the GLOBAL absolute stream positions that are multiples of stride —
// independent of where batch boundaries happen to fall.
//
// The pre-fix bug advanced the cursor twice per cycle: handleBatch did
// entriesSinceIdxWrite++ per entry (cursor = C+N at flush) and flush then set
// it to (C+N+len(pendingIdx)) % stride = (C+2N) % stride. That both offset the
// selectForIdx start phase by N and advanced the cursor by 2N per cycle, so the
// stride-aligned keeps drifted off the true stream phase batch after batch.
//
// This test simulates the exact cursor lifecycle flush() uses (start = cursor,
// then cursor = (cursor + len(batch)) % stride) and asserts every entry kept by
// the stride rule sits on a global stride boundary, and conversely every global
// stride boundary entry is kept.
func TestSelectForIdx_StrideAlignmentAcrossBatches(t *testing.T) {
	const stride = 4

	// Non-uniform batch sizes so boundaries do NOT line up with the stride —
	// this is what exposes phase drift. Total entries: 3+5+2+6+4 = 20.
	batches := [][]uint64{
		seqRange(0, 3),   // header (seq 0) + seq 1,2  → abs pos 0,1,2
		seqRange(3, 8),   // seq 3..7                  → abs pos 3..7
		seqRange(8, 10),  // seq 8,9                   → abs pos 8,9
		seqRange(10, 16), // seq 10..15                → abs pos 10..15
		seqRange(16, 20), // seq 16..19                → abs pos 16..19
	}

	cursor := 0
	keptStrideAligned := map[uint64]bool{}
	for _, b := range batches {
		pending := makeIdxEntries(b)
		// selectForIdx start phase == cursor (NOT cursor+len(batch)).
		kept := selectForIdx(pending, stride, cursor, nil)
		for i, e := range pending {
			// Record only the entries kept by the stride rule (exclude the
			// always-kept header seq==0 and always-kept last-of-batch, which
			// are kept for recovery edges, not stride alignment).
			if e.Seq != 0 && (cursor+i)%stride == 0 {
				if !containsSeq(kept, e.Seq) {
					t.Fatalf("seq %d satisfied stride rule but was not kept", e.Seq)
				}
				keptStrideAligned[e.Seq] = true
			}
		}
		// Advance cursor exactly as flush() does after a durable sync.
		cursor = (cursor + len(pending)) % stride
	}

	// The set of stride-kept seqs must equal the global stride boundaries:
	// abs position p (== seq here, since seq is the 0-based stream index)
	// with p%stride==0 and p!=0 (header excluded above).
	for seq := uint64(0); seq < 20; seq++ {
		wantKept := seq != 0 && seq%stride == 0
		if got := keptStrideAligned[seq]; got != wantKept {
			t.Fatalf("seq %d: stride-aligned kept=%v, want %v (global phase drift — cursor not aligned)",
				seq, got, wantKept)
		}
	}
}

func seqRange(lo, hi uint64) []uint64 {
	out := make([]uint64, 0, hi-lo)
	for s := lo; s < hi; s++ {
		out = append(out, s)
	}
	return out
}

func makeIdxEntries(seqs []uint64) []schema.IdxEntry {
	out := make([]schema.IdxEntry, len(seqs))
	for i, s := range seqs {
		out[i] = schema.IdxEntry{
			Seq:     s,
			ByteOff: int64(s) * 100,
			Len:     128,
			TimeMS:  int64(s) * 1000,
		}
	}
	return out
}

func containsSeq(entries []schema.IdxEntry, seq uint64) bool {
	for _, e := range entries {
		if e.Seq == seq {
			return true
		}
	}
	return false
}
