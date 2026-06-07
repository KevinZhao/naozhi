package session

import (
	"context"
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// fakeHistorySource drives the disk-tier fallback without touching the
// filesystem. Records calls so tests can assert the fallback is (or is not)
// consulted.
type fakeHistorySource struct {
	calls   int
	entries []cli.EventEntry
	err     error
}

func (f *fakeHistorySource) LoadBefore(_ context.Context, _ int64, _ int) ([]cli.EventEntry, error) {
	f.calls++
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
	s.persistedHistory = []cli.EventEntry{
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
	s.persistedHistory = []cli.EventEntry{
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
		entries: []cli.EventEntry{
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

func TestEventEntriesBeforeCtx_SkipsSourceWhenMemoryHit(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []cli.EventEntry{
		{Time: 50, Summary: "mem-a"},
	}
	fake := &fakeHistorySource{
		entries: []cli.EventEntry{{Time: 10, Summary: "disk"}},
	}
	s.SetHistorySource(fake)

	got := s.EventEntriesBeforeCtx(context.Background(), 100, 10)
	if fake.calls != 0 {
		t.Errorf("memory hit must not consult Source; got %d calls", fake.calls)
	}
	if len(got) != 1 || got[0].Summary != "mem-a" {
		t.Errorf("got %+v, want mem-a", got)
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

func TestEventEntriesBeforeCtx_SourceErrorTreatedAsEnd(t *testing.T) {
	// A Source error must not propagate as "partial result" — the handler
	// treats it as end-of-history so the dashboard stops retrying. We log
	// the error for the operator but return nil to the caller.
	t.Parallel()
	s := &ManagedSession{key: "k"}
	fake := &fakeHistorySource{err: errors.New("disk read failed")}
	s.SetHistorySource(fake)

	got := s.EventEntriesBeforeCtx(context.Background(), 100, 10)
	if fake.calls != 1 {
		t.Errorf("Source must be called exactly once, got %d", fake.calls)
	}
	if got != nil {
		t.Errorf("got %+v, want nil on Source error", got)
	}
}

func TestEventEntriesBeforeCtx_LimitZeroShortCircuits(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	fake := &fakeHistorySource{entries: []cli.EventEntry{{Time: 1}}}
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

func TestSnapshotChainIDs_AllEmpty(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	got := s.SnapshotChainIDs()
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// TestEventEntriesSince_MonotonicInjectKeepsSortedFlag verifies the
// R237-PERF-12 fast path: appending Time-monotonic batches via
// InjectHistory marks the slice sorted so subsequent EventEntriesSince
// reads can skip the per-call stable sort.
func TestEventEntriesSince_MonotonicInjectKeepsSortedFlag(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.InjectHistory([]cli.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
		{Time: 300, Summary: "c"},
	})
	if !s.persistedHistorySorted {
		t.Fatalf("monotonic InjectHistory should set persistedHistorySorted=true")
	}
	// Second monotonic batch (continuing the tail) keeps the flag.
	s.InjectHistory([]cli.EventEntry{
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
	s.InjectHistory([]cli.EventEntry{
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
	s.persistedHistory = []cli.EventEntry{
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

// TestEventEntriesSinceAppend_EquivalentToSince verifies that the append
// variant returns the same entries as EventEntriesSince (dead-session path).
// R112714-PERF-11.
func TestEventEntriesSinceAppend_EquivalentToSince(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []cli.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
		{Time: 300, Summary: "c"},
	}
	s.persistedHistorySorted = true

	want := s.EventEntriesSince(150)
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

// TestEventEntriesSinceAppend_ReusesBuffer verifies the append variant
// appends into a pre-allocated slice so callers can reuse capacity.
func TestEventEntriesSinceAppend_ReusesBuffer(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []cli.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
	}
	s.persistedHistorySorted = true

	pool := make([]cli.EventEntry, 0, 8)
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
	proc.InjectHistory([]cli.EventEntry{
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
	proc.InjectHistory([]cli.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
	})
	s.storeProcess(proc)

	dst := []cli.EventEntry{{Time: 1, Summary: "prefix"}}
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
// []cli.EventEntry allocation inside EventLog. Before the fix the non-empty
// branch called proc.EventEntriesSince (fresh alloc) + append, so the
// resubscribe catch-up path silently lost the #1740 reuse win whenever dst
// carried residual entries.
func TestEventEntriesSinceAppend_LiveProcessNonEmptyDstReusesBuffer(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	proc := NewTestProcess()
	proc.InjectHistory([]cli.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
		{Time: 300, Summary: "c"},
	})
	s.storeProcess(proc)

	// Buffer with a residual prefix plus ample spare capacity. The two matched
	// entries (Time > 150) must land in the spare slots, sharing dst's array.
	buf := make([]cli.EventEntry, 0, 8)
	buf = append(buf, cli.EventEntry{Time: 1, Summary: "prefix"})
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

// TestEventEntriesSinceAppend_LiveProcessNonEmptyDstNoSpareGrows verifies the
// fallback half of the #1922 fix: when the non-empty dst has NO spare capacity,
// the result still correctly preserves the prefix and folds in the matched
// entries (append grows into a new backing array).
func TestEventEntriesSinceAppend_LiveProcessNonEmptyDstNoSpareGrows(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	proc := NewTestProcess()
	proc.InjectHistory([]cli.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
	})
	s.storeProcess(proc)

	// len == cap so there is no spare capacity past the prefix.
	dst := make([]cli.EventEntry, 0, 1)
	dst = append(dst, cli.EventEntry{Time: 1, Summary: "prefix"})
	got := s.EventEntriesSinceAppend(dst, 0)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3 (prefix + a + b)", len(got))
	}
	if got[0].Summary != "prefix" || got[1].Summary != "a" || got[2].Summary != "b" {
		t.Fatalf("entries wrong: got %q,%q,%q want prefix,a,b",
			got[0].Summary, got[1].Summary, got[2].Summary)
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
	proc.InjectHistory([]cli.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
		{Time: 300, Summary: "c"},
	})
	s.storeProcess(proc)

	// Pre-grow a buffer, then poll with buf[:0]. The returned slice must share
	// the same backing array (no allocation) AND carry the correct entries.
	buf := make([]cli.EventEntry, 0, 8)
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
	s.persistedHistory = []cli.EventEntry{
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

// TestEventEntriesBefore_UnsortedFlagFallsBackToSort verifies that when
// persistedHistorySorted=false, the full stable sort is still applied and
// the result is correct.
func TestEventEntriesBefore_UnsortedFlagFallsBackToSort(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	// Out-of-order insertion order.
	s.persistedHistory = []cli.EventEntry{
		{Time: 300, Summary: "c"},
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
	}
	s.persistedHistorySorted = false

	got := s.EventEntriesBefore(350, 10)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time <= got[i-1].Time {
			t.Errorf("not strictly ascending at index %d: Time %d <= %d",
				i, got[i].Time, got[i-1].Time)
		}
	}
}

// TestEventEntriesBefore_SortedEmptyReturnsNil checks the nil-return contract
// for an empty sorted history.
func TestEventEntriesBefore_SortedEmptyReturnsNil(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistorySorted = true
	if got := s.EventEntriesBefore(500, 10); got != nil {
		t.Errorf("empty sorted: got %v want nil", got)
	}
}

// TestEventEntriesAppend_EquivalentToEventEntries pins R20260607-PERF-6 (#1885):
// the append variant returns the same entries as EventEntries() on the dead /
// persistedHistory path.
func TestEventEntriesAppend_EquivalentToEventEntries(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []cli.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
		{Time: 300, Summary: "c"},
	}

	want := s.EventEntries()
	got := s.EventEntriesAppend(nil)
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

// TestEventEntriesAppend_ReusesBuffer verifies the append variant grows a
// caller-supplied buffer in place instead of allocating a fresh slice, so an
// O(N sessions) scan can reuse one backing array.
func TestEventEntriesAppend_ReusesBuffer(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []cli.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
	}

	pool := make([]cli.EventEntry, 0, 8)
	got := s.EventEntriesAppend(pool)
	if len(got) != 2 {
		t.Fatalf("len = %d want 2", len(got))
	}
	if cap(got) != 8 {
		t.Errorf("cap = %d want 8 (buffer should have been reused)", cap(got))
	}
	// Second session reusing the same backing array (after dst[:0]) must not
	// see the first session's entries leak through.
	s2 := &ManagedSession{key: "k2"}
	s2.persistedHistory = []cli.EventEntry{{Time: 400, Summary: "z"}}
	got2 := s2.EventEntriesAppend(got[:0])
	if len(got2) != 1 || got2[0].Summary != "z" {
		t.Fatalf("reuse leaked prior entries: got %+v", got2)
	}
}

// TestEventEntriesAppend_EmptyHistoryNilDst confirms a nil dst with empty
// history returns nil (no zero-length allocation).
func TestEventEntriesAppend_EmptyHistoryNilDst(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	if got := s.EventEntriesAppend(nil); got != nil {
		t.Errorf("empty history with nil dst: got %v want nil", got)
	}
}

// TestEventEntriesAppend_PreservesPrefix verifies a non-empty dst keeps its
// existing contents and the session entries are appended after them.
func TestEventEntriesAppend_PreservesPrefix(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []cli.EventEntry{{Time: 200, Summary: "b"}}

	dst := []cli.EventEntry{{Time: 1, Summary: "prefix"}}
	got := s.EventEntriesAppend(dst)
	if len(got) != 2 || got[0].Summary != "prefix" || got[1].Summary != "b" {
		t.Fatalf("prefix not preserved: got %+v", got)
	}
}

// TestEventEntriesAppend_LiveProcessPreservesPrefix verifies the live-process
// branch also appends after an existing dst prefix.
func TestEventEntriesAppend_LiveProcessPreservesPrefix(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	proc := NewTestProcess()
	proc.InjectHistory([]cli.EventEntry{
		{Time: 100, Summary: "a"},
		{Time: 200, Summary: "b"},
	})
	s.storeProcess(proc)

	dst := []cli.EventEntry{{Time: 1, Summary: "prefix"}}
	got := s.EventEntriesAppend(dst)
	want := proc.EventEntries()
	if len(got) != len(want)+1 {
		t.Fatalf("len = %d want %d", len(got), len(want)+1)
	}
	if got[0].Summary != "prefix" {
		t.Errorf("prefix dropped: got[0]=%q", got[0].Summary)
	}
}

// TestEventEntriesForKeyAppend pins the Router-level append wrapper:
// unknown key returns dst unchanged; known key appends its history.
func TestEventEntriesForKeyAppend(t *testing.T) {
	t.Parallel()
	r := NewRouter(RouterConfig{})
	s := &ManagedSession{key: "alpha"}
	s.persistedHistory = []cli.EventEntry{{Time: 100, Summary: "a"}}
	r.mu.Lock()
	r.ss.sessions["alpha"] = s
	r.mu.Unlock()

	// Unknown key: dst unchanged.
	dst := []cli.EventEntry{{Time: 1, Summary: "keep"}}
	if got := r.EventEntriesForKeyAppend(dst, "missing"); len(got) != 1 || got[0].Summary != "keep" {
		t.Fatalf("unknown key mutated dst: got %+v", got)
	}

	// Known key appends after prefix.
	got := r.EventEntriesForKeyAppend(dst[:0], "alpha")
	if len(got) != 1 || got[0].Summary != "a" {
		t.Fatalf("known key append: got %+v", got)
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
