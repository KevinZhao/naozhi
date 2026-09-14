package cron

import (
	"reflect"
	"testing"
	"unsafe"
)

// TestRunCtx_NoPaddingFromMixedAlignment (R246-CR-014 / #757) keeps runCtx — the
// identity block preflightArgs and its siblings embed (Epic H #2546) — free of
// internal padding.
//
// Two corrections to what this test used to claim, both measured:
//
// It said it pinned "size-DESC field ordering", and that the pre-fix struct's
// "interleaved 8B pointers with 16B strings" cost ~16 bytes of padding.
// Reordering cannot cost anything here: every field of runCtx is 8-BYTE ALIGNED
// (jobSnapshot 216/8, time.Time 24/8, NotifyTarget 32/8, string 16/8, pointer
// 8/8), and Go never pads between fields of equal alignment regardless of size.
// Shuffling the declaration into the worst 8B/16B interleaving leaves sizeof
// unchanged. What this test actually catches is a field of DIFFERENT alignment
// being mixed in — inserting two bools fails it with excess=14 > tolerance=8,
// verified.
//
// And the sum is now taken over EVERY field by reflection. The previous version
// listed eight of nine fields and leaned on the one-word tolerance to absorb the
// ninth, so extracting the block (which added inflight) failed it for a reason
// unrelated to padding.
func TestRunCtx_NoPaddingFromMixedAlignment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		v    any
	}{
		{"runCtx", runCtx{}},
		{"preflightArgs", preflightArgs{}},
	} {
		typ := reflect.TypeOf(tc.v)
		var sum uintptr
		for i := range typ.NumField() {
			sum += typ.Field(i).Type.Size()
		}
		got := typ.Size()
		// Tolerance: one word for tail padding. With every field 8-aligned the
		// internal padding is zero, so any excess beyond a word means a field with a
		// smaller alignment was introduced and left a gap behind it.
		const wordTolerance = uintptr(8)
		if got > sum+wordTolerance {
			t.Errorf("%s sizeof=%d; sum-of-all-fields=%d (excess=%d > tolerance=%d). "+
				"A field whose alignment differs from the rest (bool, int32, ...) leaves "+
				"a gap; group such fields together at the end or widen them.",
				tc.name, got, sum, got-sum, wordTolerance)
		}
	}
	// Guard the guard: reflection must actually have seen fields, or the sums above
	// are trivially satisfied.
	if reflect.TypeOf(runCtx{}).NumField() < 8 {
		t.Fatalf("runCtx has %d fields; this test assumed the identity block, so it is no longer measuring it",
			reflect.TypeOf(runCtx{}).NumField())
	}
	// unsafe is still imported for the documented intent; assert the two agree.
	if unsafe.Sizeof(runCtx{}) != reflect.TypeOf(runCtx{}).Size() {
		t.Error("unsafe.Sizeof and reflect disagree on runCtx's size")
	}
}
