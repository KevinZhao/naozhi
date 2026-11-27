package session

import (
	"context"
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// fakeHistorySource drives the disk-tier fallback without touching the
// filesystem. Records calls so tests can assert the fallback is (or is not)
// consulted.
type fakeHistorySource struct {
	calls      int
	lastBefore int64
	lastLimit  int
	entries    []clievent.EventEntry
	err        error
}

func (f *fakeHistorySource) LoadBefore(_ context.Context, beforeMS int64, limit int) ([]clievent.EventEntry, error) {
	f.calls++
	f.lastBefore, f.lastLimit = beforeMS, limit
	if f.err != nil {
		return nil, f.err
	}
	return f.entries, nil
}

func TestEventEntriesSince_ReturnsSorted(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	// Interleaved timestamps — mimics the real persistedHistory state after
	// multiple InjectHistory calls across a session chain.
	s.persistedHistory = []clievent.EventEntry{
		{Time: 300, Summary: "c"},
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
		{Time: 500, Summary: "e"},
		{Time: 400, Summary: "d"},
	}

	got := s.EventEntriesSince(150)
	// All except "a" have Time > 150.
	if len(got) != 4 {
		t.Fatalf("len=%d want 4", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time < got[i-1].Time {
			t.Errorf("result not sorted: got[%d].Time=%d < got[%d].Time=%d",
				i, got[i].Time, i-1, got[i-1].Time)
		}
	}
}

func TestEventEntriesBefore_ReturnsSorted(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []clievent.EventEntry{
		{Time: 300, Summary: "c"},
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
		{Time: 500, Summary: "e"},
		{Time: 400, Summary: "d"},
	}

	got := s.EventEntriesBefore(450, 10)
	if len(got) != 4 {
		t.Fatalf("len=%d want 4 (a,b,c,d)", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time < got[i-1].Time {
			t.Errorf("result not sorted at i=%d", i)
		}
		if got[i].Time >= 450 {
			t.Errorf("got[%d].Time=%d not strictly < 450", i, got[i].Time)
		}
	}
}

func TestEventEntriesBeforeCtx_FallsBackToSourceWhenMemoryEmpty(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	// persistedHistory is empty → memory tier yields nothing → Source must
	// be consulted.
	fake := &fakeHistorySource{
		entries: []clievent.EventEntry{
			{Time: 10, Summary: "old-1"},
			{Time: 20, Summary: "old-2"},
		},
	}
	s.SetHistorySource(fake)

	got := s.EventEntriesBeforeCtx(context.Background(), 100, 10)
	if fake.calls != 1 {
		t.Errorf("expected 1 Source call, got %d", fake.calls)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	if got[0].Summary != "old-1" || got[1].Summary != "old-2" {
		t.Errorf("got %+v, want old-1 then old-2", got)
	}
}

func TestEventEntriesBeforeCtx_SkipsSourceWhenMemoryFillsPage(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []clievent.EventEntry{
		{Time: 40, Summary: "mem-a"},
		{Time: 50, Summary: "mem-b"},
	}
	fake := &fakeHistorySource{
		entries: []clievent.EventEntry{{Time: 10, Summary: "disk"}},
	}
	s.SetHistorySource(fake)

	got := s.EventEntriesBeforeCtx(context.Background(), 100, 2)
	if fake.calls != 0 {
		t.Errorf("a full memory page must not consult Source; got %d calls", fake.calls)
	}
	if len(got) != 2 || got[0].Summary != "mem-a" || got[1].Summary != "mem-b" {
		t.Errorf("got %+v, want mem-a, mem-b", got)
	}
}

// A short memory page means the memory bottom was reached. Returning it alone
// made the dashboard's "load earlier" stop there ("没有更早的事件") while the
// disk tier still held older history; the rest of the page must come from
// disk, anchored strictly before the earliest memory entry so tiers never
// overlap.
func TestEventEntriesBeforeCtx_TopsUpShortMemoryPageFromSource(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []clievent.EventEntry{
		{Time: 50, Summary: "mem-a"},
	}
	fake := &fakeHistorySource{
		entries: []clievent.EventEntry{
			{Time: 10, Summary: "disk-1"},
			{Time: 20, Summary: "disk-2"},
		},
	}
	s.SetHistorySource(fake)

	got := s.EventEntriesBeforeCtx(context.Background(), 100, 10)
	if fake.calls != 1 {
		t.Fatalf("short memory page must consult Source once; got %d calls", fake.calls)
	}
	if fake.lastBefore != 51 {
		t.Errorf("disk read anchored at before=%d, want 51 (re-admits the earliest memory millisecond)", fake.lastBefore)
	}
	if fake.lastLimit != 10 {
		t.Errorf("disk read limit=%d, want 10 (page minus memory entries, plus the one held at the seam)", fake.lastLimit)
	}
	want := []string{"disk-1", "disk-2", "mem-a"}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %v", got, want)
	}
	for i, w := range want {
		if got[i].Summary != w {
			t.Errorf("got[%d]=%q want %q", i, got[i].Summary, w)
		}
	}
}

func TestEventEntriesBeforeCtx_SourceErrorKeepsMemoryPage(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []clievent.EventEntry{{Time: 50, Summary: "mem-a"}}
	s.SetHistorySource(&fakeHistorySource{err: errors.New("disk read failed")})

	got := s.EventEntriesBeforeCtx(context.Background(), 100, 10)
	if len(got) != 1 || got[0].Summary != "mem-a" {
		t.Errorf("got %+v, want the memory page on Source error", got)
	}
}

// Every production write path (ring append's zero-Time fallback to
// time.Now(), and the Claude/Codex/Kiro JSONL readers' explicit ts<=0 drop)
// keeps Time>0 in anything that reaches persistedHistory, so mem[0].Time<=0
// should be unreachable there. This pins the defensive branch anyway: if that
// invariant is ever violated, skipping the disk top-up (rather than passing
// a beforeMS<=0 "no upper bound" down and risking a memory/disk overlap) is
// the deliberately chosen degradation.
func TestEventEntriesBeforeCtx_ZeroTimeMemoryHeadSkipsDiskTopUp(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []clievent.EventEntry{{Time: 0, Summary: "mem-a"}}
	fake := &fakeHistorySource{entries: []clievent.EventEntry{{Time: -10, Summary: "disk"}}}
	s.SetHistorySource(fake)

	got := s.EventEntriesBeforeCtx(context.Background(), 100, 10)
	if fake.calls != 0 {
		t.Errorf("a zero-Time memory head must not consult Source; got %d calls", fake.calls)
	}
	if len(got) != 1 || got[0].Summary != "mem-a" {
		t.Errorf("got %+v, want just the memory page", got)
	}
}

func TestEventEntriesBeforeCtx_NilSourceReturnsEmpty(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	// No Source installed, no memory → legitimate empty result.
	got := s.EventEntriesBeforeCtx(context.Background(), 100, 10)
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestEventEntriesBeforeCtx_LimitZeroShortCircuits(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	fake := &fakeHistorySource{entries: []clievent.EventEntry{{Time: 1}}}
	s.SetHistorySource(fake)

	got := s.EventEntriesBeforeCtx(context.Background(), 100, 0)
	if got != nil {
		t.Errorf("limit=0 must return nil, got %+v", got)
	}
	if fake.calls != 0 {
		t.Errorf("limit=0 must not consult Source, got %d calls", fake.calls)
	}
}

func TestSnapshotChainIDs_IncludesCurrentWhenSet(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.prevSessionIDs = []string{"p1", "p2"}
	s.setSessionID("cur")

	got := s.SnapshotChainIDs()
	want := []string{"p1", "p2", "cur"}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestSnapshotChainIDs_OmitsEmptyCurrent(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.prevSessionIDs = []string{"p1"}
	// No setSessionID call — current is "".

	got := s.SnapshotChainIDs()
	if len(got) != 1 || got[0] != "p1" {
		t.Errorf("got %v, want [p1]", got)
	}
}

// TestEventEntriesSince_MonotonicInjectKeepsSortedFlag verifies the
// R237-PERF-12 fast path: appending Time-monotonic batches via
// InjectHistory marks the slice sorted so subsequent EventEntriesSince
// reads can skip the per-call stable sort.
func TestEventEntriesSince_MonotonicInjectKeepsSortedFlag(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.InjectHistory([]clievent.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
		{Time: 300, Summary: "c"},
	})
	if !s.persistedHistorySorted {
		t.Fatalf("monotonic InjectHistory should set persistedHistorySorted=true")
	}
	// Second monotonic batch (continuing the tail) keeps the flag.
	s.InjectHistory([]clievent.EventEntry{
		{Time: 400, Summary: "d"},
		{Time: 500, Summary: "e"},
	})
	if !s.persistedHistorySorted {
		t.Fatalf("continued monotonic InjectHistory should keep flag true")
	}
	got := s.EventEntriesSince(150)
	if len(got) != 4 {
		t.Fatalf("len=%d want 4", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time < got[i-1].Time {
			t.Errorf("result not sorted at i=%d", i)
		}
	}
}

// TestEventEntriesSince_OutOfOrderInjectSortsEagerly verifies that an
// InjectHistory batch breaking Time monotonicity sorts persistedHistory
// eagerly under the existing write lock (R040034-PERF-6 / #1405) instead
// of deferring the sort to the first reader. The reader fast-path is
// then RLock-only forever after, eliminating the historyMu.Lock window
// that used to block concurrent EventEntries / EventEntriesSince
// callers on the WS push hot path.
func TestEventEntriesSince_OutOfOrderInjectSortsEagerly(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.InjectHistory([]clievent.EventEntry{
		{Time: 300, Summary: "c"},
		{Time: 100, Summary: "a"}, // breaks order vs the previous entry
		{Time: 200, Summary: "b"},
	})
	// Post-fix: flag is true immediately after InjectHistory because the
	// eager-sort path absorbed the cost under the write lock.
	if !s.persistedHistorySorted {
		t.Fatalf("non-monotonic InjectHistory should sort eagerly and leave persistedHistorySorted=true")
	}
	got := s.EventEntriesSince(0)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time < got[i-1].Time {
			t.Errorf("EventEntriesSince must still return chronological output even when input was out of order")
		}
	}
}

// TestEventEntriesSinceDeadSessionShortCircuit pins the dead-session fast
// path: when persistedHistorySorted=true and the latest entry's Time is
// already <= afterMS, EventEntriesSince must return nil without scanning
// the whole slice. Mirrors the steady-state idle dashboard poll where
// every dead-session tab calls in once a second with afterMS = last seen.
// R260528-PERF-4.
func TestEventEntriesSinceDeadSessionShortCircuit(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []clievent.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
		{Time: 300, Summary: "c"},
	}
	s.persistedHistorySorted = true

	// afterMS == last.Time → no entries strictly newer.
	if got := s.EventEntriesSince(300); got != nil {
		t.Fatalf("afterMS==last.Time: got %v want nil", got)
	}
	// afterMS > last.Time → still nothing.
	if got := s.EventEntriesSince(500); got != nil {
		t.Fatalf("afterMS>last.Time: got %v want nil", got)
	}
	// afterMS < last.Time → must still return the suffix.
	got := s.EventEntriesSince(150)
	if len(got) != 2 || got[0].Time != 200 || got[1].Time != 300 {
		t.Fatalf("afterMS<last.Time: got %+v want [200 300]", got)
	}

	// Empty history → nil regardless of sorted flag.
	empty := &ManagedSession{key: "k2"}
	if got := empty.EventEntriesSince(0); got != nil {
		t.Fatalf("empty: got %v want nil", got)
	}
	empty.persistedHistorySorted = true
	if got := empty.EventEntriesSince(0); got != nil {
		t.Fatalf("empty sorted: got %v want nil", got)
	}
}

// TestEventEntriesSinceAppend_ReusesBuffer verifies the append variant
// appends into a pre-allocated slice so callers can reuse capacity.
func TestEventEntriesSinceAppend_ReusesBuffer(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []clievent.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
	}
	s.persistedHistorySorted = true

	pool := make([]clievent.EventEntry, 0, 8)
	got := s.EventEntriesSinceAppend(pool, 0)
	if len(got) != 2 {
		t.Fatalf("len = %d want 2", len(got))
	}
	// Buffer was reused if cap is still 8 (no new backing allocation needed).
	if cap(got) != 8 {
		t.Errorf("cap = %d want 8 (buffer should have been reused)", cap(got))
	}
}

// TestEventEntriesSinceAppend_EmptyHistory mirrors the nil-return contract of
// EventEntriesSince: nil dst + empty history = nil result.
func TestEventEntriesSinceAppend_EmptyHistory(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistorySorted = true
	if got := s.EventEntriesSinceAppend(nil, 0); got != nil {
		t.Errorf("empty history with nil dst: got %v want nil", got)
	}
}

// TestEventEntriesSinceAppend_LiveProcessNilDstNoExtraCopy verifies the
// #1701 fast path: on the live-process path with a nil/empty dst, the append
// variant hands back proc.EventEntriesSince's slice directly (no extra append
// copy) while still returning the correct entries.
func TestEventEntriesSinceAppend_LiveProcessNilDstNoExtraCopy(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	proc := NewTestProcess()
	proc.InjectHistory([]clievent.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
		{Time: 300, Summary: "c"},
	})
	s.storeProcess(proc)

	want := proc.EventEntriesSince(150)
	got := s.EventEntriesSinceAppend(nil, 150)
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Time != want[i].Time || got[i].Summary != want[i].Summary {
			t.Errorf("entry[%d]: got {Time:%d Summary:%q} want {Time:%d Summary:%q}",
				i, got[i].Time, got[i].Summary, want[i].Time, want[i].Summary)
		}
	}
}

// TestEventEntriesSinceAppend_LiveProcessAppendsToNonEmptyDst verifies that
// on the live-process path a non-empty dst still has the entries appended
// after its existing contents (#1701 only short-circuits the empty-dst case;
// a non-empty dst must keep its prefix).
func TestEventEntriesSinceAppend_LiveProcessAppendsToNonEmptyDst(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	proc := NewTestProcess()
	proc.InjectHistory([]clievent.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
	})
	s.storeProcess(proc)

	dst := []clievent.EventEntry{{Time: 1, Summary: "prefix"}}
	got := s.EventEntriesSinceAppend(dst, 0)
	if len(got) != 3 {
		t.Fatalf("len = %d want 3 (prefix + 2 entries)", len(got))
	}
	if got[0].Summary != "prefix" {
		t.Errorf("got[0]=%q want prefix (existing dst contents must be preserved)", got[0].Summary)
	}
	if got[1].Summary != "a" || got[2].Summary != "b" {
		t.Errorf("appended entries wrong: got %q,%q want a,b", got[1].Summary, got[2].Summary)
	}
}

// TestEventEntriesSinceAppend_LiveProcessNonEmptyDstReusesBuffer pins
// R20260607-PERF-002 (#1922): on the live-process path a NON-empty dst that
// still has spare capacity must have the matched entries appended into that
// spare capacity (reusing the backing array) rather than triggering a fresh
// []clievent.EventEntry allocation inside EventLog. Before the fix the non-empty
// branch called proc.EventEntriesSince (fresh alloc) + append, so the
// resubscribe catch-up path silently lost the #1740 reuse win whenever dst
// carried residual entries.
func TestEventEntriesSinceAppend_LiveProcessNonEmptyDstReusesBuffer(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	proc := NewTestProcess()
	proc.InjectHistory([]clievent.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
		{Time: 300, Summary: "c"},
	})
	s.storeProcess(proc)

	// Buffer with a residual prefix plus ample spare capacity. The two matched
	// entries (Time > 150) must land in the spare slots, sharing dst's array.
	buf := make([]clievent.EventEntry, 0, 8)
	buf = append(buf, clievent.EventEntry{Time: 1, Summary: "prefix"})
	got := s.EventEntriesSinceAppend(buf, 150)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3 (prefix + b + c)", len(got))
	}
	if got[0].Summary != "prefix" || got[1].Summary != "b" || got[2].Summary != "c" {
		t.Fatalf("entries wrong: got %q,%q,%q want prefix,b,c",
			got[0].Summary, got[1].Summary, got[2].Summary)
	}
	// The returned slice must reuse buf's backing array (no fresh allocation):
	// the appended tail occupies buf's spare capacity.
	if cap(got) != cap(buf) || &got[:1][0] != &buf[:1][0] {
		t.Errorf("non-empty dst path did not reuse the caller's buffer backing array (#1922 regression)")
	}
}

// TestEventEntriesSinceAppend_LiveProcessReusesBuffer pins R20260604-PERF-25
// (#1740): on the live-process path an empty (cap>0) dst must have its backing
// array reused rather than a fresh slice allocated per call. Before the fix the
// live branch always called proc.EventEntriesSince (fresh alloc); now it
// forwards into the EventLog append-mode query.
func TestEventEntriesSinceAppend_LiveProcessReusesBuffer(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	proc := NewTestProcess()
	proc.InjectHistory([]clievent.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
		{Time: 300, Summary: "c"},
	})
	s.storeProcess(proc)

	// Pre-grow a buffer, then poll with buf[:0]. The returned slice must share
	// the same backing array (no allocation) AND carry the correct entries.
	buf := make([]clievent.EventEntry, 0, 8)
	got := s.EventEntriesSinceAppend(buf[:0], 150)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2 (entries after Time=150)", len(got))
	}
	if got[0].Summary != "b" || got[1].Summary != "c" {
		t.Errorf("entries wrong: got %q,%q want b,c", got[0].Summary, got[1].Summary)
	}
	if &got[:1][0] != &buf[:1][0] {
		t.Errorf("live path did not reuse the caller's buffer backing array (#1740 regression)")
	}
}

// TestEventEntriesBefore_SortedFlagSkipsSort verifies the R20260603000023-PERF-3
// / #1623 fast path: when persistedHistorySorted=true, EventEntriesBefore must
// return entries in strict Time-ascending order using only slices.Reverse (O(n))
// rather than a full sort. Correctness is the primary invariant tested here.
func TestEventEntriesBefore_SortedFlagSkipsSort(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []clievent.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
		{Time: 300, Summary: "c"},
		{Time: 400, Summary: "d"},
		{Time: 500, Summary: "e"},
	}
	s.persistedHistorySorted = true

	// Request entries before Time=450 — should get times 100..400 ascending.
	got := s.EventEntriesBefore(450, 10)
	if len(got) != 4 {
		t.Fatalf("len=%d want 4", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time <= got[i-1].Time {
			t.Errorf("not strictly ascending at index %d: Time %d <= %d",
				i, got[i].Time, got[i-1].Time)
		}
	}
	if got[0].Time != 100 || got[3].Time != 400 {
		t.Errorf("wrong range: got [%d..%d] want [100..400]", got[0].Time, got[3].Time)
	}
}

// linearSince returns entries with Time > afterMS by brute-force scan.
func linearSince(history []clievent.EventEntry, afterMS int64) []clievent.EventEntry {
	var out []clievent.EventEntry
	for _, e := range history {
		if e.Time > afterMS {
			out = append(out, e)
		}
	}
	return out
}

func assertEntriesEqual(t *testing.T, label string, got, want []clievent.EventEntry) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: len got=%d want=%d", label, len(got), len(want))
		return
	}
	for i := range want {
		if got[i].Time != want[i].Time || got[i].Summary != want[i].Summary {
			t.Errorf("%s: entry[%d] got {%d,%q} want {%d,%q}",
				label, i, got[i].Time, got[i].Summary, want[i].Time, want[i].Summary)
		}
	}
}

func TestEventEntriesSinceAppend_BinarySearch_MatchesLinear(t *testing.T) {
	t.Parallel()
	// The dead-session branch binary-searches the sorted history; its result
	// must equal a linear strict-> scan at every boundary, including on,
	// between, before and past entries and across duplicate Times.
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []clievent.EventEntry{
		{Time: 10, Summary: "a"},
		{Time: 20, Summary: "b1"},
		{Time: 20, Summary: "b2"},
		{Time: 30, Summary: "c"},
		{Time: 40, Summary: "d"},
		{Time: 50, Summary: "e"},
	}
	s.persistedHistorySorted = true

	boundaries := []int64{0, 5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55}
	for _, afterMS := range boundaries {
		want := linearSince(s.persistedHistory, afterMS)
		got := s.EventEntriesSinceAppend(nil, afterMS)
		assertEntriesEqual(t, "linear vs binary afterMS", got, want)
	}
}

// TestHistorySource_ConcurrentSetAndRead pins the race-free contract on the
// atomic.Pointer hand-off: SetHistorySource and EventEntriesBeforeCtx can
// execute concurrently without a -race violation. Without atomic storage
// the plain field assignment was a data race under -race.
func TestHistorySource_ConcurrentSetAndRead(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	fake := &fakeHistorySource{}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			s.SetHistorySource(fake)
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		_ = s.EventEntriesBeforeCtx(context.Background(), 100, 5)
	}
	<-done
}
