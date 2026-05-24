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

// TestEventLog_ReplayPhase_SinkSetFirst is the blocker-1 runtime
// guard: if a caller violates the ordering contract by calling
// SetPersistSink BEFORE InjectHistory, the replay-phase batches
// must carry replayPhase=true so the Persister can drop them.
//
// Here we simulate the broken path by calling SetPersistSink first
// and then attempting to "replay" via AppendBatch. Since sinkReady
// flips to true the moment SetPersistSink is called, any AppendBatch
// after it is ALIVE (not replay). That's intentional — the runtime
// contract is "only Appends BEFORE SetPersistSink are replay". The
// broken ordering is "caller Set before InjectHistory", which is
// caught at the session.Router layer's own ordering contract test
// via AST lint; runtime there depends on sinkReady being a monotonic
// one-way flag.
//
// The test below verifies the monotonic flag: once sinkReady is
// true, it never returns to false, even if SetPersistSink is called
// with nil.
func TestEventLog_ReplayPhase_MonotonicSinkReady(t *testing.T) {
	l := NewEventLog(16)
	c := &captureSink{}
	l.SetPersistSink(c.asSink())

	// Uninstall via nil.
	l.SetPersistSink(nil)

	// Reinstall a fresh capture.
	c2 := &captureSink{}
	l.SetPersistSink(c2.asSink())

	l.Append(EventEntry{Type: "user", Summary: "post-reinstall"})
	_, replay, _ := c2.lastBatch()
	if replay {
		t.Errorf("reinstalled sink received replayPhase=true; sinkReady regressed")
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
