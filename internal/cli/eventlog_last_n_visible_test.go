package cli

import "testing"

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
