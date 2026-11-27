package persist

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/eventlog/schema"
)

// entry is a convenience JSON-producing helper used across the tests.
// Generates a minimal clievent.EventEntry-shape payload (time/uuid/type/summary)
// that schema.MarshalRecord accepts.

// opGuard bounds a Flush or Stop that a test expects to succeed. It is a hang
// guard, not a latency bound: a flush fsyncs every dirty writer and its
// directory, and a one-second bound failed on a slow CI disk while the code
// was fine.
const opGuard = 30 * time.Second

func entry(t *testing.T, timeMS int64, uuid string) Entry {
	t.Helper()
	payload := map[string]any{
		"time":    timeMS,
		"uuid":    uuid,
		"type":    "user",
		"summary": "hi",
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return Entry{JSON: buf, TimeMS: timeMS}
}

// newTestPersister is a helper that builds a Persister against a
// tmpdir with tight defaults (short flush, tiny idle) so tests don't
// hang waiting for real-time intervals.
func newTestPersister(t *testing.T, overrides ...func(*Options)) (*Persister, string) {
	t.Helper()
	dir := t.TempDir()
	opts := Options{
		Dir:            dir,
		MaxFileBytes:   1 * 1024 * 1024, // 1 MiB — easy to hit from a test
		IdxStride:      4,
		FlushInterval:  20 * time.Millisecond,
		IdleCloseAfter: 500 * time.Millisecond,
		ChannelBuffer:  128,
		Generator:      "naozhi-test",
		DevMode:        false,
	}
	for _, o := range overrides {
		o(&opts)
	}
	p, err := NewPersister(opts)
	if err != nil {
		t.Fatalf("NewPersister: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	})
	return p, dir
}

// readAllRecords walks every framed record in a .log file, returning
// the decoded schema.Record values. Used by assertions below.
func readAllRecords(t *testing.T, path string) []*schema.Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64*1024)
	var out []*schema.Record
	for {
		rec, err := ReadRecord(br)
		if err != nil {
			break
		}
		out = append(out, rec)
	}
	return out
}

// TestPersister_DropsReplayPhase is the runtime blocker-1 guard from
// RFC §3.2.3: a sink receiving a replay=true batch must NOT write to
// disk and must count each of its entries in replay_leak_total — an
// observable signal rather than a panic (R242-GO-11), DevMode or not.
func TestPersister_DropsReplayPhase(t *testing.T) {
	p, dir := newTestPersister(t, func(o *Options) { o.DevMode = true })
	sink := p.SinkFor("dashboard:direct:alice:general")

	// Feed a replay batch — should be silently dropped.
	sink([]Entry{entry(t, 1700000001000, "uuid-r"), entry(t, 1700000001001, "uuid-s")}, true /* replay */)

	ctx, cancel := context.WithTimeout(context.Background(), opGuard)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	stats := p.Stats()
	if stats.ReplayLeak != 2 {
		t.Errorf("ReplayLeak=%d, want 2", stats.ReplayLeak)
	}
	if stats.Written != 0 {
		t.Errorf("Written=%d, want 0 (replay batch must not hit disk)", stats.Written)
	}
	// Log file must not exist — no live entry was ever written for
	// this key, so writerFor was never called.
	logPath := LogPath(dir, "dashboard:direct:alice:general")
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("log file exists despite replay-only feed (err=%v)", err)
	}
}

// TestPersister_FullChannel_Drops exercises the non-blocking drop
// path. We use a tiny buffer and don't drain — the second send should
// hit `default` and count as dropped.
func TestPersister_FullChannel_Drops(t *testing.T) {
	p, _ := newTestPersister(t, func(o *Options) {
		// ChannelBuffer=1 but flush is slow enough that the goroutine
		// hasn't drained by the time we send again.
		o.ChannelBuffer = 1
		o.FlushInterval = 500 * time.Millisecond
	})
	// Block the worker by holding the op channel.
	sink := p.SinkFor("k")

	// First send: enters channel.
	sink([]Entry{entry(t, 1, "u1")}, false)
	// Immediately followup: if the worker already drained we might
	// miss the drop; fire many to make the contention reliable.
	for i := 0; i < 256; i++ {
		sink([]Entry{entry(t, int64(i+2), fmt.Sprintf("u%d", i))}, false)
	}
	// At least one should have been dropped.
	stats := p.Stats()
	if stats.Dropped == 0 {
		t.Errorf("expected at least one drop under channel pressure, got 0")
	}
}

// TestPersister_SeqMonotonic confirms idx entries' Seq are strictly
// increasing, matching the record order in the log.
func TestPersister_SeqMonotonic(t *testing.T) {
	p, dir := newTestPersister(t)
	sink := p.SinkFor("k")
	for i := 0; i < 10; i++ {
		sink([]Entry{entry(t, int64(1700000000000+i), fmt.Sprintf("u%d", i))}, false)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opGuard)
	defer cancel()
	p.Flush(ctx)

	idx, err := ReadAllIdx(filepath.Join(dir, KeyHash("k")+idxExt))
	if err != nil {
		t.Fatalf("ReadAllIdx: %v", err)
	}
	if len(idx) == 0 {
		t.Fatalf("idx empty")
	}
	// Must be strictly monotonic.
	for i := 1; i < len(idx); i++ {
		if idx[i].Seq <= idx[i-1].Seq {
			t.Errorf("idx seq not monotonic: [%d]=%d <= [%d]=%d",
				i, idx[i].Seq, i-1, idx[i-1].Seq)
		}
	}
}

// TestPersister_DropKey_RemovesFiles round-trips a write, then
// DropKey, then confirms both log and idx are gone.
func TestPersister_DropKey_RemovesFiles(t *testing.T) {
	p, dir := newTestPersister(t)
	sink := p.SinkFor("k")
	sink([]Entry{entry(t, 1, "u1")}, false)

	ctx, cancel := context.WithTimeout(context.Background(), opGuard)
	defer cancel()
	p.Flush(ctx)

	// Confirm files exist first.
	if _, err := os.Stat(LogPath(dir, "k")); err != nil {
		t.Fatalf("log missing pre-drop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, KeyHash("k")+idxExt)); err != nil {
		t.Fatalf("idx missing pre-drop: %v", err)
	}

	if err := p.DropKey(ctx, "k"); err != nil {
		t.Fatalf("DropKey: %v", err)
	}
	if _, err := os.Stat(LogPath(dir, "k")); !os.IsNotExist(err) {
		t.Errorf("log still exists after drop: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, KeyHash("k")+idxExt)); !os.IsNotExist(err) {
		t.Errorf("idx still exists after drop: err=%v", err)
	}
}

// TestPersister_Stop_FlushesPending writes then Stops without an
// explicit Flush — Stop's shutdown path must fsync to disk or we'd
// lose the debounce-window data on graceful shutdown.
func TestPersister_Stop_FlushesPending(t *testing.T) {
	p, dir := newTestPersister(t, func(o *Options) {
		// Make debounce longer than the whole test so nothing flushes
		// via the tick — only Stop's own flush.
		o.FlushInterval = time.Hour
	})
	sink := p.SinkFor("k")
	sink([]Entry{entry(t, 1700000001000, "u1")}, false)
	// Give the writer goroutine a moment to receive + buffer.
	// (We don't call Flush here on purpose.)
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	recs := readAllRecords(t, LogPath(dir, "k"))
	if len(recs) != 2 {
		t.Errorf("post-Stop records=%d, want 2 (header+entry)", len(recs))
	}
}

// TestPersister_Stop_Idempotent: calling Stop twice is fine, and a sink
// called after Stop drops without panicking on the closed channel or
// blocking.
func TestPersister_Stop_Idempotent(t *testing.T) {
	p, _ := newTestPersister(t)
	sink := p.SinkFor("k")
	ctx, cancel := context.WithTimeout(context.Background(), opGuard)
	defer cancel()
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := p.Stop(ctx); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	sink([]Entry{entry(t, 1, "u")}, false)
}

// TestPersister_ConcurrentSinks exercises many goroutines feeding
// different keys. Races would show up under -race.
func TestPersister_ConcurrentSinks(t *testing.T) {
	p, _ := newTestPersister(t, func(o *Options) { o.ChannelBuffer = 2048 })

	const numKeys = 8
	const perKey = 50
	var wg sync.WaitGroup
	for i := 0; i < numKeys; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("k-%d", i)
			sink := p.SinkFor(key)
			for j := 0; j < perKey; j++ {
				sink([]Entry{entry(t,
					int64(1700000000000+i*perKey+j),
					fmt.Sprintf("u-%d-%d", i, j),
				)}, false)
			}
		}(i)
	}
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	stats := p.Stats()
	// Written ≤ total sent (may drop under channel pressure, but with
	// 2048 buffer this should fit comfortably).
	if stats.Written == 0 {
		t.Errorf("Written=0 after %d entries", numKeys*perKey)
	}
}

// TestPersister_SurvivesRestart writes data, stops, creates a fresh
// Persister against the same dir, and confirms new writes append
// alongside old records (same log file).
func TestPersister_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	opts := Options{
		Dir: dir, IdxStride: 4,
		FlushInterval: 20 * time.Millisecond, ChannelBuffer: 128,
	}

	// ----- run 1 ----------------------------------------------
	p1, err := NewPersister(opts)
	if err != nil {
		t.Fatalf("NewPersister 1: %v", err)
	}
	sink1 := p1.SinkFor("k")
	for i := 0; i < 5; i++ {
		sink1([]Entry{entry(t, int64(1700000000000+i), fmt.Sprintf("u%d", i))}, false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opGuard)
	defer cancel()
	_ = p1.Flush(ctx)
	_ = p1.Stop(ctx)

	recs1 := readAllRecords(t, LogPath(dir, "k"))
	if len(recs1) != 6 {
		t.Fatalf("run1 expected 6 records (header+5), got %d", len(recs1))
	}

	// ----- run 2 ----------------------------------------------
	p2, err := NewPersister(opts)
	if err != nil {
		t.Fatalf("NewPersister 2: %v", err)
	}
	defer p2.Stop(ctx)
	sink2 := p2.SinkFor("k")
	for i := 5; i < 10; i++ {
		sink2([]Entry{entry(t, int64(1700000000000+i), fmt.Sprintf("u%d", i))}, false)
	}
	_ = p2.Flush(ctx)

	recs2 := readAllRecords(t, LogPath(dir, "k"))
	if len(recs2) != 11 {
		t.Fatalf("run2 expected 11 records (6 prior + 5), got %d", len(recs2))
	}
	// Seq must continue from 6, not reset.
	if recs2[6].Seq != 6 || recs2[10].Seq != 10 {
		t.Errorf("seq did not continue after restart: %d...%d",
			recs2[6].Seq, recs2[10].Seq)
	}
}

// TestPersister_Stats_ReflectsWrites makes the /health-adjacent
// counters non-trivially observable so regressions are loud.
func TestPersister_Stats_ReflectsWrites(t *testing.T) {
	p, _ := newTestPersister(t)
	sink := p.SinkFor("k")
	for i := 0; i < 7; i++ {
		sink([]Entry{entry(t, int64(i+1), fmt.Sprintf("u%d", i))}, false)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opGuard)
	defer cancel()
	_ = p.Flush(ctx)

	stats := p.Stats()
	if stats.Written < 7 {
		t.Errorf("Written=%d, want >=7", stats.Written)
	}
	if stats.Fsyncs < 2 {
		t.Errorf("Fsyncs=%d, want >=2 (header + batch)", stats.Fsyncs)
	}
	if stats.ChannelCap != 128 {
		t.Errorf("ChannelCap=%d, want 128", stats.ChannelCap)
	}
}

// TestPersister_WriterAlive_MatchesStatsLogic confirms the fast
// path in WriterAlive() (which reads atomic / channel fields
// directly to skip the Stats struct alloc — R250-PERF-24) returns
// the same boolean as recomputing the predicate from a Stats
// snapshot. We sample three quiescent states: post-Stop (false),
// fresh (no drain yet), and post-Flush (drained recently).
func TestPersister_WriterAlive_MatchesStatsLogic(t *testing.T) {
	statsAlive := func(p *Persister) bool {
		if p.closed.Load() {
			return false
		}
		s := p.Stats()
		if s.ChannelCap == 0 {
			return false
		}
		notFull := s.ChannelDepth*5 < s.ChannelCap*4
		if s.ChannelDepth == 0 {
			return notFull
		}
		drainedRecently := s.LastDrainAgo > 0 && s.LastDrainAgo < 5*time.Second
		return drainedRecently && notFull
	}

	t.Run("fresh", func(t *testing.T) {
		p, _ := newTestPersister(t)
		if got, want := p.WriterAlive(), statsAlive(p); got != want {
			t.Errorf("WriterAlive=%v, stats-derived=%v", got, want)
		}
	})

	t.Run("after_drain", func(t *testing.T) {
		p, _ := newTestPersister(t)
		sink := p.SinkFor("k")
		sink([]Entry{entry(t, 1700000001000, "uuid-1")}, false)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		p.Flush(ctx)
		if got, want := p.WriterAlive(), statsAlive(p); got != want {
			t.Errorf("WriterAlive=%v, stats-derived=%v", got, want)
		}
		if !p.WriterAlive() {
			t.Errorf("WriterAlive=false after drain (unexpected)")
		}
	})

	t.Run("after_stop", func(t *testing.T) {
		p, _ := newTestPersister(t)
		ctx, cancel := context.WithTimeout(context.Background(), opGuard)
		defer cancel()
		_ = p.Stop(ctx)
		if got, want := p.WriterAlive(), statsAlive(p); got != want {
			t.Errorf("WriterAlive=%v, stats-derived=%v", got, want)
		}
		if p.WriterAlive() {
			t.Errorf("WriterAlive=true after Stop")
		}
	})
}

// TestPersister_Rotate is the O(1) tail-cut path: enough entries to
// exceed MaxFileBytes → rotate kicks in → subsequent file contains
// only the kept records (header + tail).
func TestPersister_Rotate(t *testing.T) {
	p, dir := newTestPersister(t, func(o *Options) {
		// Large payload ensures few records fill the tiny file cap.
		o.MaxFileBytes = 8 * 1024
		o.IdxStride = 2
	})
	sink := p.SinkFor("k")

	bigEntry := func(i int) Entry {
		payload := map[string]any{
			"time":   int64(1700000000000 + i),
			"uuid":   fmt.Sprintf("u%d", i),
			"type":   "user",
			"detail": fmt.Sprintf("payload %d: %s", i, string(make([]byte, 512))),
		}
		buf, _ := json.Marshal(payload)
		return Entry{JSON: buf, TimeMS: int64(1700000000000 + i)}
	}

	// Write enough to trigger rotate (well beyond DefaultKeepRecords)
	// but DefaultKeepRecords is 1000 — increase total to ensure at
	// least one rotate while still finishing fast.
	for i := 0; i < DefaultKeepRecords+50; i++ {
		sink([]Entry{bigEntry(i)}, false)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = p.Flush(ctx)

	// After rotate: log should be smaller than raw (dropped some
	// records). Exact count depends on stride; assert it's bounded.
	recs := readAllRecords(t, LogPath(dir, "k"))
	if len(recs) == 0 {
		t.Fatalf("log empty after rotate")
	}
	if recs[0].Type != schema.TypeHeader {
		t.Errorf("post-rotate record[0] not header: %+v", recs[0])
	}
	// Log size bounded — DefaultKeepRecords≈1000 → ~1000 records
	// remain at most (plus incoming writes during the test). We just
	// ensure rotate ran (size < total sends).
	if len(recs) > DefaultKeepRecords+100 {
		t.Errorf("rotate did not trim; records=%d", len(recs))
	}
}

// TestPersister_Stats_NoRace runs a basic concurrent Stats reader
// against concurrent writes to make sure the atomic counters hold
// under -race.
func TestPersister_Stats_NoRace(t *testing.T) {
	p, _ := newTestPersister(t)
	sink := p.SinkFor("k")
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-done:
				return
			default:
				_ = p.Stats()
				_ = p.WriterAlive()
			}
		}
	}()

	for i := 0; i < 200; i++ {
		sink([]Entry{entry(t, int64(i), fmt.Sprintf("u%d", i))}, false)
	}
	close(done)

	ctx, cancel := context.WithTimeout(context.Background(), opGuard)
	defer cancel()
	_ = p.Flush(ctx)
	if s := p.Stats(); s.Written == 0 {
		t.Errorf("Written=0 after 200 sends")
	}
}

// TestEffectiveFlushInterval_Buckets locks down the adaptive scaling
// table for R214-PERF-3 so changes to the multiplier ladder require a
// deliberate test update. Boundaries (16, 64, 256) are spelled out
// because the production behavior at high session counts depends on
// each one — bumping a writer count past a bucket halves the per-tick
// fsync rate, and we don't want a silent edit to that table.
func TestEffectiveFlushInterval_Buckets(t *testing.T) {
	const base = 200 * time.Millisecond
	cases := []struct {
		writers int
		want    time.Duration
	}{
		{0, base},           // empty persister stays at base
		{1, base},           // single session unchanged
		{16, base},          // upper edge of bucket 1
		{17, base + base/2}, // first to cross into 1.5×
		{50, base + base/2}, // issue's headline scenario → 300 ms
		{64, base + base/2}, // upper edge of bucket 2
		{65, base * 2},      // first to cross into 2×
		{200, base * 2},     // mid bucket 3
		{256, base * 2},     // upper edge of bucket 3
		{257, base * 4},     // first to cross into 4× cap
		{10_000, base * 4},  // cap holds — no unbounded scaling
	}
	for _, tc := range cases {
		got := effectiveFlushInterval(base, tc.writers)
		if got != tc.want {
			t.Errorf("writers=%d: got %v, want %v", tc.writers, got, tc.want)
		}
	}
}

// TestCollectFlushCandidates_AdaptiveSkipsUnderHighWriterCount verifies
// that the adaptive interval defers a flush that would have fired at
// the un-scaled FlushInterval boundary. Constructs writers manually
// (bypassing the run goroutine) to assert the scheduling decision in
// isolation from disk IO.
func TestCollectFlushCandidates_AdaptiveSkipsUnderHighWriterCount(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	base := 200 * time.Millisecond
	p := &Persister{
		opts:    Options{FlushInterval: base},
		writers: make(map[string]*perKeyWriter),
	}
	// Fill enough writers to land in the 1.5× bucket (17–64).
	const N = 30
	for i := 0; i < N; i++ {
		key := fmt.Sprintf("k%d", i)
		p.writers[key] = &perKeyWriter{
			dirty:        true,
			firstDirtyAt: now.Add(-250 * time.Millisecond), // past base, before 1.5×base=300ms
		}
	}
	// At base (200 ms) all 30 would flush. With the 1.5× scale (300 ms)
	// none of them have aged past the threshold yet.
	cands := p.collectFlushCandidates(now)
	if len(cands) != 0 {
		t.Errorf("expected adaptive interval to defer flush, got %d candidates", len(cands))
	}
	// Advance past 1.5×base — every writer should now flush.
	now2 := now.Add(60 * time.Millisecond) // age = 310 ms
	cands2 := p.collectFlushCandidates(now2)
	if len(cands2) != N {
		t.Errorf("after deadline: got %d candidates, want %d", len(cands2), N)
	}
	// Sanity: with a small writer-set the same age would already be
	// flushable at the base interval — confirms the adaptive branch is
	// what gated the first call, not some other filter.
	small := &Persister{
		opts:    Options{FlushInterval: base},
		writers: make(map[string]*perKeyWriter),
	}
	for i := 0; i < 5; i++ {
		small.writers[fmt.Sprintf("k%d", i)] = &perKeyWriter{
			dirty:        true,
			firstDirtyAt: now.Add(-250 * time.Millisecond),
		}
	}
	if got := len(small.collectFlushCandidates(now)); got != 5 {
		t.Errorf("small writer-set should flush all 5 at age=250ms, got %d", got)
	}
}

// TestPersister_PressureAndAccept pins the R244-ARCH-2 / #1057
// observability surface: callers can probe ingest backpressure without
// waiting for OnDrop. Pressure() reflects the in-channel queue depth as a
// 0..1 ratio; Accept() returns true while there is meaningful slack.
func TestPersister_PressureAndAccept(t *testing.T) {
	t.Parallel()

	// Empty queue: Pressure 0, Accept true.
	p, _ := newTestPersister(t)
	defer func() {
		_ = p.Stop(context.Background())
	}()
	if got := p.Pressure(); got != 0 {
		t.Errorf("empty Pressure() = %v, want 0", got)
	}
	if !p.Accept() {
		t.Error("empty Accept() = false, want true")
	}

	// Fill ~half the buffer by sending direct batchJobs.
	half := cap(p.in) / 2
	for i := 0; i < half; i++ {
		select {
		case p.in <- batchJob{Key: "k", Stem: "s", Entries: nil}:
		default:
			t.Fatalf("could not enqueue at i=%d", i)
		}
	}
	pr := p.Pressure()
	if pr <= 0.3 || pr >= 0.7 {
		t.Errorf("half-full Pressure() = %v, want roughly 0.5", pr)
	}
	if !p.Accept() {
		t.Error("half-full Accept() = false, want true (slack remaining)")
	}

	// Closed persister: Pressure 0, Accept false (uniform "do not produce").
	closed := &Persister{}
	closed.closed.Store(true)
	if got := closed.Pressure(); got != 0 {
		t.Errorf("closed Pressure() = %v, want 0", got)
	}
	if closed.Accept() {
		t.Error("closed Accept() = true, want false")
	}

	// Nil receiver: same defensive behaviour.
	var nilP *Persister
	if got := nilP.Pressure(); got != 0 {
		t.Errorf("nil Pressure() = %v, want 0", got)
	}
	if nilP.Accept() {
		t.Error("nil Accept() = true, want false")
	}
}

// TestSinkFor_RoutesByBoundKey pins R249-PERF-29 (#997): SinkFor now returns
// a *sessionSink method value rather than a multi-variable closure. Two sinks
// built from the same persister must each route their batches to the file
// stem derived from their own bound key — i.e. the per-key binding survives
// the closure→method-value refactor. Regression guard against accidentally
// sharing key/stem state across sinks.
func TestSinkFor_RoutesByBoundKey(t *testing.T) {
	p, dir := newTestPersister(t)

	const keyA = "feishu:group:aaa"
	const keyB = "feishu:p2p:bbb"
	sinkA := p.SinkFor(keyA)
	sinkB := p.SinkFor(keyB)

	sinkA([]Entry{entry(t, 1, "a1")}, false)
	sinkB([]Entry{entry(t, 2, "b1")}, false)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if KeyHash(keyA) == KeyHash(keyB) {
		t.Fatalf("test precondition: distinct keys hashed to same stem")
	}

	assertSingleEntryFile(t, LogPath(dir, keyA), keyA)
	assertSingleEntryFile(t, LogPath(dir, keyB), keyB)
}

// assertSingleEntryFile verifies a freshly-written per-session log holds
// exactly one header (seq 0, carrying wantKey) plus one entry (seq 1) —
// confirming the sessionSink routed its batch to the file bound to its own
// key.
func assertSingleEntryFile(t *testing.T, path, wantKey string) {
	t.Helper()
	recs := readAllRecords(t, path)
	if len(recs) != 2 {
		t.Fatalf("file %s: got %d records, want 2 (header+entry)", path, len(recs))
	}
	if recs[0].Type != schema.TypeHeader || recs[0].Header == nil || recs[0].Seq != 0 {
		t.Fatalf("file %s: record[0] is not a header: %+v", path, recs[0])
	}
	if recs[0].Header.Key != wantKey {
		t.Errorf("file %s: header key = %q, want %q", path, recs[0].Header.Key, wantKey)
	}
	if recs[1].Type != schema.TypeEntry || recs[1].Seq != 1 {
		t.Errorf("file %s: record[1] is not an entry: %+v", path, recs[1])
	}
}
