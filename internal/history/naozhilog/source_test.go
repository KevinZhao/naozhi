package naozhilog

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

// newPersister is a small wrapper that spins up a Persister pointed
// at t.TempDir() and wires it up for a single key. Returns the
// (persister, naozhi-log source, sink) triple so tests can produce
// data via the sink and read it back via the source — the same
// round-trip the router does in production.
func newPersister(t *testing.T, key string) (*persist.Persister, *Source, persist.PersistSink) {
	t.Helper()
	dir := t.TempDir()
	p, err := persist.NewPersister(persist.Options{
		Dir:           dir,
		IdxStride:     2,
		FlushInterval: 20 * time.Millisecond,
		ChannelBuffer: 128,
		Generator:     "naozhilog-test",
	})
	if err != nil {
		t.Fatalf("NewPersister: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	})
	return p, New(dir, key), p.SinkFor(key)
}

// persistOne is a test-only adapter that does what session.Router's
// bridge does: marshal a clievent.EventEntry into persist.Entry and hand
// it to the sink.
func persistOne(t *testing.T, sink persist.PersistSink, e clievent.EventEntry) {
	t.Helper()
	buf, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sink([]persist.Entry{{JSON: buf, TimeMS: e.Time}}, false)
}

// flush waits for the persister to write what the sink was handed.
func flush(p *persist.Persister) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.Flush(ctx)
}

// userLog persists n user entries at Time first, first+step, … and returns
// the source reading them back.
func userLog(t *testing.T, key string, n int, first, step int64) *Source {
	t.Helper()
	p, src, sink := newPersister(t, key)
	for i := 0; i < n; i++ {
		persistOne(t, sink, clievent.EventEntry{UUID: key + "-" + rune2hex(i), Time: first + int64(i)*step, Type: "user", Summary: rune2hex(i)})
	}
	flush(p)
	return src
}

// TestSource_EmptyOrDisabled: a key never written to, and a Source with no
// dir (deployments that opt out of event-log persistence), return nil
// without error. The router relies on this to distinguish "no local
// history" from an actual failure.
func TestSource_EmptyOrDisabled(t *testing.T) {
	_, src, _ := newPersister(t, "k")
	for name, s := range map[string]*Source{"never written": src, "no dir": New("", "k")} {
		got, err := s.LoadLatest(context.Background(), 500)
		if err != nil || len(got) != 0 {
			t.Errorf("%s: LoadLatest = (%d entries, %v), want (0, nil)", name, len(got), err)
		}
	}
}

// TestSource_LoadLatest_RoundTrip writes N entries through the
// persister and reads them back via the Source. Every field that
// matters for dashboard rendering (UUID, Time, Type, Summary,
// Images, ImagePaths) and the RunID join key must survive the trip.
func TestSource_LoadLatest_RoundTrip(t *testing.T) {
	p, src, sink := newPersister(t, "k")

	inputs := []clievent.EventEntry{
		{UUID: "aaaa11", Time: 100, Type: "user", Summary: "hi", RunID: "0123456789abcdef"},
		{UUID: "bbbb22", Time: 200, Type: "text", Summary: "hello back"},
		{UUID: "dddd44", Time: 250, Type: "result", RunID: "0123456789abcdef"},
		{
			UUID: "cccc33", Time: 300, Type: "user", Summary: "look",
			Images:     []string{"data:image/jpeg;base64,AAA="},
			ImagePaths: []string{".naozhi/attachments/2026-05-10/x.jpg"},
		},
	}
	for _, e := range inputs {
		persistOne(t, sink, e)
	}
	flush(p)

	got, err := src.LoadLatest(context.Background(), 100)
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if len(got) != len(inputs) {
		t.Fatalf("got %d entries, want %d", len(got), len(inputs))
	}
	for i, want := range inputs {
		if got[i].UUID != want.UUID {
			t.Errorf("entry[%d].UUID=%q, want %q", i, got[i].UUID, want.UUID)
		}
		if got[i].Time != want.Time {
			t.Errorf("entry[%d].Time=%d, want %d", i, got[i].Time, want.Time)
		}
		if got[i].Summary != want.Summary {
			t.Errorf("entry[%d].Summary=%q, want %q", i, got[i].Summary, want.Summary)
		}
		if got[i].RunID != want.RunID {
			t.Errorf("entry[%d].RunID=%q, want %q", i, got[i].RunID, want.RunID)
		}
		if len(got[i].Images) != len(want.Images) {
			t.Errorf("entry[%d] image count mismatch", i)
		}
		if len(got[i].ImagePaths) != len(want.ImagePaths) {
			t.Errorf("entry[%d] image_paths count mismatch", i)
		}
	}
}

// TestSource_Pages: over 10 entries at 100..109, a page is the newest
// `limit` entries strictly before beforeMS in chronological order — users
// paginating "load earlier" want the entries adjacent to the screen first.
// beforeMS 0 is LoadLatest, so the first page and the later ones share one
// read path.
func TestSource_Pages(t *testing.T) {
	src := userLog(t, "k", 10, 100, 1)
	bg := context.Background()
	latest, err := src.LoadLatest(bg, 3)
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if got := times(latest); !slices.Equal(got, []int64{107, 108, 109}) {
		t.Errorf("LoadLatest(3) = %v, want the newest 3", got)
	}
	for _, tc := range []struct {
		before int64
		limit  int
		want   []int64
	}{
		{105, 100, []int64{100, 101, 102, 103, 104}},
		{108, 3, []int64{105, 106, 107}},
		{0, 10, []int64{100, 101, 102, 103, 104, 105, 106, 107, 108, 109}},
	} {
		got, err := src.LoadBefore(bg, tc.before, tc.limit)
		if err != nil {
			t.Fatalf("LoadBefore(%d, %d): %v", tc.before, tc.limit, err)
		}
		if !slices.Equal(times(got), tc.want) {
			t.Errorf("LoadBefore(%d, %d) = %v, want %v", tc.before, tc.limit, times(got), tc.want)
		}
	}
}

// TestSource_LoadBefore_IdxSeekMatchesFullScan is the R20260530-PERF-1
// (#1485) regression: the idx-seek fast path must return the same page as
// the full-scan fallback. The log is large enough that the sparse idx
// (IdxStride=2 from newPersister) has many entries; records 10 ms apart let
// the sweep put beforeMS exactly on a record, between two and past all of
// them, the binary-search boundary cases of R112714-PERF-12. A drift between
// the two — e.g. an off-by-one in the back-up-by-slots seek math — fails
// here.
func TestSource_LoadBefore_IdxSeekMatchesFullScan(t *testing.T) {
	src := userLog(t, "k", 60, 1000, 10) // 1000, 1010, …, 1590

	bg := context.Background()
	for _, before := range []int64{1000, 1001, 1050, 1099, 1100, 1205, 1370, 1555, 1591, 9999} {
		for _, limit := range []int{1, 3, 10, 100} {
			idxGot, ok, err := src.loadBeforeViaIdx(bg, before, limit)
			if err != nil {
				t.Fatalf("loadBeforeViaIdx(before=%d,limit=%d): %v", before, limit, err)
			}
			fullGot, err := src.loadBeforeFullScan(bg, before, limit)
			if err != nil {
				t.Fatalf("loadBeforeFullScan(before=%d,limit=%d): %v", before, limit, err)
			}
			// When the idx path declines (ok==false) the public LoadBefore
			// would use the full scan, so only compare when idx produced a
			// result. Either way the public result must equal full scan.
			if ok && !equalTimes(idxGot, fullGot) {
				t.Errorf("idx vs full mismatch before=%d limit=%d: idx=%v full=%v",
					before, limit, times(idxGot), times(fullGot))
			}
			pub, err := src.LoadBefore(bg, before, limit)
			if err != nil {
				t.Fatalf("LoadBefore(before=%d,limit=%d): %v", before, limit, err)
			}
			if !equalTimes(pub, fullGot) {
				t.Errorf("public LoadBefore vs full mismatch before=%d limit=%d: pub=%v full=%v",
					before, limit, times(pub), times(fullGot))
			}
		}
	}
}

func times(es []clievent.EventEntry) []int64 {
	out := make([]int64, len(es))
	for i, e := range es {
		out[i] = e.Time
	}
	return out
}

func equalTimes(a, b []clievent.EventEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Time != b[i].Time || a[i].UUID != b[i].UUID {
			return false
		}
	}
	return true
}

// TestSource_ContextCancel surfaces ctx cancellation promptly. The
// HTTP handler that drives this uses request-scoped ctx, so a
// client-side abort MUST stop the read.
func TestSource_ContextCancel(t *testing.T) {
	src := userLog(t, "k", 100, 0, 1)
	ctx, cancelRead := context.WithCancel(context.Background())
	cancelRead() // cancel immediately
	if _, err := src.LoadLatest(ctx, 100); err == nil {
		t.Errorf("expected cancellation error, got nil")
	}
}

// TestLoadBefore_NoSliceAlias: neither the full-scan nor the idx loader may
// hand back a slice sharing its backing array with what it read from disk:
// mutating the returned slice must not affect the next read
// (R20260603-CR-3).
func TestLoadBefore_NoSliceAlias(t *testing.T) {
	src := userLog(t, "alias", 30, 200, 1) // enough idx entries (IdxStride=2) for the idx path
	bg := context.Background()
	loaders := map[string]func() ([]clievent.EventEntry, bool, error){
		"full scan": func() ([]clievent.EventEntry, bool, error) {
			es, err := src.loadBeforeFullScan(bg, 220, 5)
			return es, true, err
		},
		"via idx": func() ([]clievent.EventEntry, bool, error) { return src.loadBeforeViaIdx(bg, 220, 5) },
	}
	for name, load := range loaders {
		got1, ok, err := load()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !ok || len(got1) == 0 {
			t.Errorf("%s: no page (ok=%v), want one to check", name, ok)
			continue
		}
		const sentinel = int64(9999)
		got1[0].Time = sentinel
		got2, _, err := load()
		if err != nil || len(got2) == 0 {
			t.Fatalf("%s second call: %d entries, %v", name, len(got2), err)
		}
		if got2[0].Time == sentinel {
			t.Errorf("%s: mutation of returned slice affected source data — slice alias detected (CR-3)", name)
		}
	}
}

// rune2hex is a tiny helper so test entries can have distinct UUIDs
// without pulling crypto/rand into the test path.
func rune2hex(i int) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[i/16], hex[i%16]})
}

// TestDecode_KeepsUUIDLessEntries: persist's gap record is UUID-less by
// design, and a producer bug can leave others without one. decodeRecords
// (shared by the full-scan and idx paths) must keep them: dropping would lose
// the gap marker and history.
func TestDecode_KeepsUUIDLessEntries(t *testing.T) {
	p, src, sink := newPersister(t, "k")
	for _, e := range []clievent.EventEntry{
		{UUID: "a", Time: 1000, Type: "user", Summary: "q"},
		{Time: 2000, Type: clievent.KindPersistGap, Detail: "dropped=3"},
		{Time: 3000, Type: "text", Summary: "producer bug 1"},
		{Time: 4000, Type: "text", Summary: "producer bug 2"},
	} {
		persistOne(t, sink, e)
	}
	flush(p)

	got, err := src.LoadLatest(context.Background(), 100)
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	var gaps, uuidless int
	for _, e := range got {
		if e.UUID == "" {
			uuidless++
			if e.Type == clievent.KindPersistGap {
				gaps++
			}
		}
	}
	if gaps != 1 || uuidless != 3 {
		t.Errorf("%d gap records and %d UUID-less entries, want 1 and 3", gaps, uuidless)
	}
}
