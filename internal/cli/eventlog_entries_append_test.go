package cli

// R249-PERF-18 (#937): EntriesSinceAppend / EntriesBeforeAppend pin tests.
// Cover three observable behaviours:
//   1. Equivalence with EntriesSince / EntriesBefore on same input.
//   2. Buffer reuse: a pre-grown dst is returned populated without alloc
//      (cap preserved).
//   3. Empty-match contract: dst==nil callers get nil; pool callers get
//      their buffer back length-zero so they can retain it.

import (
	"testing"
)

func TestEventLog_EntriesSinceAppend_EquivalentToEntriesSince(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	for i, ts := range []int64{1000, 1500, 2000, 2500, 3000} {
		l.Append(EventEntry{Time: ts, Type: "user", Summary: string(rune('a' + i))})
	}
	want := l.EntriesSince(1500)
	got := l.EntriesSinceAppend(nil, 1500)
	if len(want) != len(got) {
		t.Fatalf("len mismatch: want %d, got %d", len(want), len(got))
	}
	for i := range want {
		if want[i].Time != got[i].Time || want[i].Summary != got[i].Summary {
			t.Errorf("entry[%d] mismatch: want %+v got %+v", i, want[i], got[i])
		}
	}
}

func TestEventLog_EntriesSinceAppend_ReusesBuffer(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	for _, ts := range []int64{1000, 2000, 3000} {
		l.Append(EventEntry{Time: ts, Type: "text"})
	}
	// Pre-grown buffer with cap large enough for all matches; ensure
	// EntriesSinceAppend reuses it (pointer parity is the cleanest signal
	// that no fresh make() ran on the matched path).
	pool := make([]EventEntry, 0, 16)
	got := l.EntriesSinceAppend(pool, 0)
	if len(got) != 3 {
		t.Fatalf("want 3 entries, got %d", len(got))
	}
	if cap(got) != cap(pool) {
		t.Errorf("cap changed: got %d, want %d (buffer was reallocated — pool reuse broken)", cap(got), cap(pool))
	}
	if &got[:1][0] != &pool[:1][0] {
		t.Errorf("backing array swapped — pool reuse broken")
	}
}

func TestEventLog_EntriesSinceAppend_EmptyContractMatchesEntriesSince(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	l.Append(EventEntry{Time: 1000, Type: "user"})
	// No matches: afterMS far in the future.
	if got := l.EntriesSinceAppend(nil, 99999); got != nil {
		t.Errorf("dst==nil + no matches: got %v, want nil", got)
	}
	// Pool caller: buffer returned length-zero with cap preserved.
	pool := make([]EventEntry, 0, 4)
	got := l.EntriesSinceAppend(pool, 99999)
	if got == nil {
		t.Error("dst!=nil + no matches: got nil; pool caller expects len==0 buffer back")
	}
	if len(got) != 0 || cap(got) != 4 {
		t.Errorf("pool buffer not preserved: len=%d cap=%d", len(got), cap(got))
	}
}

func TestEventLog_EntriesSinceAppend_EmptyLogMatchesContract(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	if got := l.EntriesSinceAppend(nil, 0); got != nil {
		t.Errorf("empty log + dst==nil: got %v, want nil", got)
	}
	pool := make([]EventEntry, 0, 4)
	got := l.EntriesSinceAppend(pool, 0)
	if len(got) != 0 || cap(got) != 4 {
		t.Errorf("empty log + pool buffer: got len=%d cap=%d, want len=0 cap=4", len(got), cap(got))
	}
}

func TestEventLog_EntriesBeforeAppend_EquivalentToEntriesBefore(t *testing.T) {
	t.Parallel()
	l := NewEventLog(16)
	for i := 1; i <= 10; i++ {
		l.Append(EventEntry{Time: int64(i * 1000), Type: "text"})
	}
	want := l.EntriesBefore(5000, 3)
	got := l.EntriesBeforeAppend(nil, 5000, 3)
	if len(want) != len(got) {
		t.Fatalf("len mismatch: want %d, got %d", len(want), len(got))
	}
	for i := range want {
		if want[i].Time != got[i].Time {
			t.Errorf("entry[%d] mismatch: want %d got %d", i, want[i].Time, got[i].Time)
		}
	}
}

func TestEventLog_EntriesBeforeAppend_ReusesBuffer(t *testing.T) {
	t.Parallel()
	l := NewEventLog(16)
	for i := 1; i <= 10; i++ {
		l.Append(EventEntry{Time: int64(i * 1000), Type: "text"})
	}
	pool := make([]EventEntry, 0, 8)
	got := l.EntriesBeforeAppend(pool, 11000, 5)
	if len(got) != 5 {
		t.Fatalf("want 5 entries, got %d", len(got))
	}
	if cap(got) != cap(pool) {
		t.Errorf("cap changed: got %d, want %d (buffer was reallocated)", cap(got), cap(pool))
	}
	if &got[:1][0] != &pool[:1][0] {
		t.Errorf("backing array swapped — pool reuse broken")
	}
}

func TestEventLog_EntriesBeforeAppend_LimitZeroContract(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	l.Append(EventEntry{Time: 1000, Type: "text"})
	if got := l.EntriesBeforeAppend(nil, 2000, 0); got != nil {
		t.Errorf("limit==0 + dst==nil: got %v, want nil (matches EntriesBefore)", got)
	}
	if got := l.EntriesBeforeAppend(nil, 2000, -3); got != nil {
		t.Errorf("limit<0 + dst==nil: got %v, want nil", got)
	}
	// Pool caller still gets buffer back length-zero so it can be retained.
	pool := make([]EventEntry, 0, 4)
	got := l.EntriesBeforeAppend(pool, 2000, 0)
	if len(got) != 0 || cap(got) != 4 {
		t.Errorf("limit==0 + pool: got len=%d cap=%d, want len=0 cap=4", len(got), cap(got))
	}
}

func TestEventLog_EntriesBeforeAppend_NoMatch(t *testing.T) {
	t.Parallel()
	l := NewEventLog(8)
	l.Append(EventEntry{Time: 5000, Type: "text"})
	// All entries have Time >= beforeMS — no match.
	if got := l.EntriesBeforeAppend(nil, 1000, 5); got != nil {
		t.Errorf("no match + dst==nil: got %v, want nil", got)
	}
	pool := make([]EventEntry, 0, 4)
	got := l.EntriesBeforeAppend(pool, 1000, 5)
	if len(got) != 0 || cap(got) != 4 {
		t.Errorf("no match + pool: got len=%d cap=%d, want len=0 cap=4", len(got), cap(got))
	}
}
