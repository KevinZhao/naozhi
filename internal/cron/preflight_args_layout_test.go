package cron

import (
	"testing"
	"unsafe"
)

// TestPreflightArgs_FieldOrderingPaddingFloor (R246-CR-014 / #757) pins the
// size-DESC field ordering documented on preflightArgs. Before the fix
// the struct interleaved 8B pointers with 16B strings and the compiler
// inserted ~16 bytes of padding per call. Layout is intentionally
// minimal: lead with the largest field (snap) so no head padding is
// needed, group equally-sized fields together (string-typed = 16B,
// pointer = 8B), and trail with the 8B pointers.
//
// We assert the struct's size is no larger than the sum of its field
// sizes plus a one-word padding tolerance — anything bigger means a
// future field reorder slipped padding back in.
func TestPreflightArgs_FieldOrderingPaddingFloor(t *testing.T) {
	a := preflightArgs{}
	got := unsafe.Sizeof(a)

	want := unsafe.Sizeof(a.snap) +
		unsafe.Sizeof(a.startedAt) +
		unsafe.Sizeof(a.notifyTo) +
		unsafe.Sizeof(a.key) +
		unsafe.Sizeof(a.runID) +
		unsafe.Sizeof(a.trigger) +
		unsafe.Sizeof(a.job) +
		unsafe.Sizeof(a.lg)

	// Tolerance: one word for tail padding to align the surrounding
	// caller's stack frame. Pre-fix value exceeded the sum by ~16 bytes
	// (two padding gaps); the size-DESC ordering keeps internal padding
	// at zero on both amd64 and arm64.
	const wordTolerance = uintptr(8)
	if got > want+wordTolerance {
		t.Fatalf("preflightArgs sizeof=%d; sum-of-fields=%d (excess=%d > tolerance=%d). "+
			"Field ordering must stay size DESC — see preflightArgs godoc.",
			got, want, got-want, wordTolerance)
	}
}
