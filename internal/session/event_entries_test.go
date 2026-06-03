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
