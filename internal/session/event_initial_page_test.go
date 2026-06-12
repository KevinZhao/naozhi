package session

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

// TestEventInitialPageCtx_HasMoreWhenOlderExists is the regression for the
// stranded-opening-messages bug: the session has more visible bubbles than the
// returned slice carries, so older history exists and hasMore must be true even
// though the *total* returned event count is below the client's page-size hint
// (INITIAL_HISTORY_LIMIT). The old client heuristic (len >= 100) would have
// returned false here and hidden the "load earlier" affordance.
func TestEventInitialPageCtx_HasMoreWhenOlderExists(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	// Memory tier all-internal so the visible-aware read pages into disk and
	// the returned slice does NOT include the very oldest entries.
	for i := 0; i < 5; i++ {
		s.persistedHistory = append(s.persistedHistory, cli.EventEntry{Time: int64(901 + i), Type: "tool_use"})
	}
	// Disk: 900 entries, a visible bubble every 10th → 90 visible total.
	var disk []cli.EventEntry
	for i := 1; i <= 900; i++ {
		ty := "tool_use"
		if i%10 == 0 {
			ty = "text"
		}
		disk = append(disk, cli.EventEntry{Time: int64(i), Type: ty})
	}
	s.SetHistorySource(&pagingHistorySource{all: disk})

	// Ask for only DefaultVisibleTarget visible bubbles; the slice will stop
	// well short of the oldest disk entry, so older history remains.
	entries, hasMore := s.EventInitialPageCtx(context.Background(), DefaultVisibleTarget, maxVisibleTotal)
	if len(entries) == 0 {
		t.Fatal("got empty slice, want a populated initial page")
	}
	if !hasMore {
		t.Errorf("hasMore=false but older history exists before Time=%d", entries[0].Time)
	}
}

// TestEventInitialPageCtx_NoMoreWhenSliceIsOldest: the returned slice already
// starts at the very first event → hasMore must be false (no useless button).
func TestEventInitialPageCtx_NoMoreWhenSliceIsOldest(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	// 10 visible events entirely in memory, none on disk.
	for i := 1; i <= 10; i++ {
		s.persistedHistory = append(s.persistedHistory, cli.EventEntry{Time: int64(i), Type: "text"})
	}
	fake := &pagingHistorySource{all: nil}
	s.SetHistorySource(fake)

	entries, hasMore := s.EventInitialPageCtx(context.Background(), DefaultVisibleTarget, maxVisibleTotal)
	if len(entries) != 10 {
		t.Fatalf("got %d entries want 10", len(entries))
	}
	if hasMore {
		t.Error("hasMore=true but the slice already includes the oldest event")
	}
}

// TestEventInitialPageCtx_EmptySession: no history at all → empty slice, no more.
func TestEventInitialPageCtx_EmptySession(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	entries, hasMore := s.EventInitialPageCtx(context.Background(), DefaultVisibleTarget, maxVisibleTotal)
	if len(entries) != 0 {
		t.Errorf("got %d entries want 0", len(entries))
	}
	if hasMore {
		t.Error("hasMore=true on an empty session")
	}
}

// ctxCancelAwareSource returns memory-tier entries on a live ctx but reports
// nil (no error surfaced) once the ctx is cancelled — mirroring how a real
// disk source aborts a reverse JSONL scan mid-walk. Used to prove the hasMore
// probe fails OPEN when its shared ctx budget is already spent.
type ctxCancelAwareSource struct {
	all []cli.EventEntry // chronological; older history that exists on disk
}

func (s *ctxCancelAwareSource) LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]cli.EventEntry, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var picked []cli.EventEntry
	for i := len(s.all) - 1; i >= 0 && len(picked) < limit; i-- {
		if beforeMS > 0 && s.all[i].Time >= beforeMS {
			continue
		}
		picked = append(picked, s.all[i])
	}
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	return picked, nil
}

// TestEventInitialPageCtx_FailsOpenOnCtxCancel is the regression for the
// shared-budget starvation hole the reviewers flagged: EventInitialPageCtx
// reuses one ctx for the visible-aware walk AND the hasMore probe. On a slow
// filesystem the walk can drain the deadline, so the probe runs under a
// cancelled ctx and EventEntriesBeforeCtx returns nil — indistinguishable from
// genuine end-of-history. Reporting hasMore=false there would re-hide "load
// earlier" and re-strand the opening messages. The reader must instead fail
// OPEN (hasMore=true) whenever the probe came back empty under a cancelled ctx.
func TestEventInitialPageCtx_FailsOpenOnCtxCancel(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	// Enough in-memory visible bubbles that the visible-aware read is satisfied
	// from memory alone (no disk walk needed for the slice itself), so the slice
	// is non-empty and entries[0] is a real anchor.
	for i := 1; i <= DefaultVisibleTarget+5; i++ {
		s.persistedHistory = append(s.persistedHistory, cli.EventEntry{Time: int64(1000 + i), Type: "text"})
	}
	// Disk genuinely holds older history (times 1..900), but the cancelled ctx
	// will prevent the probe from ever seeing it.
	var disk []cli.EventEntry
	for i := 1; i <= 900; i++ {
		disk = append(disk, cli.EventEntry{Time: int64(i), Type: "text"})
	}
	s.SetHistorySource(&ctxCancelAwareSource{all: disk})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // budget already spent — the probe will run cancelled

	entries, hasMore := s.EventInitialPageCtx(ctx, DefaultVisibleTarget, maxVisibleTotal)
	if len(entries) == 0 {
		t.Fatal("empty slice; memory tier should have satisfied the visible target")
	}
	if !hasMore {
		t.Error("hasMore=false under a cancelled ctx; must fail OPEN so 'load earlier' stays mounted rather than stranding older history")
	}
}

// TestEventInitialPageCtx_CleanEmptyProbeReportsNoMore: a live ctx whose probe
// genuinely finds nothing older must still report hasMore=false (the fail-open
// guard must not swallow real end-of-history).
func TestEventInitialPageCtx_CleanEmptyProbeReportsNoMore(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	for i := 1; i <= 10; i++ {
		s.persistedHistory = append(s.persistedHistory, cli.EventEntry{Time: int64(i), Type: "text"})
	}
	// Live ctx, no older history on disk.
	s.SetHistorySource(&ctxCancelAwareSource{all: nil})
	entries, hasMore := s.EventInitialPageCtx(context.Background(), DefaultVisibleTarget, maxVisibleTotal)
	if len(entries) != 10 {
		t.Fatalf("got %d entries want 10", len(entries))
	}
	if hasMore {
		t.Error("hasMore=true on a clean empty probe; the slice already holds the oldest event")
	}
}

// TestEventInitialPageCtx_MemoryHasOlder: the slice is a tail of an in-memory
// history that has older entries the slice omitted → hasMore true via the ring
// (no disk source needed). Mirrors a live session whose ring holds more than
// DefaultVisibleTarget visible bubbles.
func TestEventInitialPageCtx_MemoryHasOlder(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	// 2*DefaultVisibleTarget visible events in memory; the visible-aware read
	// returns only the last DefaultVisibleTarget, leaving older ones in the ring.
	n := 2 * DefaultVisibleTarget
	for i := 1; i <= n; i++ {
		s.persistedHistory = append(s.persistedHistory, cli.EventEntry{Time: int64(i), Type: "text"})
	}
	// No disk source: the limit=1 probe must find the older entry in memory.
	entries, hasMore := s.EventInitialPageCtx(context.Background(), DefaultVisibleTarget, maxVisibleTotal)
	if len(entries) == 0 {
		t.Fatal("empty slice")
	}
	if entries[0].Time <= 1 {
		t.Skipf("slice already reached the oldest entry (Time=%d); ring returned everything", entries[0].Time)
	}
	if !hasMore {
		t.Errorf("hasMore=false but ring holds entries older than Time=%d", entries[0].Time)
	}
}
