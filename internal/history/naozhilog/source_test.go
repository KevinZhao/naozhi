package naozhilog

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/eventlog/persist"
)

// newPersister is a small wrapper that spins up a Persister pointed
// at t.TempDir() and wires it up for a single key. Returns the
// (persister, naozhi-log source, sink) triple so tests can produce
// data via the sink and read it back via the source — the same
// round-trip the router does in production.
func newPersister(t *testing.T, key string) (*persist.Persister, *Source, persist.PersistSink, string) {
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
	return p, New(dir, key), p.SinkFor(key), dir
}

// persistOne is a test-only adapter that does what session.Router's
// bridge will do in Phase 4: marshal a cli.EventEntry into
// persist.Entry and hand it to the sink.
func persistOne(t *testing.T, sink persist.PersistSink, e cli.EventEntry) {
	t.Helper()
	buf, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sink([]persist.Entry{{JSON: buf, TimeMS: e.Time}}, false)
}

// TestSource_LoadLatest_EmptyDir returns nil without error for a key
// that has never been written to. The router relies on this to
// distinguish "no local history" from an actual failure.
func TestSource_LoadLatest_EmptyDir(t *testing.T) {
	_, src, _, _ := newPersister(t, "k")
	got, err := src.LoadLatest(context.Background(), 500)
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0", len(got))
	}
}

// TestSource_LoadLatest_RoundTrip writes N entries through the
// persister and reads them back via the Source. Every field that
// matters for dashboard rendering (UUID, Time, Type, Summary,
// Images, ImagePaths) must survive the trip.
func TestSource_LoadLatest_RoundTrip(t *testing.T) {
	p, src, sink, _ := newPersister(t, "k")

	inputs := []cli.EventEntry{
		{UUID: "aaaa11", Time: 100, Type: "user", Summary: "hi"},
		{UUID: "bbbb22", Time: 200, Type: "text", Summary: "hello back"},
		{
			UUID: "cccc33", Time: 300, Type: "user", Summary: "look",
			Images:     []string{"data:image/jpeg;base64,AAA="},
			ImagePaths: []string{".naozhi/attachments/2026-05-10/x.jpg"},
		},
	}
	for _, e := range inputs {
		persistOne(t, sink, e)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.Flush(ctx)

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
		if len(got[i].Images) != len(want.Images) {
			t.Errorf("entry[%d] image count mismatch", i)
		}
		if len(got[i].ImagePaths) != len(want.ImagePaths) {
			t.Errorf("entry[%d] image_paths count mismatch", i)
		}
	}
}

// TestSource_LoadLatest_RespectsLimit: 10 entries, limit=3, returns
// the newest 3 in chronological order.
func TestSource_LoadLatest_RespectsLimit(t *testing.T) {
	p, src, sink, _ := newPersister(t, "k")
	for i := 0; i < 10; i++ {
		persistOne(t, sink, cli.EventEntry{
			UUID: "u" + rune2hex(i),
			Time: int64(100 + i),
			Type: "user",
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.Flush(ctx)

	got, err := src.LoadLatest(context.Background(), 3)
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	// Newest 3 by Time are 107, 108, 109.
	for i, want := range []int64{107, 108, 109} {
		if got[i].Time != want {
			t.Errorf("got[%d].Time=%d, want %d", i, got[i].Time, want)
		}
	}
}

// TestSource_LoadBefore_FiltersAndOrders: 10 entries times 100..109,
// LoadBefore(beforeMS=105) returns the 5 entries strictly < 105
// (100..104) in chronological order.
func TestSource_LoadBefore_FiltersAndOrders(t *testing.T) {
	p, src, sink, _ := newPersister(t, "k")
	for i := 0; i < 10; i++ {
		persistOne(t, sink, cli.EventEntry{
			UUID: "u" + rune2hex(i),
			Time: int64(100 + i),
			Type: "user",
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.Flush(ctx)

	got, err := src.LoadBefore(context.Background(), 105, 100)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	wantTimes := []int64{100, 101, 102, 103, 104}
	if len(got) != len(wantTimes) {
		t.Fatalf("got %d entries, want %d", len(got), len(wantTimes))
	}
	for i, w := range wantTimes {
		if got[i].Time != w {
			t.Errorf("got[%d].Time=%d, want %d", i, got[i].Time, w)
		}
	}
}

// TestSource_LoadBefore_RespectsLimit returns the newest `limit`
// filtered entries, not the oldest — users paginating "load earlier"
// want adjacent-to-screen entries first.
func TestSource_LoadBefore_RespectsLimit(t *testing.T) {
	p, src, sink, _ := newPersister(t, "k")
	for i := 0; i < 10; i++ {
		persistOne(t, sink, cli.EventEntry{
			UUID: "u" + rune2hex(i),
			Time: int64(100 + i),
			Type: "user",
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.Flush(ctx)

	got, err := src.LoadBefore(context.Background(), 108, 3)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	// Candidates (<108) are 100..107, newest 3 are 105,106,107.
	wantTimes := []int64{105, 106, 107}
	if len(got) != len(wantTimes) {
		t.Fatalf("got %d entries, want %d", len(got), len(wantTimes))
	}
	for i, w := range wantTimes {
		if got[i].Time != w {
			t.Errorf("got[%d].Time=%d, want %d", i, got[i].Time, w)
		}
	}
}

// TestSource_LoadBefore_ZeroBeforeMS degenerates to LoadLatest so
// the dashboard's "first page" and "paginated" call sites share
// one code path on the read side.
func TestSource_LoadBefore_ZeroBeforeMS(t *testing.T) {
	p, src, sink, _ := newPersister(t, "k")
	for i := 0; i < 5; i++ {
		persistOne(t, sink, cli.EventEntry{
			UUID: "u" + rune2hex(i),
			Time: int64(100 + i),
			Type: "user",
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.Flush(ctx)

	got, err := src.LoadBefore(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("got %d, want 5", len(got))
	}
}

// TestSource_DisabledConfig: empty dir returns nil without error.
// Used by deployments that opt out of event-log persistence (e.g.
// stateless test harnesses).
func TestSource_DisabledConfig(t *testing.T) {
	s := New("", "k")
	got, err := s.LoadLatest(context.Background(), 100)
	if err != nil {
		t.Fatalf("LoadLatest on empty dir: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d, want 0", len(got))
	}
}

// TestSource_ContextCancel surfaces ctx cancellation promptly. The
// HTTP handler that drives this uses request-scoped ctx, so a
// client-side abort MUST stop the read.
func TestSource_ContextCancel(t *testing.T) {
	p, src, sink, _ := newPersister(t, "k")
	for i := 0; i < 100; i++ {
		persistOne(t, sink, cli.EventEntry{
			UUID: "u" + rune2hex(i),
			Time: int64(i),
			Type: "user",
		})
	}
	flushCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.Flush(flushCtx)

	ctx, cancelRead := context.WithCancel(context.Background())
	cancelRead() // cancel immediately
	_, err := src.LoadLatest(ctx, 100)
	if err == nil {
		t.Errorf("expected cancellation error, got nil")
	}
}

// rune2hex is a tiny helper so test entries can have distinct UUIDs
// without pulling crypto/rand into the test path.
func rune2hex(i int) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[i/16], hex[i%16]})
}
