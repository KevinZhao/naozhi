package session

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// sameMSEntry builds a visible entry; same-millisecond groups in these tests
// share Time and differ by UUID.
func sameMSEntry(t int64, uuid string) clievent.EventEntry {
	return clievent.EventEntry{Time: t, UUID: uuid, Type: "text", Summary: uuid}
}

// internalRun returns n internal entries at consecutive Times from start.
func internalRun(start int64, n int) []clievent.EventEntry {
	out := make([]clievent.EventEntry, n)
	for i := range out {
		out[i] = clievent.EventEntry{Time: start + int64(i), UUID: fmt.Sprintf("i%d", start+int64(i)), Type: "tool_use"}
	}
	return out
}

// requireEachOnce fails unless entries carry every uuid in want exactly once
// (other uuids are allowed).
func requireEachOnce(t *testing.T, entries []clievent.EventEntry, want ...string) {
	t.Helper()
	n := map[string]int{}
	for _, e := range entries {
		n[e.UUID]++
	}
	for _, u := range want {
		if n[u] != 1 {
			t.Errorf("uuid %q appears %d times, want 1 (page: %s)", u, n[u], uuidList(entries))
		}
	}
}

func uuidList(entries []clievent.EventEntry) string {
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = fmt.Sprintf("%d:%s", e.Time, e.UUID)
	}
	return strings.Join(parts, " ")
}

// The ring holds the newer two of a three-entry millisecond; the spool holds
// all three. Walking EventEntriesBeforeCtx with the dashboard's cursor
// convention (before = oldest ms + 1, held siblings dropped client-side) must
// reach the evicted sibling, and no page may carry an entry twice.
func TestEventEntriesBeforeCtx_SameMSSiblingAcrossMemoryDiskSeam(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []clievent.EventEntry{
		sameMSEntry(100, "b"), sameMSEntry(100, "c"), sameMSEntry(101, "d"),
	}
	disk := []clievent.EventEntry{
		sameMSEntry(90, "z"),
		sameMSEntry(100, "a"), sameMSEntry(100, "b"), sameMSEntry(100, "c"),
		sameMSEntry(101, "d"),
	}
	s.SetHistorySource(&pagingHistorySource{all: disk})

	const pageLimit = 3
	seen := map[string]bool{}
	var cursor int64
	var held []clievent.EventEntry
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatal("walk did not terminate")
		}
		before, limit := int64(0), pageLimit
		if page > 0 {
			before, limit = cursor+1, pageLimit+len(held)
		}
		raw := s.EventEntriesBeforeCtx(context.Background(), before, limit)
		requireEachOnce(t, raw, uuidsOf(raw)...)
		for _, e := range raw {
			seen[e.UUID] = true
		}
		if len(raw) < limit {
			break
		}
		next := raw[0].Time
		if next != cursor {
			held = nil
		}
		cursor = next
		held = appendAtTime(held, raw, cursor)
	}
	for _, u := range []string{"z", "a", "b", "c", "d"} {
		if !seen[u] {
			t.Errorf("uuid %q never reached by the load-earlier walk", u)
		}
	}
}

// A gap-fill turn at the seam millisecond is part of the memory page, so the
// disk copy of it (merged.Source returns the same fallback turn) is a dup.
func TestEventEntriesBeforeCtx_GapFillTurnAtSeamNotDuplicated(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	s.persistedHistory = []clievent.EventEntry{sameMSEntry(100, "b"), sameMSEntry(101, "d")}
	gf := []clievent.EventEntry{sameMSEntry(100, "g")}
	s.gapFillCell().turns.Store(&gf)
	s.SetHistorySource(&pagingHistorySource{all: []clievent.EventEntry{
		sameMSEntry(90, "z"), sameMSEntry(100, "g"), sameMSEntry(100, "b"), sameMSEntry(101, "d"),
	}})

	got := s.EventEntriesBeforeCtx(context.Background(), 0, 10)
	requireEachOnce(t, got, "z", "g", "b", "d")
}

// Uuid-less entries match by content at the seam: the disk copy of the
// memory one is dropped, a distinct one in the same millisecond is kept.
func TestEventEntriesBeforeCtx_UUIDLessSeamDedupByContent(t *testing.T) {
	t.Parallel()
	s := &ManagedSession{key: "k"}
	mine := clievent.EventEntry{Time: 100, Type: "text", Summary: "x", Detail: "same"}
	other := clievent.EventEntry{Time: 100, Type: "text", Summary: "y", Detail: "other"}
	s.persistedHistory = []clievent.EventEntry{mine}
	s.SetHistorySource(&pagingHistorySource{all: []clievent.EventEntry{other, mine}})

	got := s.EventEntriesBeforeCtx(context.Background(), 0, 10)
	count := map[string]int{}
	for _, e := range got {
		count[e.Summary]++
	}
	if count["x"] != 1 || count["y"] != 1 || len(got) != 2 {
		t.Errorf("got %+v, want x and y once each", got)
	}
}

// The visible-aware initial read crosses a seam twice: memory to disk, and
// one disk page to the next. A three-entry millisecond split by either must
// come back whole, each entry once.
func TestEventLastNVisibleCtx_SameMSGroupAtSeams(t *testing.T) {
	t.Parallel()
	group := []clievent.EventEntry{sameMSEntry(300, "a"), sameMSEntry(300, "b"), sameMSEntry(300, "c")}
	cases := []struct {
		name string
		mem  []clievent.EventEntry
		disk []clievent.EventEntry
	}{{
		name: "memory to disk",
		mem:  group[1:],
		disk: append(internalRun(1, 10), group...),
	}, {
		// The first page (visibleDiskPageSize) takes the 198 newer entries
		// plus b and c; a is left for the second page.
		name: "disk page to disk page",
		disk: append(append(internalRun(1, 10), group...), internalRun(301, visibleDiskPageSize-2)...),
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &ManagedSession{key: "k"}
			s.persistedHistory = tc.mem
			s.SetHistorySource(&pagingHistorySource{all: tc.disk})
			got := s.EventLastNVisibleCtx(context.Background(), DefaultVisibleTarget, maxVisibleTotal)
			requireEachOnce(t, got, uuidsOf(append(tc.disk, tc.mem...))...)
		})
	}
}

// The has-more probe must see an older sibling that shares the slice's
// earliest millisecond, and must not mistake the spool's copies of the
// slice's own entries for older history.
func TestEventInitialPageCtx_HasMoreAtSameMS(t *testing.T) {
	t.Parallel()
	mem := []clievent.EventEntry{sameMSEntry(100, "b"), sameMSEntry(100, "c")}
	for i := 0; i < DefaultVisibleTarget; i++ {
		mem = append(mem, sameMSEntry(int64(101+i), fmt.Sprintf("m%d", i)))
	}
	cases := []struct {
		name string
		disk []clievent.EventEntry
		want bool
	}{
		{"older sibling on disk", []clievent.EventEntry{sameMSEntry(100, "a"), mem[0], mem[1]}, true},
		{"disk mirrors the slice", []clievent.EventEntry{mem[0], mem[1]}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &ManagedSession{key: "k"}
			// Exactly DefaultVisibleTarget visible entries: memory alone
			// satisfies the read and the slice starts at b.
			s.persistedHistory = append([]clievent.EventEntry(nil), mem[:DefaultVisibleTarget]...)
			s.SetHistorySource(&pagingHistorySource{all: tc.disk})
			entries, hasMore := s.EventInitialPageCtx(context.Background(), DefaultVisibleTarget, maxVisibleTotal)
			if len(entries) == 0 || entries[0].UUID != "b" {
				t.Fatalf("slice starts at %s, want b", uuidList(entries[:min(3, len(entries))]))
			}
			if hasMore != tc.want {
				t.Errorf("hasMore=%v want %v", hasMore, tc.want)
			}
		})
	}
}
