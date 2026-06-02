package cli

import (
	"testing"
	"unsafe"
)

// countVisible counts entries the dashboard would render (non-internal).
func countVisible(entries []EventEntry) int {
	n := 0
	for i := range entries {
		if IsVisibleEntry(entries[i]) {
			n++
		}
	}
	return n
}

func TestIsInternalEventType(t *testing.T) {
	t.Parallel()
	internal := []string{"tool_use", "result", "agent", "task_start", "task_progress", "task_done"}
	for _, ty := range internal {
		if !IsInternalEventType(ty) {
			t.Errorf("IsInternalEventType(%q) = false, want true", ty)
		}
		if IsVisibleEntry(EventEntry{Type: ty}) {
			t.Errorf("IsVisibleEntry(%q) = true, want false", ty)
		}
	}
	visible := []string{"user", "text", "thinking", "init", "system", "todo", "ask_question"}
	for _, ty := range visible {
		if IsInternalEventType(ty) {
			t.Errorf("IsInternalEventType(%q) = true, want false", ty)
		}
		if !IsVisibleEntry(EventEntry{Type: ty}) {
			t.Errorf("IsVisibleEntry(%q) = false, want true", ty)
		}
	}
}

func TestLastNVisible_EmptyRing(t *testing.T) {
	t.Parallel()
	l := NewEventLog(10)
	if got := l.LastNVisible(5, 10); got != nil {
		t.Errorf("LastNVisible on empty ring = %v, want nil", got)
	}
}

func TestLastNVisible_VisibleSufficient(t *testing.T) {
	t.Parallel()
	l := NewEventLog(100)
	for i := 0; i < 10; i++ {
		l.Append(EventEntry{Time: int64(i + 1), Type: "text", Summary: "msg"})
	}
	got := l.LastNVisible(3, 100)
	// Stops as soon as 3 visible entries are collected, walking from newest.
	if countVisible(got) != 3 {
		t.Errorf("visible count = %d, want exactly 3", countVisible(got))
	}
	// Chronological order preserved.
	for i := 1; i < len(got); i++ {
		if got[i].Time < got[i-1].Time {
			t.Errorf("not chronological at %d: %d < %d", i, got[i].Time, got[i-1].Time)
		}
	}
	// The 3 visible should be the NEWEST three (times 8,9,10).
	if got[len(got)-1].Time != 10 {
		t.Errorf("newest entry Time = %d, want 10", got[len(got)-1].Time)
	}
}

// TestLastNVisible_SparseVisible reproduces the bug shape: a long run of
// internal events with rare visible bubbles. The reader must keep walking
// past the internal flood to surface the requested visible count.
func TestLastNVisible_SparseVisible(t *testing.T) {
	t.Parallel()
	l := NewEventLog(500)
	tm := int64(0)
	// 3 visible messages, each followed by 50 internal events.
	for m := 0; m < 3; m++ {
		tm++
		l.Append(EventEntry{Time: tm, Type: "text", Summary: "real message"})
		for i := 0; i < 50; i++ {
			tm++
			l.Append(EventEntry{Time: tm, Type: "task_progress", Summary: "agent working"})
		}
	}
	// Ask for 2 visible with a generous total ceiling.
	got := l.LastNVisible(2, 500)
	if countVisible(got) < 2 {
		t.Errorf("visible count = %d, want >= 2 (reader must walk past internal flood)", countVisible(got))
	}
}

// TestLastNVisible_AllInternal is the exact screenshot scenario: the trailing
// window is entirely internal events. The reader returns the contiguous tail
// (so turnState can rebuild) but the caller will see zero visible and fall
// through to the disk tier.
func TestLastNVisible_AllInternal(t *testing.T) {
	t.Parallel()
	l := NewEventLog(200)
	for i := 0; i < 200; i++ {
		l.Append(EventEntry{Time: int64(i + 1), Type: "tool_use", Summary: "Grep"})
	}
	got := l.LastNVisible(30, 100)
	if countVisible(got) != 0 {
		t.Errorf("visible count = %d, want 0 (ring is all internal)", countVisible(got))
	}
	// maxTotal caps the slice length even when the visible target is unmet.
	if len(got) > 100 {
		t.Errorf("len = %d, want <= maxTotal 100", len(got))
	}
}

// TestLastNVisible_MaxTotalCap ensures the cost ceiling halts the walk.
func TestLastNVisible_MaxTotalCap(t *testing.T) {
	t.Parallel()
	l := NewEventLog(500)
	for i := 0; i < 500; i++ {
		l.Append(EventEntry{Time: int64(i + 1), Type: "task_progress"})
	}
	got := l.LastNVisible(30, 50)
	if len(got) != 50 {
		t.Errorf("len = %d, want exactly 50 (maxTotal cap)", len(got))
	}
}

// TestLastNVisible_ZeroTargetFallsBackToLastN verifies the no-accounting path.
func TestLastNVisible_ZeroTargetFallsBackToLastN(t *testing.T) {
	t.Parallel()
	l := NewEventLog(100)
	for i := 0; i < 20; i++ {
		l.Append(EventEntry{Time: int64(i + 1), Type: "text"})
	}
	got := l.LastNVisible(0, 10)
	if len(got) != 10 {
		t.Errorf("len = %d, want 10 (maxTotal tail, no visible accounting)", len(got))
	}
	if got[len(got)-1].Time != 20 {
		t.Errorf("newest Time = %d, want 20", got[len(got)-1].Time)
	}
}

// sliceData returns the backing-array address of a slice for identity
// comparison in the buffer-reuse test (R20260602-PERF-8, #1631).
func sliceData(s []EventEntry) uintptr {
	if cap(s) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&s[:1][0]))
}

// TestLastNVisibleAppend_ReusesBuffer is the regression test for
// R20260602-PERF-8 (#1631): a pooled caller passing a pre-grown dst[:0]
// must get its own backing array back, with no fresh allocation on the
// dashboard first-render path.
func TestLastNVisibleAppend_ReusesBuffer(t *testing.T) {
	t.Parallel()
	l := NewEventLog(500)
	// All-internal entries so the maxTotal=200 cap trips (not visibleTarget).
	for i := 0; i < 300; i++ {
		l.Append(EventEntry{Time: int64(i + 1), Type: "task_progress"})
	}
	// Pre-grown buffer with capacity >= maxTotal so no realloc is needed.
	buf := make([]EventEntry, 0, 200)
	wantData := sliceData(buf)

	got := l.LastNVisibleAppend(buf[:0], 50, 200)
	if len(got) != 200 {
		t.Fatalf("len = %d, want 200 (maxTotal cap)", len(got))
	}
	if sliceData(got) != wantData {
		t.Errorf("LastNVisibleAppend allocated a new backing array; want the caller's pooled buffer reused")
	}
	if got[len(got)-1].Time != 300 {
		t.Errorf("newest Time = %d, want 300", got[len(got)-1].Time)
	}
}

// TestLastNVisibleAppend_GrowsWhenTooSmall verifies a short pooled buffer
// is grown (not silently truncated): the result must still carry the full
// requested tail even when cap(dst) < maxTotal.
func TestLastNVisibleAppend_GrowsWhenTooSmall(t *testing.T) {
	t.Parallel()
	l := NewEventLog(500)
	for i := 0; i < 300; i++ {
		l.Append(EventEntry{Time: int64(i + 1), Type: "task_progress"})
	}
	got := l.LastNVisibleAppend(make([]EventEntry, 0, 4), 50, 200)
	if len(got) != 200 {
		t.Fatalf("len = %d, want 200 (buffer must grow, not truncate)", len(got))
	}
	if got[len(got)-1].Time != 300 {
		t.Errorf("newest Time = %d, want 300", got[len(got)-1].Time)
	}
}

// TestLastNVisibleAppend_EmptyRingContract checks the nil-vs-dst[:0] return
// contract on an empty ring matches the sibling Append methods.
func TestLastNVisibleAppend_EmptyRingContract(t *testing.T) {
	t.Parallel()
	l := NewEventLog(10)
	if got := l.LastNVisibleAppend(nil, 5, 10); got != nil {
		t.Errorf("nil dst on empty ring = %v, want nil", got)
	}
	buf := make([]EventEntry, 0, 8)
	got := l.LastNVisibleAppend(buf[:0], 5, 10)
	if got == nil {
		t.Errorf("dst[:0] on empty ring = nil, want length-zero buffer (caller retains it)")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0 on empty ring", len(got))
	}
}

// TestLastNVisibleAppend_MatchesLastNVisible asserts the Append variant is
// behaviourally identical to LastNVisible (which now delegates to it).
func TestLastNVisibleAppend_MatchesLastNVisible(t *testing.T) {
	t.Parallel()
	l := NewEventLog(500)
	for i := 0; i < 250; i++ {
		ty := "task_progress"
		if i%7 == 0 {
			ty = "text"
		}
		l.Append(EventEntry{Time: int64(i + 1), Type: ty})
	}
	want := l.LastNVisible(30, 100)
	got := l.LastNVisibleAppend(make([]EventEntry, 0, 100), 30, 100)
	if len(got) != len(want) {
		t.Fatalf("len mismatch: append=%d direct=%d", len(got), len(want))
	}
	for i := range want {
		if got[i].Time != want[i].Time || got[i].Type != want[i].Type {
			t.Errorf("entry %d mismatch: append={t:%d ty:%s} direct={t:%d ty:%s}",
				i, got[i].Time, got[i].Type, want[i].Time, want[i].Type)
		}
	}
}
