package merged

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// countingSource is a history.Source that records every LoadBefore call.
type countingSource struct {
	entries []clievent.EventEntry
	err     error
	calls   int
	before  int64
	limit   int
}

func (s *countingSource) LoadBefore(_ context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error) {
	s.calls++
	s.before, s.limit = beforeMS, limit
	return s.entries, s.err
}

func gapRec(t int64) clievent.EventEntry {
	return clievent.EventEntry{Time: t, Type: schema.GapEntryType, Detail: "dropped=3 reason=persist_channel_full"}
}

func uuids(es []clievent.EventEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.UUID)
	}
	return out
}

// TestGapFill_ReturnsDroppedTurnsOldestFirst: local lost q2..q4 to a dropped
// batch and carries a gap record at the next stored batch. The fallback's q2..q4
// come back oldest first; q1 and q5 are twins of local rows and stay out. The
// fallback is read once, newest-first unbounded (before 0) with the caller's
// limit, and the slice it handed back is not reordered in place.
func TestGapFill_ReturnsDroppedTurnsOldestFirst(t *testing.T) {
	local := []clievent.EventEntry{
		{UUID: "l1", Time: 1000, Type: "user", Detail: "q1"},
		{UUID: "l1t", Time: 1200, Type: "tool_use", Detail: "Bash ls"},
		gapRec(5000),
		{UUID: "l5", Time: 5000, Type: "user", Detail: "q5"},
	}
	fbEntries := []clievent.EventEntry{
		{UUID: "c5", Time: 5005, Type: "user", Detail: "q5"},
		{UUID: "c4", Time: 4005, Type: "user", Detail: "q4"},
		{UUID: "c3", Time: 3005, Type: "user", Detail: "q3"},
		{UUID: "c2", Time: 2005, Type: "user", Detail: "q2"},
		{UUID: "c1", Time: 1005, Type: "user", Detail: "q1"},
	}
	orig := slices.Clone(fbEntries)
	fb := &countingSource{entries: fbEntries}
	got := (&Source{Fallback: fb}).GapFill(context.Background(), local, 0, 77)
	if want := []string{"c2", "c3", "c4"}; !slices.Equal(uuids(got), want) {
		t.Fatalf("GapFill = %v, want %v", uuids(got), want)
	}
	if fb.calls != 1 || fb.before != 0 || fb.limit != 77 {
		t.Errorf("fallback read calls=%d before=%d limit=%d, want 1 / 0 / 77", fb.calls, fb.before, fb.limit)
	}
	if !slices.EqualFunc(fbEntries, orig, func(a, b clievent.EventEntry) bool { return a.UUID == b.UUID }) {
		t.Errorf("GapFill reordered the fallback's slice in place: %v", uuids(fbEntries))
	}
}

// TestGapFill_WindowEdges pins both window bounds, inclusive: from the previous
// local entry minus contentSkewLagMS to the gap record plus contentSkewLeadMS.
// One millisecond outside either edge is not filled.
func TestGapFill_WindowEdges(t *testing.T) {
	const prev, gap = 10_000, 20_000
	local := []clievent.EventEntry{
		{UUID: "l0", Time: prev, Type: "text", Detail: "before the gap"},
		gapRec(gap),
		{UUID: "l9", Time: gap, Type: "text", Detail: "after the gap"},
	}
	lo, hi := int64(prev-contentSkewLagMS), int64(gap+contentSkewLeadMS)
	fb := &countingSource{entries: []clievent.EventEntry{
		{UUID: "below", Time: lo - 1, Type: "text", Detail: "a"},
		{UUID: "lo", Time: lo, Type: "text", Detail: "b"},
		{UUID: "mid", Time: 15_000, Type: "text", Detail: "c"},
		{UUID: "hi", Time: hi, Type: "text", Detail: "d"},
		{UUID: "above", Time: hi + 1, Type: "text", Detail: "e"},
	}}
	got := (&Source{Fallback: fb}).GapFill(context.Background(), local, 0, 100)
	if want := []string{"lo", "mid", "hi"}; !slices.Equal(uuids(got), want) {
		t.Fatalf("GapFill = %v, want %v (window [%d, %d])", uuids(got), want, lo, hi)
	}
}

// TestGapFill_RecordWithNothingBeforeIt: a gap record that is local's first
// entry has no lower bound; the gap's window is not taken from a later entry.
func TestGapFill_RecordWithNothingBeforeIt(t *testing.T) {
	local := []clievent.EventEntry{
		gapRec(50_000),
		{UUID: "l1", Time: 50_000, Type: "user", Detail: "q"},
		{UUID: "l2", Time: 90_000, Type: "user", Detail: "later"},
	}
	fb := &countingSource{entries: []clievent.EventEntry{
		{UUID: "early", Time: 1, Type: "user", Detail: "first ever"},
		{UUID: "between", Time: 70_000, Type: "user", Detail: "outside every window"},
	}}
	got := (&Source{Fallback: fb}).GapFill(context.Background(), local, 0, 100)
	if want := []string{"early"}; !slices.Equal(uuids(got), want) {
		t.Fatalf("GapFill = %v, want %v", uuids(got), want)
	}
}

// TestGapFill_SkipsTwinsAndRepeatedUUIDs: inside the window, a fallback row is
// not filled when its UUID is a local row's, when a resume chain surfaced the
// same record twice (second copy), or when it pairs by content with a local
// row inside the skew window.
func TestGapFill_SkipsTwinsAndRepeatedUUIDs(t *testing.T) {
	local := []clievent.EventEntry{
		{UUID: "l0", Time: 1000, Type: "text", Detail: "start"},
		{UUID: "l3", Time: 3000, Type: "user", Detail: "kept by local"},
		{UUID: "shared", Time: 3100, Type: "text", Detail: "same uuid both tiers"},
		gapRec(4000),
		{UUID: "l4", Time: 4000, Type: "text", Detail: "end"},
	}
	fb := &countingSource{entries: []clievent.EventEntry{
		{UUID: "shared", Time: 3100, Type: "text", Detail: "same uuid both tiers"},
		{UUID: "twin", Time: 3500, Type: "user", Detail: "kept by local"},
		{UUID: "dropped", Time: 3600, Type: "text", Detail: "lost turn"},
		{UUID: "dropped", Time: 3600, Type: "text", Detail: "lost turn"},
	}}
	got := (&Source{Fallback: fb}).GapFill(context.Background(), local, 0, 100)
	if want := []string{"dropped"}; !slices.Equal(uuids(got), want) {
		t.Fatalf("GapFill = %v, want %v", uuids(got), want)
	}
}

// TestGapFill_Floor: rows below floor belong to the disk tier, which already
// keeps unpaired fallback rows; filling them too would show them twice.
func TestGapFill_Floor(t *testing.T) {
	local := []clievent.EventEntry{
		{UUID: "l0", Time: 1000, Type: "text", Detail: "start"},
		gapRec(9000),
	}
	fb := &countingSource{entries: []clievent.EventEntry{
		{UUID: "below", Time: 4999, Type: "text", Detail: "a"},
		{UUID: "at", Time: 5000, Type: "text", Detail: "b"},
		{UUID: "above", Time: 6000, Type: "text", Detail: "c"},
	}}
	got := (&Source{Fallback: fb}).GapFill(context.Background(), local, 5000, 100)
	if want := []string{"at", "above"}; !slices.Equal(uuids(got), want) {
		t.Fatalf("GapFill = %v, want %v", uuids(got), want)
	}
}

// TestGapFill_NoGapRecordReadsNothing: the common session (no gap record) pays
// no fallback I/O, and the degenerate receivers return nil without a read.
func TestGapFill_NoGapRecordReadsNothing(t *testing.T) {
	ctx := context.Background()
	local := []clievent.EventEntry{
		{UUID: "l1", Time: 1000, Type: "user", Detail: "q1"},
		{UUID: "l2", Time: 2000, Type: "text", Detail: "a1"},
	}
	fb := &countingSource{entries: []clievent.EventEntry{
		{UUID: "c9", Time: 1500, Type: "user", Detail: "fallback only"},
	}}
	src := &Source{Fallback: fb}
	if got := src.GapFill(ctx, local, 0, 100); got != nil {
		t.Errorf("GapFill without a gap record = %v, want nil", uuids(got))
	}
	if got := src.GapFill(ctx, nil, 0, 100); got != nil {
		t.Errorf("GapFill on empty local = %v, want nil", uuids(got))
	}
	withGap := append(slices.Clone(local), gapRec(3000))
	if got := src.GapFill(ctx, withGap, 0, 0); got != nil {
		t.Errorf("GapFill with limit 0 = %v, want nil", uuids(got))
	}
	if fb.calls != 0 {
		t.Errorf("fallback read %d times, want 0", fb.calls)
	}
	var nilSrc *Source
	if got := nilSrc.GapFill(ctx, withGap, 0, 100); got != nil {
		t.Errorf("nil Source GapFill = %v, want nil", uuids(got))
	}
	if got := (&Source{}).GapFill(ctx, withGap, 0, 100); got != nil {
		t.Errorf("nil Fallback GapFill = %v, want nil", uuids(got))
	}
}

// TestGapFill_FallbackErrorReturnsNil: a failed fallback read fills nothing,
// even when it also handed back rows.
func TestGapFill_FallbackErrorReturnsNil(t *testing.T) {
	local := []clievent.EventEntry{
		{UUID: "l0", Time: 1000, Type: "text", Detail: "start"},
		gapRec(5000),
	}
	fb := &countingSource{
		entries: []clievent.EventEntry{{UUID: "c", Time: 2000, Type: "text", Detail: "x"}},
		err:     errors.New("transcript unreadable"),
	}
	if got := (&Source{Fallback: fb}).GapFill(context.Background(), local, 0, 100); got != nil {
		t.Fatalf("GapFill on a failed read = %v, want nil", uuids(got))
	}
}
