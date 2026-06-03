package session

import (
	"context"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
)

func visibleCount(entries []cli.EventEntry) int {
	n := 0
	for i := range entries {
		if cli.IsVisibleEntry(entries[i]) {
			n++
		}
	}
	return n
}

// pagingHistorySource is a disk-tier stub that honours the beforeMS cursor so
// repeated LoadBefore calls walk strictly backward through a fixed corpus —
// the realistic shape the visible-aware reader paginates against.
type pagingHistorySource struct {
	all   []cli.EventEntry // chronological
	calls int
}

func (p *pagingHistorySource) LoadBefore(_ context.Context, beforeMS int64, limit int) ([]cli.EventEntry, error) {
	p.calls++
	// Collect entries strictly older than beforeMS (or all when beforeMS<=0),
	// newest-first, capped at limit, then return chronological.
	var picked []cli.EventEntry
	for i := len(p.all) - 1; i >= 0 && len(picked) < limit; i-- {
		if beforeMS > 0 && p.all[i].Time >= beforeMS {
			continue
		}
		picked = append(picked, p.all[i])
	}
	// reverse to chronological
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	return picked, nil
}

// TestEventLastNVisibleCtx_MemorySufficient: persistedHistory already carries
// enough visible bubbles → disk is never consulted.
func TestEventLastNVisibleCtx_MemorySufficient(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	for i := 0; i < 10; i++ {
		s.persistedHistory = append(s.persistedHistory, cli.EventEntry{Time: int64(i + 1), Type: "text"})
	}
	fake := &pagingHistorySource{all: []cli.EventEntry{{Time: -1, Type: "text"}}}
	s.SetHistorySource(fake)

	got := s.EventLastNVisibleCtx(context.Background(), 5, 100)
	if visibleCount(got) < 5 {
		t.Errorf("visible=%d want >=5", visibleCount(got))
	}
	if fake.calls != 0 {
		t.Errorf("disk consulted %d times, want 0 (memory sufficient)", fake.calls)
	}
}

// TestEventLastNVisibleCtx_FallsThroughToDisk reproduces the screenshot bug:
// persistedHistory is entirely internal events, so the reader must page into
// the disk tier to surface real messages.
func TestEventLastNVisibleCtx_FallsThroughToDisk(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	// Memory: 100 internal events, times 901..1000.
	for i := 0; i < 100; i++ {
		s.persistedHistory = append(s.persistedHistory, cli.EventEntry{Time: int64(901 + i), Type: "task_progress"})
	}
	// Disk: older real messages, times 1..900 (mix of visible + internal).
	var disk []cli.EventEntry
	for i := 1; i <= 900; i++ {
		ty := "task_progress"
		if i%30 == 0 {
			ty = "text" // a visible bubble every 30 entries → 30 total
		}
		disk = append(disk, cli.EventEntry{Time: int64(i), Type: ty})
	}
	fake := &pagingHistorySource{all: disk}
	s.SetHistorySource(fake)

	got := s.EventLastNVisibleCtx(context.Background(), 5, maxVisibleTotal)
	if visibleCount(got) < 5 {
		t.Errorf("visible=%d want >=5 (must walk into disk)", visibleCount(got))
	}
	if fake.calls == 0 {
		t.Error("disk never consulted despite all-internal memory tier")
	}
	// Result must stay chronological across the memory/disk seam.
	for i := 1; i < len(got); i++ {
		if got[i].Time < got[i-1].Time {
			t.Errorf("not chronological at %d: %d < %d", i, got[i].Time, got[i-1].Time)
		}
	}
	// Disk tier must be strictly older than the memory tier (no overlap).
	// The newest disk-sourced entry (time 900) precedes the oldest memory
	// entry (time 901).
	if got[len(got)-1].Time != 1000 {
		t.Errorf("newest entry Time=%d want 1000 (memory tail preserved)", got[len(got)-1].Time)
	}
}

// TestEventLastNVisibleCtx_DiskExhausted: memory all-internal and disk has no
// visible events → reader returns what it has without spinning forever.
func TestEventLastNVisibleCtx_DiskExhausted(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	for i := 0; i < 50; i++ {
		s.persistedHistory = append(s.persistedHistory, cli.EventEntry{Time: int64(101 + i), Type: "tool_use"})
	}
	var disk []cli.EventEntry
	for i := 1; i <= 100; i++ {
		disk = append(disk, cli.EventEntry{Time: int64(i), Type: "tool_use"})
	}
	fake := &pagingHistorySource{all: disk}
	s.SetHistorySource(fake)

	got := s.EventLastNVisibleCtx(context.Background(), 30, maxVisibleTotal)
	if visibleCount(got) != 0 {
		t.Errorf("visible=%d want 0 (no visible anywhere)", visibleCount(got))
	}
	// Bounded by maxVisibleDiskPages — must not exceed that many disk reads.
	if fake.calls > maxVisibleDiskPages {
		t.Errorf("disk calls=%d exceed maxVisibleDiskPages=%d", fake.calls, maxVisibleDiskPages)
	}
}

// TestEventLastNVisibleCtx_NilSourceReturnsMemory: no disk source → memory tier
// returned as-is even when visible target unmet (non-claude backends).
func TestEventLastNVisibleCtx_NilSourceReturnsMemory(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	for i := 0; i < 20; i++ {
		s.persistedHistory = append(s.persistedHistory, cli.EventEntry{Time: int64(i + 1), Type: "tool_use"})
	}
	got := s.EventLastNVisibleCtx(context.Background(), 30, maxVisibleTotal)
	if got == nil {
		t.Fatal("got nil, want memory-tier slice")
	}
	if visibleCount(got) != 0 {
		t.Errorf("visible=%d want 0", visibleCount(got))
	}
}

// TestEventLastNVisibleCtx_CtxCanceled: a canceled context stops disk paging
// promptly, returning the memory tier.
func TestEventLastNVisibleCtx_CtxCanceled(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	for i := 0; i < 10; i++ {
		s.persistedHistory = append(s.persistedHistory, cli.EventEntry{Time: int64(i + 1), Type: "tool_use"})
	}
	var disk []cli.EventEntry
	for i := 1; i <= 100; i++ {
		disk = append(disk, cli.EventEntry{Time: int64(i), Type: "text"})
	}
	fake := &pagingHistorySource{all: disk}
	s.SetHistorySource(fake)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	got := s.EventLastNVisibleCtx(ctx, 30, maxVisibleTotal)
	if fake.calls != 0 {
		t.Errorf("disk consulted %d times despite canceled ctx, want 0", fake.calls)
	}
	if visibleCount(got) != 0 {
		t.Errorf("visible=%d want 0 (memory all-internal, disk skipped)", visibleCount(got))
	}
}

// TestEventLastNVisibleCtx_MultiPageChronological pins the single-concatenation
// fix (R20260603000023-PERF-2 / #1622): when multiple disk pages are loaded, the
// final result must be strictly chronological even though pages are accumulated
// in newest-first order and only assembled once at the end.
func TestEventLastNVisibleCtx_MultiPageChronological(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	// Memory: 5 internal events, times 901..905.
	for i := 0; i < 5; i++ {
		s.persistedHistory = append(s.persistedHistory, cli.EventEntry{
			Time: int64(901 + i),
			Type: "tool_use",
		})
	}
	// Disk: 9 visible entries spread across 3 pages of 3.
	// pagingHistorySource returns chunks in chronological order already.
	var disk []cli.EventEntry
	for i := 1; i <= 9; i++ {
		disk = append(disk, cli.EventEntry{Time: int64(i * 100), Type: "text"})
	}
	fake := &pagingHistorySource{all: disk}
	s.SetHistorySource(fake)

	got := s.EventLastNVisibleCtx(context.Background(), 9, maxVisibleTotal)

	// Must reach at least 9 visible entries from disk.
	if visibleCount(got) < 9 {
		t.Errorf("visible=%d want >=9", visibleCount(got))
	}
	// Result must be strictly non-decreasing in Time.
	for i := 1; i < len(got); i++ {
		if got[i].Time < got[i-1].Time {
			t.Errorf("not chronological at index %d: Time %d < %d",
				i, got[i].Time, got[i-1].Time)
		}
	}
	// Memory tail (times 901..905) must appear after all disk entries.
	// The newest disk entry is time 900; memory starts at 901.
	diskNewest := int64(0)
	for _, e := range got {
		if e.Type != "tool_use" && e.Time > diskNewest {
			diskNewest = e.Time
		}
	}
	if diskNewest != 900 {
		t.Errorf("newest disk entry Time=%d want 900", diskNewest)
	}
}
