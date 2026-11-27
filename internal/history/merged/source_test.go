package merged

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// captureSlog redirects the default slog logger to an in-memory buffer
// for the duration of the test, restoring the prior default on cleanup.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// stubSource is a minimal history.Source implementation — returns
// canned entries (optionally with an error). Used to pump predictable
// data through MergedSource without reaching disk.
type stubSource struct {
	entries []clievent.EventEntry
	err     error
}

func (s *stubSource) LoadBefore(_ context.Context, _ int64, _ int) ([]clievent.EventEntry, error) {
	return s.entries, s.err
}

// TestMerged_DistinctUUIDsAtSameTime: two genuinely different events
// at the same Time must NOT be deduped. Dashboard relies on this:
// users who press "ok" twice in the same millisecond see both
// bubbles.
func TestMerged_DistinctUUIDsAtSameTime(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			{UUID: "aa", Time: 100, Summary: "ok"},
			{UUID: "bb", Time: 100, Summary: "ok"},
		}},
		Fallback: &stubSource{},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 2 {
		t.Errorf("got %d, want 2 (distinct UUIDs at same Time)", len(got))
	}
}

// TestMerged_TimeBeforeFilter: entries >= beforeMS must be excluded.
// Prevents a timing drift between sources from returning "too new"
// entries when the caller asked for "< beforeMS".
func TestMerged_TimeBeforeFilter(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			{UUID: "aa", Time: 100},
			{UUID: "bb", Time: 200},
		}},
		Fallback: &stubSource{entries: []clievent.EventEntry{
			{UUID: "cc", Time: 150},
			{UUID: "dd", Time: 250},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 200, 100)
	for _, e := range got {
		if e.Time >= 200 {
			t.Errorf("entry with Time=%d leaked despite beforeMS=200", e.Time)
		}
	}
	// Should have aa(100) + cc(150) only.
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
}

// TestMerged_LimitTailKept: when the merged result exceeds limit,
// the NEWEST limit entries are kept. Dashboard pagination wants
// recent-first adjacency.
func TestMerged_LimitTailKept(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			{UUID: "a", Time: 1},
			{UUID: "b", Time: 2},
			{UUID: "c", Time: 3},
			{UUID: "d", Time: 4},
			{UUID: "e", Time: 5},
		}},
		Fallback: &stubSource{},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 2)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	if got[0].UUID != "d" || got[1].UUID != "e" {
		t.Errorf("wrong tail: %+v", got)
	}
}

// TestMerged_OneSourceErrors: an error from one side does not
// short-circuit — the other side's data still surfaces. Matches
// "fallback when local misbehaves" contract.
func TestMerged_OneSourceErrors(t *testing.T) {
	buf := captureSlog(t)
	m := &Source{
		Local:    &stubSource{err: errors.New("local disk full")},
		Fallback: &stubSource{entries: []clievent.EventEntry{{UUID: "a", Time: 1}}},
	}
	got, err := m.LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Errorf("merged surfaced error despite fallback having data: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d, want 1 from fallback", len(got))
	}
	// [R202606g-CR-001] operator must get a signal the local tier failed.
	if log := buf.String(); !strings.Contains(log, "local source failed") || !strings.Contains(log, "local disk full") {
		t.Errorf("expected WARN about local-source failure, got log: %q", log)
	}
}

// TestMerged_FallbackSourceErrors is the symmetric case: fallback fails,
// local has data. Result is local, and a WARN is logged. [R202606g-CR-001]
func TestMerged_FallbackSourceErrors(t *testing.T) {
	buf := captureSlog(t)
	m := &Source{
		Local:    &stubSource{entries: []clievent.EventEntry{{UUID: "a", Time: 1}}},
		Fallback: &stubSource{err: errors.New("jsonl unreadable")},
	}
	got, err := m.LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Errorf("merged surfaced error despite local having data: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d, want 1 from local", len(got))
	}
	if log := buf.String(); !strings.Contains(log, "fallback source failed") || !strings.Contains(log, "jsonl unreadable") {
		t.Errorf("expected WARN about fallback-source failure, got log: %q", log)
	}
}

// TestMerged_BothSourcesError: only when both fail do we return.
func TestMerged_BothSourcesError(t *testing.T) {
	m := &Source{
		Local:    &stubSource{err: errors.New("local")},
		Fallback: &stubSource{err: errors.New("fallback")},
	}
	_, err := m.LoadBefore(context.Background(), 0, 100)
	if err == nil {
		t.Errorf("expected error when both sides fail, got nil")
	}
}

// TestMerged_NilSources: either side may be nil. MergedSource
// tolerates a router that hasn't finished wiring one tier.
func TestMerged_NilSources(t *testing.T) {
	m := &Source{
		Fallback: &stubSource{entries: []clievent.EventEntry{
			{UUID: "a", Time: 1},
		}},
	}
	got, err := m.LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d, want 1 (nil local + populated fallback)", len(got))
	}

	m2 := &Source{}
	got2, err := m2.LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("all-nil LoadBefore: %v", err)
	}
	if len(got2) != 0 {
		t.Errorf("expected empty for all-nil sources, got %d", len(got2))
	}
}

// TestMerged_SortedInputs: both sources return chronological
// slices. Asserts interleaved ordering + UUID-based dedup in the same pass;
// fails on the dedup check if `seen` stops being seeded from local. Local
// wins a UUID dup with all its fields: it has richer ones (Images) than
// Claude's JSONL.
func TestMerged_SortedInputs(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			{UUID: "aa", Time: 100, Summary: "l-aa"},
			{UUID: "cc", Time: 300, Summary: "l-cc", Images: []string{"data:image/jpeg;base64,XYZ="}},
		}},
		Fallback: &stubSource{entries: []clievent.EventEntry{
			{UUID: "bb", Time: 200, Summary: "f-bb"},
			{UUID: "cc", Time: 300, Summary: "f-cc-dup"}, // dup of local
			{UUID: "dd", Time: 400, Summary: "f-dd"},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 4 {
		t.Fatalf("got %d, want 4", len(got))
	}
	// Interleave expected: aa(100), bb(200), cc(300)(local), dd(400).
	want := []string{"aa", "bb", "cc", "dd"}
	for i, e := range got {
		if e.UUID != want[i] {
			t.Errorf("pos %d: UUID=%q, want %q", i, e.UUID, want[i])
		}
	}
	for _, e := range got {
		if e.UUID == "cc" && (e.Summary != "l-cc" || len(e.Images) != 1) {
			t.Errorf("dedup did not keep local version for cc: got %+v", e)
		}
	}
}

// TestMerged_EmptyUUIDKept: legacy entries with no UUID, from either tier,
// must pass through the merge without being swallowed by the dedup map
// lookup. Covers the legacy-JSONL entry path that survives the migration
// window.
func TestMerged_EmptyUUIDKept(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			{UUID: "aa", Time: 100},
			{UUID: "", Time: 120, Summary: "legacy-local"},
		}},
		Fallback: &stubSource{entries: []clievent.EventEntry{
			{UUID: "", Time: 50, Summary: "legacy"},
			{UUID: "", Time: 150, Summary: "legacy2"},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 4 {
		t.Fatalf("got %d, want 4 (empty-UUID entries kept)", len(got))
	}
}

// TestMerged_UnsortedInputs feeds descending-time entries and
// asserts the output is re-sorted ascending.
func TestMerged_UnsortedInputs(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			{UUID: "z", Time: 500}, // descending
			{UUID: "y", Time: 300},
			{UUID: "x", Time: 100},
		}},
		Fallback: &stubSource{},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	// Repair must produce ascending Time.
	for i := 1; i < len(got); i++ {
		if got[i].Time < got[i-1].Time {
			t.Errorf("repair did not reorder: %+v", got)
			break
		}
	}
	if got[0].UUID != "x" || got[2].UUID != "z" {
		t.Errorf("repair order wrong: got %v", got)
	}
}

// TestMerged_AboveCutoffLocalDoesNotEvictFallback [R100110-EDGE-2]:
// a local entry at/above beforeMS is dropped by the emit filter, so it
// must NOT seed the dedup set. Otherwise a same-UUID fallback entry that
// is BELOW the cutoff — a legitimately visible backfill — would be
// silently deduped away, losing one row of visible history.
func TestMerged_AboveCutoffLocalDoesNotEvictFallback(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			// Same UUID "x" as the fallback entry, but Time >= beforeMS:
			// emit drops it, so it must not occupy a `seen` slot.
			{UUID: "x", Time: 300, Summary: "local-above-cutoff"},
		}},
		Fallback: &stubSource{entries: []clievent.EventEntry{
			{UUID: "x", Time: 100, Summary: "fallback-below-cutoff"},
		}},
	}
	got, err := m.LoadBefore(context.Background(), 200, 100)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (below-cutoff fallback must survive)", len(got))
	}
	if got[0].UUID != "x" || got[0].Summary != "fallback-below-cutoff" {
		t.Errorf("expected visible fallback backfill, got %+v", got[0])
	}
}

// TestMerged_CrossSourceContentDedup_DistinctNotCollapsed: entries that
// share Time+Type+Summary but DIFFER in Detail must NOT be deduped —
// Detail is the discriminator that keeps genuinely distinct turns apart.
func TestMerged_CrossSourceContentDedup_DistinctNotCollapsed(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []clievent.EventEntry{
			{UUID: "u1", Time: 100, Type: "text", Summary: "reply", Detail: "answer A"},
		}},
		Fallback: &stubSource{entries: []clievent.EventEntry{
			{UUID: "u2", Time: 100, Type: "text", Summary: "reply", Detail: "answer B"},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 2 {
		t.Errorf("got %d, want 2 (different Detail must not collapse)", len(got))
	}
}

// TestMerged_SingleTier_DedupAndCutoff pins the single-tier shapes: with
// local empty, fallback still self-dedups by UUID (first occurrence wins,
// including across an intervening empty-UUID entry: `b, <empty>, b`) and
// honours the cutoff; with fallback empty, local is filtered by the cutoff.
func TestMerged_SingleTier_DedupAndCutoff(t *testing.T) {
	m := &Source{
		Local: &stubSource{},
		Fallback: &stubSource{entries: []clievent.EventEntry{
			{UUID: "a", Time: 100},
			{UUID: "b", Time: 200, Summary: "b1"},
			{UUID: "b", Time: 200, Summary: "b2-dup"},
			{UUID: "", Time: 210, Summary: "legacy"},
			{UUID: "b", Time: 220, Summary: "b3-dup"},
			{UUID: "c", Time: 300},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 250, 100)
	if want := []string{"a", "b", ""}; !slices.Equal(uuids(got), want) {
		t.Errorf("local empty: got %v, want %v", uuids(got), want)
	} else if got[1].Summary != "b1" {
		t.Errorf("local empty: first b must win, got %q", got[1].Summary)
	}

	m = &Source{
		Local:    &stubSource{entries: []clievent.EventEntry{{UUID: "x", Time: 100}, {UUID: "y", Time: 300}}},
		Fallback: &stubSource{},
	}
	got, _ = m.LoadBefore(context.Background(), 200, 100)
	if want := []string{"x"}; !slices.Equal(uuids(got), want) {
		t.Errorf("fallback empty: got %v, want %v", uuids(got), want)
	}
}

// TestMerged_NilReceiver: methods on nil receiver don't panic. The
// router's attachHistorySource may install MergedSource as nil on
// older sessions that opt out.
func TestMerged_NilReceiver(t *testing.T) {
	var m *Source
	got, err := m.LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("nil-receiver error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("nil-receiver returned data: %+v", got)
	}
}
