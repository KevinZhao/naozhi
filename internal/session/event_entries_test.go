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

	got := s.snapshotChainIDs()
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

	got := s.snapshotChainIDs()
	if len(got) != 1 || got[0] != "p1" {
		t.Errorf("got %v, want [p1]", got)
	}
}

func TestSnapshotChainIDs_AllEmpty(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	got := s.snapshotChainIDs()
	if got != nil {
		t.Errorf("got %v, want nil", got)
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
