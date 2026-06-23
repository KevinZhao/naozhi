package merged

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
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
	entries []cli.EventEntry
	err     error
}

func (s *stubSource) LoadBefore(_ context.Context, _ int64, _ int) ([]cli.EventEntry, error) {
	return s.entries, s.err
}

// TestMerged_LocalOnly: fallback empty → local is returned verbatim.
func TestMerged_LocalOnly(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
			{UUID: "aa", Time: 1, Summary: "first"},
			{UUID: "bb", Time: 2, Summary: "second"},
		}},
		Fallback: &stubSource{},
	}
	got, err := m.LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	if got[0].UUID != "aa" || got[1].UUID != "bb" {
		t.Errorf("ordering changed: %+v", got)
	}
}

// TestMerged_FallbackOnly: the upgrade path — local empty, fallback
// carries the history. Result must be fallback exactly.
func TestMerged_FallbackOnly(t *testing.T) {
	m := &Source{
		Local: &stubSource{},
		Fallback: &stubSource{entries: []cli.EventEntry{
			{UUID: "aa", Time: 1, Summary: "old1"},
			{UUID: "bb", Time: 2, Summary: "old2"},
		}},
	}
	got, err := m.LoadBefore(context.Background(), 0, 100)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

// TestMerged_OverlapDedup: same UUID present in both. Local wins
// (local has richer fields than Claude JSONL). Critical for the
// upgrade path when both tiers overlap.
func TestMerged_OverlapDedup(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
			{UUID: "shared", Time: 1, Summary: "local-version", Images: []string{"data:image/jpeg;base64,XYZ="}},
		}},
		Fallback: &stubSource{entries: []cli.EventEntry{
			{UUID: "shared", Time: 1, Summary: "fallback-version"},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (dedup by UUID)", len(got))
	}
	if got[0].Summary != "local-version" {
		t.Errorf("local did not win dedup: %q", got[0].Summary)
	}
	if len(got[0].Images) != 1 {
		t.Errorf("local's Images field dropped during dedup")
	}
}

// TestMerged_DistinctUUIDsAtSameTime: two genuinely different events
// at the same Time must NOT be deduped. Dashboard relies on this:
// users who press "ok" twice in the same millisecond see both
// bubbles.
func TestMerged_DistinctUUIDsAtSameTime(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
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
		Local: &stubSource{entries: []cli.EventEntry{
			{UUID: "aa", Time: 100},
			{UUID: "bb", Time: 200},
		}},
		Fallback: &stubSource{entries: []cli.EventEntry{
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

// TestMerged_SortedByTime: even when inputs are unsorted, the output
// must be sorted ascending by Time.
func TestMerged_SortedByTime(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
			{UUID: "z", Time: 300},
			{UUID: "a", Time: 100},
		}},
		Fallback: &stubSource{entries: []cli.EventEntry{
			{UUID: "m", Time: 200},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time < got[i-1].Time {
			t.Errorf("not sorted: %+v", got)
			break
		}
	}
}

// TestMerged_LimitTailKept: when the merged result exceeds limit,
// the NEWEST limit entries are kept. Dashboard pagination wants
// recent-first adjacency.
func TestMerged_LimitTailKept(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
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
		Fallback: &stubSource{entries: []cli.EventEntry{{UUID: "a", Time: 1}}},
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
		Local:    &stubSource{entries: []cli.EventEntry{{UUID: "a", Time: 1}}},
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

// TestMerged_EmptyUUID_Kept: legacy entries (no UUID) aren't deduped
// by UUID but must still be kept. Shows up on first upgrade before
// DeriveLegacyUUID is wired through discovery.
func TestMerged_EmptyUUID_Kept(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
			{UUID: "", Time: 1, Summary: "legacy-a"},
		}},
		Fallback: &stubSource{entries: []cli.EventEntry{
			{UUID: "", Time: 2, Summary: "legacy-b"},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 2 {
		t.Errorf("got %d, want 2 (empty UUIDs not deduped)", len(got))
	}
}

// TestMerged_NilSources: either side may be nil. MergedSource
// tolerates a router that hasn't finished wiring one tier.
func TestMerged_NilSources(t *testing.T) {
	m := &Source{
		Fallback: &stubSource{entries: []cli.EventEntry{
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

// TestMerged_FastPathSortedInputs exercises the 2-way merge fast
// path: both sources return chronological slices (the contract).
// Asserts interleaved ordering + UUID-based dedup in the same pass.
// Regression guard: if mergeSorted accidentally drops `seen` seeding
// from local this test fails on the dedup check.
func TestMerged_FastPathSortedInputs(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
			{UUID: "aa", Time: 100, Summary: "l-aa"},
			{UUID: "cc", Time: 300, Summary: "l-cc"},
		}},
		Fallback: &stubSource{entries: []cli.EventEntry{
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
		if e.UUID == "cc" && e.Summary != "l-cc" {
			t.Errorf("dedup did not keep local version for cc: got %q", e.Summary)
		}
	}
}

// TestMerged_FastPathFallbackWithEmptyUUID: empty-UUID entries from
// fallback must pass through the merge without being swallowed by
// the dedup map lookup. Covers the legacy-JSONL entry path that
// survives the migration window.
func TestMerged_FastPathFallbackWithEmptyUUID(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
			{UUID: "aa", Time: 100},
		}},
		Fallback: &stubSource{entries: []cli.EventEntry{
			{UUID: "", Time: 50, Summary: "legacy"},
			{UUID: "", Time: 150, Summary: "legacy2"},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 0, 100)
	if len(got) != 3 {
		t.Fatalf("got %d, want 3 (empty-UUID fallback kept)", len(got))
	}
}

// TestMerged_SlowPathUnsortedInputs forces the defensive path by
// feeding descending-time entries. Exercises mergeSortFallback. The
// existing TestMerged_SortedByTime also covers this, but an explicit
// test with reversed inputs pins the intent in case someone later
// removes the runtime check.
func TestMerged_SlowPathUnsortedInputs(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
			{UUID: "z", Time: 500}, // descending — triggers repair
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

// TestMerged_FastPathBeforeMSFilter covers beforeMS filtering inside
// the fast path — the filter used to live in a single loop, now lives
// inside emit() which is called from both branches of the 2-way merge.
// Missing the filter in either branch would leak "too new" entries.
func TestMerged_FastPathBeforeMSFilter(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
			{UUID: "a", Time: 100},
			{UUID: "c", Time: 300},
		}},
		Fallback: &stubSource{entries: []cli.EventEntry{
			{UUID: "b", Time: 200},
			{UUID: "d", Time: 400},
		}},
	}
	got, _ := m.LoadBefore(context.Background(), 250, 100)
	for _, e := range got {
		if e.Time >= 250 {
			t.Errorf("entry Time=%d leaked past beforeMS=250", e.Time)
		}
	}
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
}

// TestMerged_AboveCutoffLocalDoesNotEvictFallback [R100110-EDGE-2]:
// a local entry at/above beforeMS is dropped by the emit filter, so it
// must NOT seed the dedup set. Otherwise a same-UUID fallback entry that
// is BELOW the cutoff — a legitimately visible backfill — would be
// silently deduped away, losing one row of visible history. Inputs are
// pre-sorted so the fast-path mergeSorted runs.
func TestMerged_AboveCutoffLocalDoesNotEvictFallback(t *testing.T) {
	m := &Source{
		Local: &stubSource{entries: []cli.EventEntry{
			// Same UUID "x" as the fallback entry, but Time >= beforeMS:
			// emit drops it, so it must not occupy a `seen` slot.
			{UUID: "x", Time: 300, Summary: "local-above-cutoff"},
		}},
		Fallback: &stubSource{entries: []cli.EventEntry{
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

// generalMergeSorted is the pre-fast-path reference implementation of
// mergeSorted: it always seeds `seen` from local and runs the full
// two-way merge regardless of whether a tier is empty. The fast-path
// branches in mergeSorted must produce element-for-element identical
// output to this on the one-tier-empty inputs below.
func generalMergeSorted(local, fallback []cli.EventEntry, beforeMS int64) []cli.EventEntry {
	seen := make(map[string]struct{}, len(local))
	for _, e := range local {
		if e.UUID != "" && (beforeMS <= 0 || e.Time < beforeMS) {
			seen[e.UUID] = struct{}{}
		}
	}
	out := make([]cli.EventEntry, 0, len(local)+len(fallback))
	emit := func(e cli.EventEntry) {
		if beforeMS > 0 && e.Time >= beforeMS {
			return
		}
		out = append(out, e)
	}
	i, j := 0, 0
	for i < len(local) && j < len(fallback) {
		if entryCmp(local[i], fallback[j]) <= 0 {
			emit(local[i])
			i++
			continue
		}
		f := fallback[j]
		if f.UUID == "" {
			emit(f)
		} else if _, dup := seen[f.UUID]; !dup {
			seen[f.UUID] = struct{}{}
			emit(f)
		}
		j++
	}
	for ; i < len(local); i++ {
		emit(local[i])
	}
	for ; j < len(fallback); j++ {
		f := fallback[j]
		if f.UUID == "" {
			emit(f)
			continue
		}
		if _, dup := seen[f.UUID]; dup {
			continue
		}
		seen[f.UUID] = struct{}{}
		emit(f)
	}
	return out
}

func eventsEqual(a, b []cli.EventEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].UUID != b[i].UUID || a[i].Time != b[i].Time || a[i].Summary != b[i].Summary {
			return false
		}
	}
	return true
}

// TestMergeSorted_FastPathFallbackEmpty_EquivalentToGeneral pins the
// [R202606b-PERF-003] fast path (len(fallback)==0): output must equal the
// reference two-way-merge implementation element-for-element across
// cutoff boundaries and empty-UUID entries.
func TestMergeSorted_FastPathFallbackEmpty_EquivalentToGeneral(t *testing.T) {
	local := []cli.EventEntry{
		{UUID: "a", Time: 100, Summary: "a"},
		{UUID: "", Time: 150, Summary: "legacy"}, // empty UUID kept
		{UUID: "b", Time: 200, Summary: "b"},
		{UUID: "c", Time: 300, Summary: "c"},
	}
	for _, beforeMS := range []int64{0, -1, 150, 200, 250, 1000} {
		fast := mergeSorted(local, nil, beforeMS)
		ref := generalMergeSorted(local, nil, beforeMS)
		if !eventsEqual(fast, ref) {
			t.Errorf("beforeMS=%d: fast=%+v ref=%+v", beforeMS, fast, ref)
		}
	}
	// empty local + empty fallback degenerate case
	if got := mergeSorted(nil, nil, 0); len(got) != 0 {
		t.Errorf("empty/empty: got %+v, want nil", got)
	}
}

// TestMergeSorted_FastPathLocalEmpty_EquivalentToGeneral pins the
// [R202606b-PERF-003] fast path (len(local)==0): fallback self-dedup
// (first UUID wins), empty-UUID pass-through, and cutoff filtering must
// match the reference implementation.
func TestMergeSorted_FastPathLocalEmpty_EquivalentToGeneral(t *testing.T) {
	fallback := []cli.EventEntry{
		{UUID: "a", Time: 100, Summary: "a"},
		{UUID: "", Time: 120, Summary: "legacy1"}, // empty UUID kept
		{UUID: "b", Time: 200, Summary: "b1"},
		{UUID: "b", Time: 200, Summary: "b2-dup"}, // self-dup, first wins
		{UUID: "", Time: 220, Summary: "legacy2"}, // empty UUID kept
		{UUID: "c", Time: 300, Summary: "c"},
	}
	for _, beforeMS := range []int64{0, -1, 120, 200, 201, 300, 1000} {
		fast := mergeSorted(nil, fallback, beforeMS)
		ref := generalMergeSorted(nil, fallback, beforeMS)
		if !eventsEqual(fast, ref) {
			t.Errorf("beforeMS=%d: fast=%+v ref=%+v", beforeMS, fast, ref)
		}
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
