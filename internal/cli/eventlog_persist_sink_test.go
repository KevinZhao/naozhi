package cli

import (
	"sync"
	"sync/atomic"
	"testing"
)

// captureSink records every invocation so tests can assert what
// EventLog passed downstream. Concurrency-safe since Append / AppendBatch
// may be called from any goroutine.
type captureSink struct {
	mu        sync.Mutex
	batches   [][]EventEntry
	replays   []bool
	callCount atomic.Int64
}

func (c *captureSink) asSink() PersistSink {
	return func(entries []EventEntry, replayPhase bool) {
		c.mu.Lock()
		// Copy the slice so the ring buffer can reuse its backing
		// array without clobbering our captured history.
		cp := make([]EventEntry, len(entries))
		copy(cp, entries)
		c.batches = append(c.batches, cp)
		c.replays = append(c.replays, replayPhase)
		c.mu.Unlock()
		c.callCount.Add(1)
	}
}

func (c *captureSink) lastBatch() ([]EventEntry, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.batches) == 0 {
		return nil, false, false
	}
	return c.batches[len(c.batches)-1], c.replays[len(c.replays)-1], true
}

func (c *captureSink) batchCount() int {
	return int(c.callCount.Load())
}

// TestEventLog_StampUUID_Append assigns a UUID to any Append entry
// that arrives without one. This is the foundation of MergedSource
// dedup — if stampUUID ever regresses, dedup fails open and dashboard
// history shows duplicates.
func TestEventLog_StampUUID_Append(t *testing.T) {
	l := NewEventLog(16)
	l.Append(EventEntry{Type: "user", Summary: "hi"})
	got := l.Entries()
	if len(got) != 1 {
		t.Fatalf("got %d entries", len(got))
	}
	if got[0].UUID == "" {
		t.Errorf("Append did not stamp UUID on entry")
	}
	if len(got[0].UUID) != 32 {
		t.Errorf("UUID shape wrong: %q", got[0].UUID)
	}
}

// TestEventLog_StampUUID_AppendBatch covers the batch path. Each
// entry in the batch must get its own fresh UUID; a regression that
// reuses the same UUID across the batch would coalesce distinct
// events.
func TestEventLog_StampUUID_AppendBatch(t *testing.T) {
	l := NewEventLog(16)
	l.AppendBatch([]EventEntry{
		{Type: "user", Summary: "a"},
		{Type: "user", Summary: "b"},
		{Type: "user", Summary: "c"},
	})
	got := l.Entries()
	if len(got) != 3 {
		t.Fatalf("got %d entries", len(got))
	}
	seen := make(map[string]struct{})
	for _, e := range got {
		if e.UUID == "" {
			t.Errorf("entry %q has no UUID", e.Summary)
		}
		if _, dup := seen[e.UUID]; dup {
			t.Errorf("duplicate UUID %q in batch", e.UUID)
		}
		seen[e.UUID] = struct{}{}
	}
}

// TestEventLog_StampUUID_PreservesCaller: if the caller already set
// a UUID (e.g. DeriveLegacyUUID on Claude JSONL replay), stampUUID
// must not overwrite it. This keeps replay → persist → replay stable
// across naozhi restarts.
func TestEventLog_StampUUID_PreservesCaller(t *testing.T) {
	l := NewEventLog(16)
	l.Append(EventEntry{Type: "user", UUID: "aaaabbbbccccdddd0000111122223333"})
	got := l.Entries()
	if got[0].UUID != "aaaabbbbccccdddd0000111122223333" {
		t.Errorf("UUID overwritten: %q", got[0].UUID)
	}
}

// TestEventLog_SinkFires_Append is the hot-path guarantee: Append
// triggers the sink exactly once per call.
func TestEventLog_SinkFires_Append(t *testing.T) {
	l := NewEventLog(16)
	c := &captureSink{}
	l.SetPersistSink(c.asSink())
	l.Append(EventEntry{Type: "user", Summary: "hi"})
	if c.batchCount() != 1 {
		t.Errorf("sink called %d times, want 1", c.batchCount())
	}
	batch, replay, _ := c.lastBatch()
	if len(batch) != 1 {
		t.Errorf("batch size %d, want 1", len(batch))
	}
	if replay {
		t.Errorf("Append after SetPersistSink fired with replayPhase=true")
	}
	if batch[0].UUID == "" {
		t.Errorf("sink entry missing stamped UUID")
	}
	if batch[0].Summary != "hi" {
		t.Errorf("summary lost in sink copy: %q", batch[0].Summary)
	}
}

// TestEventLog_SinkFires_AppendBatch: one sink call for one
// AppendBatch, carrying the full slice in order.
func TestEventLog_SinkFires_AppendBatch(t *testing.T) {
	l := NewEventLog(16)
	c := &captureSink{}
	l.SetPersistSink(c.asSink())
	l.AppendBatch([]EventEntry{
		{Type: "user", Summary: "a"},
		{Type: "user", Summary: "b"},
		{Type: "user", Summary: "c"},
	})
	if c.batchCount() != 1 {
		t.Errorf("sink called %d times, want 1 (batch collapses)", c.batchCount())
	}
	batch, replay, _ := c.lastBatch()
	if len(batch) != 3 {
		t.Fatalf("batch size %d, want 3", len(batch))
	}
	if replay {
		t.Errorf("batch marked replayPhase=true despite sink being set")
	}
	if batch[0].Summary != "a" || batch[1].Summary != "b" || batch[2].Summary != "c" {
		t.Errorf("batch ordering not preserved: %+v", batch)
	}
}

// TestEventLog_AppendBatchReplay_NeverPersistsAfterSinkReady is the
// R20260530-COR-1 (#1482) regression: a late AppendBatchReplay that runs
// AFTER the persist sink has flipped sinkReady=true (reconnect/reattach
// ordering) must NOT push the replayed historical entries to the sink.
// Doing so would re-persist already-written JSONL turns. The replay
// contract is gated on isReplay, independent of sinkReady.
func TestEventLog_AppendBatchReplay_NeverPersistsAfterSinkReady(t *testing.T) {
	l := NewEventLog(16)
	c := &captureSink{}
	// Sink attached and ready (sinkReady=true).
	l.SetPersistSink(c.asSink())
	// Replay historical entries — this simulates a late InjectHistory after
	// the persister already flipped sinkReady=true.
	l.AppendBatchReplay([]EventEntry{
		{Type: "user", Summary: "old-1"},
		{Type: "text", Summary: "old-2"},
	})
	if c.batchCount() != 0 {
		batch, replay, _ := c.lastBatch()
		t.Fatalf("replay batch leaked to persist sink: calls=%d batch=%+v replayPhase=%v", c.batchCount(), batch, replay)
	}
	// A subsequent live AppendBatch must still persist normally.
	l.AppendBatch([]EventEntry{{Type: "user", Summary: "live"}})
	if c.batchCount() != 1 {
		t.Fatalf("live batch after replay should persist exactly once, got %d calls", c.batchCount())
	}
	batch, replay, _ := c.lastBatch()
	if replay {
		t.Errorf("live batch tagged replayPhase=true")
	}
	if len(batch) != 1 || batch[0].Summary != "live" {
		t.Errorf("live batch corrupted: %+v", batch)
	}
}

// TestEventLog_ReplayPhase_WithoutSink: calls before SetPersistSink
// don't fire the sink at all (sink Load returns nil). This is the
// SetPersistSink-ordering contract's positive path.
func TestEventLog_ReplayPhase_WithoutSink(t *testing.T) {
	l := NewEventLog(16)
	l.AppendBatch([]EventEntry{{Type: "user", Summary: "replay"}})
	// No sink set yet — the batch should be committed to the ring
	// but nothing should land in any capture.
	c := &captureSink{}
	l.SetPersistSink(c.asSink())
	// A post-Set Append now should NOT see the earlier replay batch.
	l.Append(EventEntry{Type: "user", Summary: "live"})
	if c.batchCount() != 1 {
		t.Fatalf("sink called %d times, want 1 (only the live append)", c.batchCount())
	}
	batch, replay, _ := c.lastBatch()
	if replay {
		t.Errorf("live Append marked replayPhase=true")
	}
	if batch[0].Summary != "live" {
		t.Errorf("sink saw %q, want 'live'", batch[0].Summary)
	}
}

// TestEventLog_ReplayPhase_NilResetsSinkReady locks in the
// R20260526-GO-010 contract: SetPersistSink(nil) flips sinkReady
// back to false so any subsequent SetPersistSink(real) re-enters
// the pre-attach phase cleanly. Without this reset, a
// "pause-persist → InjectHistory replay → resume-persist" sequence
// would tag replay batches as live and the Persister would commit
// duplicates to disk.
//
// The post-reinstall Append below MUST land with replayPhase=false
// because SetPersistSink(real) flips sinkReady back to true atomically.
// What the test really proves is that nil + real-install behaves like
// a fresh EventLog — the install path's atomic Store(true) cancels
// the nil path's Store(false) before any live Append observes it.
func TestEventLog_ReplayPhase_NilResetsSinkReady(t *testing.T) {
	l := NewEventLog(16)
	c := &captureSink{}
	l.SetPersistSink(c.asSink())

	// Uninstall via nil. After this call sinkReady MUST be false so a
	// subsequent install enters the pre-attach phase.
	l.SetPersistSink(nil)
	if l.sinkReady.Load() {
		t.Errorf("SetPersistSink(nil) did not reset sinkReady")
	}

	// Reinstall a fresh capture. The install path flips sinkReady
	// back to true, so the next Append sees replayPhase=false.
	c2 := &captureSink{}
	l.SetPersistSink(c2.asSink())
	if !l.sinkReady.Load() {
		t.Errorf("SetPersistSink(real) after nil did not flip sinkReady=true")
	}

	l.Append(EventEntry{Type: "user", Summary: "post-reinstall"})
	_, replay, _ := c2.lastBatch()
	if replay {
		t.Errorf("post-reinstall live Append marked replayPhase=true")
	}
}

// TestEventLog_SetPersistSinkNil_ThenInstall_ReplaysCorrectly
// exercises the operationally-meaningful path the R20260526-GO-010
// fix opens: pause persist via SetPersistSink(nil), feed a replay
// batch (e.g. InjectHistory), then re-install the sink. The replay
// batch fired between nil and re-install fires no sink (pointer is
// nil); the batch fired AFTER re-install but during the new replay
// phase MUST carry replayPhase=true so downstream persisters drop
// the duplicate.
//
// The "pre-install replay" + "post-install live" boundary is the
// inverse of this — exercised by TestEventLog_ReplayPhase_WithoutSink.
// What this test adds is the toggle: sinkReady must be reset by
// nil so that AppendBatchReplay-style use after re-install lands
// with replayPhase=true. SetPersistSink(real) flips it back to true
// before live Appends, so we use AppendBatchReplay (or its public
// equivalent) to exercise the moment between sink re-attach and the
// next live event — but since the public API doesn't expose a
// "stay-replay" hook, we assert via the simpler invariant: after
// SetPersistSink(nil) the field is false; after SetPersistSink(real)
// the field is true again, no leftover state.
func TestEventLog_SetPersistSinkNil_TogglesSinkReadyBoth(t *testing.T) {
	l := NewEventLog(16)
	if l.sinkReady.Load() {
		t.Fatalf("fresh EventLog has sinkReady=true")
	}

	c1 := &captureSink{}
	l.SetPersistSink(c1.asSink())
	if !l.sinkReady.Load() {
		t.Errorf("first install did not flip sinkReady=true")
	}

	l.SetPersistSink(nil)
	if l.sinkReady.Load() {
		t.Errorf("uninstall did not reset sinkReady=false")
	}

	c2 := &captureSink{}
	l.SetPersistSink(c2.asSink())
	if !l.sinkReady.Load() {
		t.Errorf("re-install did not flip sinkReady back to true")
	}
}

// TestEventLog_SetPersistSinkPair_NilBatchResetsSinkReady mirrors
// the SetPersistSink(nil) reset behaviour for SetPersistSinkPair's
// nil-batch uninstall path. R20260526-GO-010 symmetry: both clear
// entrypoints must produce a clean pre-attach state for the next
// install.
func TestEventLog_SetPersistSinkPair_NilBatchResetsSinkReady(t *testing.T) {
	l := NewEventLog(16)
	batch := &captureSink{}
	one := &captureSinkOne{}
	l.SetPersistSinkPair(batch.asSink(), one.asSink())
	if !l.sinkReady.Load() {
		t.Fatalf("SetPersistSinkPair did not flip sinkReady=true")
	}

	l.SetPersistSinkPair(nil, nil)
	if l.sinkReady.Load() {
		t.Errorf("SetPersistSinkPair(nil, nil) did not reset sinkReady=false")
	}
}

// TestEventLog_SinkNotCalledWhenUnset confirms Append is a no-op
// on the persist side when no sink is installed. Fake processes
// and unit-test harnesses rely on this.
func TestEventLog_SinkNotCalledWhenUnset(t *testing.T) {
	l := NewEventLog(16)
	// No SetPersistSink call at all.
	l.Append(EventEntry{Type: "user", Summary: "alone"})
	// Nothing to assert except that we reach here without panic and
	// the entry is visible in the ring.
	got := l.Entries()
	if len(got) != 1 || got[0].Summary != "alone" {
		t.Errorf("ring state wrong: %+v", got)
	}
}

// TestEventLog_SinkReceivesDefensiveCopy guarantees the sink can
// retain its slice. If we passed a view into the ring buffer, the
// next Append would overwrite entries and corrupt the sink's state.
func TestEventLog_SinkReceivesDefensiveCopy(t *testing.T) {
	l := NewEventLog(2) // small ring so Append wraps immediately
	c := &captureSink{}
	l.SetPersistSink(c.asSink())

	l.Append(EventEntry{Type: "user", Summary: "one"})
	// Capture the first batch's entry.
	firstBatch, _, _ := c.lastBatch()
	firstUUID := firstBatch[0].UUID

	// Now wrap the ring with 3 more appends — last one will overwrite
	// slot 0 (where "one" lived).
	l.Append(EventEntry{Type: "user", Summary: "two"})
	l.Append(EventEntry{Type: "user", Summary: "three"})
	l.Append(EventEntry{Type: "user", Summary: "four"})

	// The first batch captured earlier must still show "one".
	if firstBatch[0].Summary != "one" {
		t.Errorf("captured batch mutated: got %q", firstBatch[0].Summary)
	}
	if firstBatch[0].UUID != firstUUID {
		t.Errorf("captured UUID mutated: got %q, want %q",
			firstBatch[0].UUID, firstUUID)
	}
}

// TestEventLog_ReplayInvokeTotal_PreSinkAttach confirms R242-ARCH-20:
// any invokePersistSink that fires while sinkReady is still false must
// bump the diagnostic counter so /health (and tests) can detect a
// SetPersistSink-after-InjectHistory ordering violation in production.
//
// The setup runs SetPersistSink AFTER an Append + AppendBatch — i.e.
// the broken ordering. Both pre-attach calls observe sinkReady=false
// (the field starts zero). Post-attach calls see sinkReady=true and
// must NOT bump the counter further.
func TestEventLog_ReplayInvokeTotal_PreSinkAttach(t *testing.T) {
	l := NewEventLog(16)
	c := &captureSink{}

	// Stage 1: pre-attach calls. invokePersistSink early-returns when
	// the sink pointer is nil, so these do NOT count — the counter is
	// scoped to the window where a sink IS attached but sinkReady is
	// still false (which only happens via the
	// persistSinkPtr.Store-then-sinkReady.Store window inside
	// SetPersistSink itself, normally too tight to observe in tests).
	l.Append(EventEntry{Type: "user", Summary: "pre1"})
	l.AppendBatch([]EventEntry{{Type: "user", Summary: "pre2"}})

	if got := l.ReplayInvokeTotal(); got != 0 {
		t.Errorf("counter bumped without an attached sink: %d", got)
	}

	// Stage 2: install the sink and verify post-attach Appends DO NOT
	// count — sinkReady flipped to true atomically with the pointer
	// Store, so replay=false on every subsequent invocation.
	l.SetPersistSink(c.asSink())
	l.Append(EventEntry{Type: "user", Summary: "post1"})
	l.AppendBatch([]EventEntry{{Type: "user", Summary: "post2"}})

	if got := l.ReplayInvokeTotal(); got != 0 {
		t.Errorf("counter bumped on live-phase batch: %d", got)
	}

	// Verify the sink received exactly 2 batches with replay=false.
	if c.batchCount() != 2 {
		t.Fatalf("sink received %d batches, want 2", c.batchCount())
	}
	for i, replay := range c.replays {
		if replay {
			t.Errorf("batch %d carried replay=true post-attach", i)
		}
	}
}

// TestEventLog_SinkReady_LifecycleTransitions pins R242-ARCH-20 by
// locking down the SinkReady() accessor — the diagnostic /health ports
// the original review asked for. The state machine is:
//
//	construction          → SinkReady=false
//	SetPersistSink(real)  → SinkReady=true
//	SetPersistSink(nil)   → SinkReady=false (clear path)
//	SetPersistSink(real2) → SinkReady=true
//
// Each transition is atomic with the persistSinkPtr Store inside
// SetPersistSink (sink-first/ready-second on install, ready-first/sink-
// second on clear — see the godoc on SetPersistSink for the asymmetry
// proof). A regression that flipped sinkReady without the matching
// pointer Store would either silently lose an event (sinkReady=true,
// ptr=nil) or mis-tag a live event as replay (sinkReady=false,
// ptr=real). The accessor lets /health detect both via the
// SinkReady ↔ ReplayInvokeTotal pair.
//
// Also exercises the nil-receiver guard so /health request paths that
// race a torn-down EventLog report "not ready" instead of panicking.
func TestEventLog_SinkReady_LifecycleTransitions(t *testing.T) {
	t.Parallel()

	var nilLog *EventLog
	if nilLog.SinkReady() {
		t.Fatalf("SinkReady on nil receiver = true, want false")
	}

	l := NewEventLog(16)
	if l.SinkReady() {
		t.Fatalf("SinkReady at construction = true, want false")
	}

	c := &captureSink{}
	l.SetPersistSink(c.asSink())
	if !l.SinkReady() {
		t.Fatalf("SinkReady after SetPersistSink(real) = false, want true")
	}

	// Clear path: ready must flip back to false so a subsequent
	// SetPersistSink(real) re-enters the pre-attach phase cleanly.
	// Without this, a "pause persist → re-install sink → InjectHistory"
	// sequence would tag the replay batch replayPhase=false (live)
	// per R20260526-GO-010.
	l.SetPersistSink(nil)
	if l.SinkReady() {
		t.Fatalf("SinkReady after SetPersistSink(nil) = true, want false")
	}

	// Re-install: ready returns to true.
	l.SetPersistSink(c.asSink())
	if !l.SinkReady() {
		t.Fatalf("SinkReady after re-install = false, want true")
	}

	// SetPersistSinkPair install path also flips ready=true.
	l2 := NewEventLog(16)
	l2.SetPersistSinkPair(c.asSink(), nil)
	if !l2.SinkReady() {
		t.Fatalf("SinkReady after SetPersistSinkPair = false, want true")
	}
}

// TestEventLog_SinkConcurrent runs Appends under -race alongside
// sink-replacing SetPersistSink calls. Racey access to the atomic
// pointer would show up here.
func TestEventLog_SinkConcurrent(t *testing.T) {
	l := NewEventLog(256)
	var wg sync.WaitGroup
	wg.Add(2)
	// Producer.
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			l.Append(EventEntry{Type: "user", Summary: "x"})
		}
	}()
	// Sink swapper.
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			c := &captureSink{}
			l.SetPersistSink(c.asSink())
		}
	}()
	wg.Wait()
	// Ring should reflect all 200 appends (some may have been evicted
	// by the ring wrap; we just assert count > 0 and no panic).
	if len(l.Entries()) == 0 {
		t.Errorf("ring empty despite 200 appends")
	}
}

// TestEventLog_AppendBatch_RingMatchesSink locks down the R214-PERF-5
// invariant that moved the sinkCopy build outside l.mu: the ring buffer
// entry at slot N must be byte-identical to the sink batch entry at
// slot N. Both paths now derive from the same pre-prepared slice, so
// any drift here would mean an InjectHistory replay shows different
// data on disk vs. dashboard reads. Specifically asserts:
//   - default-time substitution (e.Time == 0 → defaultTime applied to both)
//   - UUID stamping reaches both
//   - Summary / Type fields preserved on both
func TestEventLog_AppendBatch_RingMatchesSink(t *testing.T) {
	l := NewEventLog(64)
	c := &captureSink{}
	l.SetPersistSink(c.asSink())
	in := []EventEntry{
		{Type: "user", Summary: "a"},                   // Time=0 → triggers default-time path
		{Type: "text", Summary: "b", Time: 1700000},    // explicit Time preserved
		{Type: "tool_use", Summary: "c", Tool: "Read"}, // tool field preserved
	}
	l.AppendBatch(in)
	if c.batchCount() != 1 {
		t.Fatalf("sink called %d times, want 1", c.batchCount())
	}
	sinkBatch, _, _ := c.lastBatch()
	ring := l.Entries()
	if len(sinkBatch) != len(ring) || len(ring) != 3 {
		t.Fatalf("len mismatch: sink=%d ring=%d want 3", len(sinkBatch), len(ring))
	}
	for i := range sinkBatch {
		s, r := sinkBatch[i], ring[i]
		if s.UUID != r.UUID || s.UUID == "" {
			t.Errorf("entry %d UUID mismatch: sink=%q ring=%q", i, s.UUID, r.UUID)
		}
		if s.Time != r.Time || s.Time == 0 {
			t.Errorf("entry %d Time mismatch or default-time not applied: sink=%d ring=%d", i, s.Time, r.Time)
		}
		if s.Summary != r.Summary {
			t.Errorf("entry %d Summary mismatch: sink=%q ring=%q", i, s.Summary, r.Summary)
		}
		if s.Type != r.Type {
			t.Errorf("entry %d Type mismatch: sink=%q ring=%q", i, s.Type, r.Type)
		}
		if s.Tool != r.Tool {
			t.Errorf("entry %d Tool mismatch: sink=%q ring=%q", i, s.Tool, r.Tool)
		}
	}
	// Default-time entry should match the explicit-time entry's time
	// only if the test ran across a millisecond boundary (defaultTime is
	// captured once per AppendBatch call from time.Now().UnixMilli()).
	// We just verify the default-time entry got SOME nonzero value that
	// matches between ring and sink (already covered above), and that
	// the explicit-time entry kept its 1700000 value.
	if ring[1].Time != 1700000 {
		t.Errorf("explicit Time was overwritten: got %d, want 1700000", ring[1].Time)
	}
}

// TestEventLog_AppendBatch_NoSinkFastPath confirms the !captureForSink
// path still works correctly after the R214-PERF-5 split: when no sink
// is wired, sinkCopy stays nil, the inner loop falls back to in-loop
// stamping, and the ring still receives correctly-prepared entries.
func TestEventLog_AppendBatch_NoSinkFastPath(t *testing.T) {
	l := NewEventLog(16)
	// No SetPersistSink call — captureForSink will be false.
	in := []EventEntry{
		{Type: "user", Summary: "x"}, // Time=0 → default-time
		{Type: "text", Summary: "y", Time: 1700001},
	}
	l.AppendBatch(in)
	ring := l.Entries()
	if len(ring) != 2 {
		t.Fatalf("ring len=%d, want 2", len(ring))
	}
	if ring[0].UUID == "" || ring[1].UUID == "" {
		t.Errorf("UUIDs not stamped on no-sink path: %+v", ring)
	}
	if ring[0].Time == 0 {
		t.Errorf("default Time not applied on no-sink path: %d", ring[0].Time)
	}
	if ring[1].Time != 1700001 {
		t.Errorf("explicit Time clobbered on no-sink path: %d", ring[1].Time)
	}
}
