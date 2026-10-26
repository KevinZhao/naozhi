package ring

import (
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestEventLog_EntriesBefore_NonMonotonicTimeFiltersCorrectly pins the
// R040034-CHANGES (#1383 review) regression-lock contract: entries are
// stored in INSERTION order but Time field is caller-supplied, so a
// late-arriving high-Time entry can sit AFTER an earlier low-Time entry
// in the ring. The previous "crossed" fast-path assumed the first
// sub-beforeMS entry meant all earlier entries also satisfied
// Time < beforeMS — switching to "collect greedily" without per-entry
// filter. Under non-monotonic Time that lets a Time-too-large entry
// past the filter.
//
// Repro setup mirrors the documented breakage: append entries with
// non-monotonic Time, then EntriesBefore(beforeMS) targeting a value
// that bisects the sequence. A correct implementation MUST exclude
// every entry whose Time >= beforeMS regardless of position.
func TestEventLog_EntriesBefore_NonMonotonicTimeFiltersCorrectly(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)

	// Insertion order: 200, 150, 300, 100. EntriesBefore(beforeMS=180)
	// must return only Time<180 entries: {150, 100}. The pre-fix fast
	// path would emit {150} (sees first <180), then "collect greedily"
	// the rest of the ring including 200 and 300 — a false positive.
	l.Append(clievent.EventEntry{Time: 200, Type: "text", Summary: "t200"})
	l.Append(clievent.EventEntry{Time: 150, Type: "text", Summary: "t150"})
	l.Append(clievent.EventEntry{Time: 300, Type: "text", Summary: "t300"})
	l.Append(clievent.EventEntry{Time: 100, Type: "text", Summary: "t100"})

	got := l.EntriesBefore(180, 10)
	if len(got) != 2 {
		t.Fatalf("EntriesBefore(180): got %d entries, want 2 (Time<180 only); entries=%+v", len(got), got)
	}
	for _, e := range got {
		if e.Time >= 180 {
			t.Errorf("EntriesBefore(180) returned entry with Time=%d (Summary=%q) — must filter out Time>=180 regardless of insertion order",
				e.Time, e.Summary)
		}
	}

	// Reverse-order ring: every Time-decreasing sequence is still common
	// enough (e.g. test fixtures, replay merging two streams) that it
	// gets its own assertion. Pre-fix code hit the fast-path immediately
	// at idx[count-1] (first entry seen has Time=400<beforeMS=500),
	// then "collected greedily" 600 ahead of it — a contract violation.
	l2 := NewEventLog(8)
	l2.Append(clievent.EventEntry{Time: 600, Type: "text"})
	l2.Append(clievent.EventEntry{Time: 400, Type: "text"})
	got2 := l2.EntriesBefore(500, 10)
	if len(got2) != 1 {
		t.Fatalf("reverse-order ring EntriesBefore(500): got %d entries, want 1; entries=%+v", len(got2), got2)
	}
	if got2[0].Time != 400 {
		t.Errorf("reverse-order ring EntriesBefore(500): got Time=%d, want 400", got2[0].Time)
	}
}

// TestEventLog_EntriesBeforeAppend_NonMonotonicTimeFiltersCorrectly mirrors
// the EntriesBefore test on the buffer-reusing variant — both go through
// EntriesBeforeAppend internally, but the contract is exposed at both
// surface levels and the test makes that explicit.
func TestEventLog_EntriesBeforeAppend_NonMonotonicTimeFiltersCorrectly(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	l.Append(clievent.EventEntry{Time: 200, Type: "text"})
	l.Append(clievent.EventEntry{Time: 150, Type: "text"})
	l.Append(clievent.EventEntry{Time: 300, Type: "text"})
	l.Append(clievent.EventEntry{Time: 100, Type: "text"})

	pool := make([]clievent.EventEntry, 0, 8)
	got := l.EntriesBeforeAppend(pool, 180, 10)
	if len(got) != 2 {
		t.Fatalf("EntriesBeforeAppend(180): got %d entries, want 2", len(got))
	}
	for _, e := range got {
		if e.Time >= 180 {
			t.Errorf("EntriesBeforeAppend(180): returned Time=%d, must filter out Time>=180", e.Time)
		}
	}
}
