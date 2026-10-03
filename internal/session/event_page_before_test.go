package session

import (
	"context"
	"errors"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/history"
)

func textRun(start int64, n int) []clievent.EventEntry {
	out := make([]clievent.EventEntry, n)
	for i := range out {
		out[i] = clievent.EventEntry{Time: start + int64(i), Type: "text"}
	}
	return out
}

// dupKeyRun is 5:K stored twice (a UUID spooled twice) under 6:y, with 4:x
// below when older is set: a page holding 5:K 6:y has an unread entry only
// if 4:x exists.
func dupKeyRun(older bool) []clievent.EventEntry {
	out := []clievent.EventEntry{sameMSEntry(5, "K"), sameMSEntry(5, "K"), sameMSEntry(6, "y")}
	if older {
		out = append([]clievent.EventEntry{sameMSEntry(4, "x")}, out...)
	}
	return out
}

func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// A short page only means end-of-history when the disk tier was read
// cleanly; every other way of coming up short must report hasMore so the
// dashboard keeps "load earlier" retryable.
func TestEventPageBeforeCtx_HasMore(t *testing.T) {
	t.Parallel()
	diskErr := errors.New("disk read failed")
	cases := []struct {
		name    string
		mem     []clievent.EventEntry
		src     history.Source // nil: no source installed
		ctx     context.Context
		before  int64
		limit   int
		wantLen int
		want    bool
	}{
		{name: "short memory, source error", mem: textRun(100, 3),
			src: &fakeHistorySource{err: diskErr}, before: 200, limit: 10, wantLen: 3, want: true},
		{name: "empty memory, source error",
			src: &fakeHistorySource{err: diskErr}, before: 200, limit: 10, wantLen: 0, want: true},
		{name: "cancelled ctx swallowed as a clean empty read", mem: textRun(100, 3),
			src: &pagingHistorySource{}, ctx: cancelledCtx(), before: 200, limit: 10, wantLen: 3, want: true},
		{name: "disk exhausted exactly at the page bottom", mem: textRun(100, 2),
			src: &pagingHistorySource{all: textRun(1, 8)}, before: 200, limit: 10, wantLen: 10, want: false},
		{name: "full memory page, older entry in memory", mem: textRun(100, 11),
			src: &pagingHistorySource{}, before: 200, limit: 10, wantLen: 10, want: true},
		{name: "full memory page, nothing older", mem: textRun(100, 10),
			src: &pagingHistorySource{}, before: 200, limit: 10, wantLen: 10, want: false},
		{name: "full memory page, has-more probe hits a source error", mem: textRun(100, 10),
			src: &fakeHistorySource{err: diskErr}, before: 200, limit: 10, wantLen: 10, want: true},
		{name: "full memory page, has-more probe cancelled", mem: textRun(100, 10),
			src: &pagingHistorySource{}, ctx: cancelledCtx(), before: 200, limit: 10, wantLen: 10, want: true},
		{name: "full memory page, older entry only on disk", mem: textRun(100, 10),
			src: &pagingHistorySource{all: textRun(1, 1)}, before: 200, limit: 10, wantLen: 10, want: true},
		{name: "nil source, short memory", mem: textRun(100, 3), before: 200, limit: 10, wantLen: 3, want: false},
		{name: "zero-Time memory head skips the disk", mem: []clievent.EventEntry{{Time: 0, Type: "text"}},
			src: &pagingHistorySource{all: textRun(1, 1)}, before: 200, limit: 10, wantLen: 1, want: true},
		{name: "held key twice on disk, older entry below", src: &pagingHistorySource{all: dupKeyRun(true)},
			before: 7, limit: 2, wantLen: 2, want: true},
		{name: "held key twice on disk, nothing older", src: &pagingHistorySource{all: dupKeyRun(false)},
			before: 7, limit: 2, wantLen: 2, want: false},
		{name: "held key twice in memory, older entry below", mem: dupKeyRun(true),
			before: 7, limit: 2, wantLen: 2, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &ManagedSession{key: "k"}
			s.persistedHistory = tc.mem
			if tc.src != nil {
				s.SetHistorySource(tc.src)
			}
			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			got, hasMore := s.EventPageBeforeCtx(ctx, tc.before, tc.limit)
			if len(got) != tc.wantLen {
				t.Errorf("len=%d want %d (page: %s)", len(got), tc.wantLen, uuidList(got))
			}
			if hasMore != tc.want {
				t.Errorf("hasMore=%v want %v", hasMore, tc.want)
			}
		})
	}
}

// The page bottom splits a same-millisecond group: the sibling below the cut
// is still unread, so hasMore must be true even though nothing is strictly
// older than the page.
func TestEventPageBeforeCtx_HasMoreForSameMSSiblingBelowPage(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.SetHistorySource(&pagingHistorySource{all: []clievent.EventEntry{
		sameMSEntry(5, "a"), sameMSEntry(5, "b"), sameMSEntry(5, "c"),
		sameMSEntry(6, "d"), sameMSEntry(7, "e"),
	}})
	got, hasMore := s.EventPageBeforeCtx(context.Background(), 8, 4)
	if uuidList(got) != "5:b 5:c 6:d 7:e" {
		t.Fatalf("page = %s, want 5:b 5:c 6:d 7:e", uuidList(got))
	}
	if !hasMore {
		t.Error("hasMore=false but 5:a is still unread")
	}
}

// Walking pages with the strict cursor the dashboard sends today must report
// hasMore on every page but the last, and the last must reach the oldest
// entry.
func TestEventPageBeforeCtx_WalkStopsExactlyAtOldest(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = textRun(100, 5)
	s.SetHistorySource(&pagingHistorySource{all: textRun(1, 25)})
	before, pages := int64(1000), 0
	for {
		pages++
		if pages > 10 {
			t.Fatal("walk did not terminate")
		}
		got, hasMore := s.EventPageBeforeCtx(context.Background(), before, 10)
		if len(got) == 0 {
			t.Fatalf("page %d is empty: the previous page reported hasMore", pages)
		}
		before = got[0].Time
		if !hasMore {
			break
		}
	}
	if before != 1 || pages != 3 {
		t.Errorf("walk ended at Time=%d after %d pages, want Time=1 after 3", before, pages)
	}
}

// The initial page probes the same way: a disk error under it must leave
// "load earlier" visible rather than look like the oldest event.
func TestEventInitialPageCtx_FailsOpenOnSourceError(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = textRun(100, 3)
	s.SetHistorySource(&fakeHistorySource{err: errors.New("disk read failed")})
	entries, hasMore := s.EventInitialPageCtx(context.Background(), DefaultVisibleTarget, maxVisibleTotal)
	if len(entries) != 3 {
		t.Fatalf("got %d entries want 3", len(entries))
	}
	if !hasMore {
		t.Error("hasMore=false although the disk tier below memory was never read")
	}
}

// The initial page probes with the same widening: when its disk page ends on
// a held key stored twice, the probe must not use up its read on the two
// copies and hide the older entry.
func TestEventInitialPageCtx_HasMoreWithHeldKeyTwiceOnDisk(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	disk := append(dupKeyRun(true)[:3], textRun(6, visibleDiskPageSize-1)...)
	s.SetHistorySource(&pagingHistorySource{all: disk})
	entries, hasMore := s.EventInitialPageCtx(context.Background(), 1, 1)
	if len(entries) != visibleDiskPageSize || entries[0].UUID != "K" {
		t.Fatalf("got %d entries from %s, want a full disk page from 5:K",
			len(entries), uuidList(entries[:min(1, len(entries))]))
	}
	if !hasMore {
		t.Error("hasMore=false but 4:x is still unread")
	}
}
