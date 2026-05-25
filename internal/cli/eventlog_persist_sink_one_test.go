package cli

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
)

// captureSinkOne records every PersistSinkOne invocation. Mirrors
// captureSink but for the single-entry contract added in #410.
type captureSinkOne struct {
	mu        sync.Mutex
	entries   []EventEntry
	replays   []bool
	callCount atomic.Int64
}

func (c *captureSinkOne) asSink() PersistSinkOne {
	return func(entry EventEntry, replayPhase bool) {
		c.mu.Lock()
		c.entries = append(c.entries, entry)
		c.replays = append(c.replays, replayPhase)
		c.mu.Unlock()
		c.callCount.Add(1)
	}
}

func (c *captureSinkOne) count() int { return int(c.callCount.Load()) }

func (c *captureSinkOne) last() (EventEntry, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) == 0 {
		return EventEntry{}, false, false
	}
	return c.entries[len(c.entries)-1], c.replays[len(c.replays)-1], true
}

// TestEventLog_SetPersistSinkPair_AppendUsesSingle confirms that when
// SetPersistSinkPair installs both a slice and single sink, Append
// dispatches to the single sink and the slice sink stays silent. This
// is the #410 hot-path guarantee — the slice-literal alloc disappears
// only because Append picks the single sink first.
func TestEventLog_SetPersistSinkPair_AppendUsesSingle(t *testing.T) {
	l := NewEventLog(16)
	batch := &captureSink{}
	one := &captureSinkOne{}
	l.SetPersistSinkPair(batch.asSink(), one.asSink())

	l.Append(EventEntry{Type: "user", Summary: "hi"})

	if batch.batchCount() != 0 {
		t.Errorf("slice sink called %d times for Append; expected 0", batch.batchCount())
	}
	if one.count() != 1 {
		t.Fatalf("single sink called %d times for Append; expected 1", one.count())
	}
	got, replay, _ := one.last()
	if got.Summary != "hi" {
		t.Errorf("single sink saw summary %q, want %q", got.Summary, "hi")
	}
	if replay {
		t.Errorf("single sink got replayPhase=true post-attach")
	}
	if got.UUID == "" {
		t.Errorf("single sink saw empty UUID; stampUUID should run before sink dispatch")
	}
}

// TestEventLog_SetPersistSinkPair_AppendBatchUsesSlice ensures the
// slice sink owns the batch path even when a single sink is paired —
// AppendBatch can't fan out into N PersistSinkOne calls because that
// would break the persister's per-batch atomic write-order.
func TestEventLog_SetPersistSinkPair_AppendBatchUsesSlice(t *testing.T) {
	l := NewEventLog(16)
	batch := &captureSink{}
	one := &captureSinkOne{}
	l.SetPersistSinkPair(batch.asSink(), one.asSink())

	l.AppendBatch([]EventEntry{
		{Type: "user", Summary: "a"},
		{Type: "user", Summary: "b"},
	})

	if one.count() != 0 {
		t.Errorf("single sink called %d times for AppendBatch; expected 0", one.count())
	}
	if batch.batchCount() != 1 {
		t.Fatalf("slice sink called %d times for AppendBatch; expected 1", batch.batchCount())
	}
	gotBatch, _, _ := batch.lastBatch()
	if len(gotBatch) != 2 {
		t.Errorf("slice batch len=%d, want 2", len(gotBatch))
	}
}

// TestEventLog_SetPersistSinkPair_FallbackWhenSingleNil verifies that
// passing a nil PersistSinkOne to SetPersistSinkPair degrades gracefully
// to slice-only behaviour — Append falls back to the legacy slice sink
// path. This keeps the API safe against partial-opt-in callers.
func TestEventLog_SetPersistSinkPair_FallbackWhenSingleNil(t *testing.T) {
	l := NewEventLog(16)
	batch := &captureSink{}
	l.SetPersistSinkPair(batch.asSink(), nil)

	l.Append(EventEntry{Type: "user", Summary: "x"})

	if batch.batchCount() != 1 {
		t.Errorf("slice sink called %d times; expected 1 fallback", batch.batchCount())
	}
}

// TestEventLog_SetPersistSink_ClearsPairedSingle locks in the symmetry
// rule: SetPersistSink overrides any previously paired single sink so
// callers can't end up in an inconsistent state where Append fires the
// stale single sink while AppendBatch fires the new slice sink. A
// SetPersistSink call retracts the single sink unconditionally.
func TestEventLog_SetPersistSink_ClearsPairedSingle(t *testing.T) {
	l := NewEventLog(16)
	batch1 := &captureSink{}
	one := &captureSinkOne{}
	l.SetPersistSinkPair(batch1.asSink(), one.asSink())

	// Switch back to the legacy slice-only API.
	batch2 := &captureSink{}
	l.SetPersistSink(batch2.asSink())

	l.Append(EventEntry{Type: "user", Summary: "after-switch"})

	if one.count() != 0 {
		t.Errorf("paired single sink fired after SetPersistSink override (count=%d)", one.count())
	}
	if batch2.batchCount() != 1 {
		t.Errorf("new slice sink called %d times; want 1", batch2.batchCount())
	}
	if batch1.batchCount() != 0 {
		t.Errorf("old slice sink fired post-replace (count=%d)", batch1.batchCount())
	}
}

// TestEventLog_SetPersistSinkPair_NilBatchClearsAll verifies the
// "nil batch == uninstall everything" semantics documented on
// SetPersistSinkPair. After a nil batch call both single and slice
// sinks are cleared so a subsequent Append fires neither.
func TestEventLog_SetPersistSinkPair_NilBatchClearsAll(t *testing.T) {
	l := NewEventLog(16)
	batch := &captureSink{}
	one := &captureSinkOne{}
	l.SetPersistSinkPair(batch.asSink(), one.asSink())

	// Uninstall.
	l.SetPersistSinkPair(nil, nil)

	l.Append(EventEntry{Type: "user", Summary: "after-clear"})

	if batch.batchCount() != 0 {
		t.Errorf("slice sink fired post-clear (count=%d)", batch.batchCount())
	}
	if one.count() != 0 {
		t.Errorf("single sink fired post-clear (count=%d)", one.count())
	}
}

// TestEventLog_PairedSingle_ReplayInvokeTotal confirms the diagnostic
// counter ReplayInvokeTotal accounts for single-sink dispatches the
// same way it accounts for slice-sink dispatches. A SetPersistSink-
// after-Append ordering violation observed via the single sink must
// surface on the same /health metric as the slice path. (The window
// where ptr is set but sinkReady is still false is too tight to hit
// via the public API; the test verifies the steady-state counter
// remains 0 for the well-ordered path, mirroring
// TestEventLog_ReplayInvokeTotal_PreSinkAttach.)
func TestEventLog_PairedSingle_ReplayInvokeTotal(t *testing.T) {
	l := NewEventLog(16)
	batch := &captureSink{}
	one := &captureSinkOne{}
	l.SetPersistSinkPair(batch.asSink(), one.asSink())

	l.Append(EventEntry{Type: "user", Summary: "p1"})
	l.Append(EventEntry{Type: "user", Summary: "p2"})

	if got := l.ReplayInvokeTotal(); got != 0 {
		t.Errorf("paired single dispatch bumped replay counter unexpectedly: %d", got)
	}
	if one.count() != 2 {
		t.Errorf("single sink called %d times; want 2", one.count())
	}
}

// TestEventLog_PairedSingle_ConcurrentAppend exercises the atomic
// pointer pair under -race so a regression to the inverted-ordering
// problem (single sink visible without sinkReady) would be caught.
// Producers race a sink swapper that flips between paired and
// unpaired modes; we just assert no panic and that the ring is
// populated.
func TestEventLog_PairedSingle_ConcurrentAppend(t *testing.T) {
	l := NewEventLog(256)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			l.Append(EventEntry{Type: "user", Summary: "x"})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			batch := &captureSink{}
			one := &captureSinkOne{}
			if i%2 == 0 {
				l.SetPersistSinkPair(batch.asSink(), one.asSink())
			} else {
				l.SetPersistSink(batch.asSink())
			}
		}
	}()
	wg.Wait()
	if len(l.Entries()) == 0 {
		t.Errorf("ring empty despite 200 appends")
	}
}

// TestEventLog_PairedSingle_PreservesEntryFields uses quick-check to
// fuzz a few representative fields and confirm the single sink
// receives exactly the entry the caller passed (modulo stampUUID
// filling in a fresh UUID). Catches accidental field-stripping in any
// future single-path optimisation.
func TestEventLog_PairedSingle_PreservesEntryFields(t *testing.T) {
	one := &captureSinkOne{}
	l := NewEventLog(64)
	l.SetPersistSinkPair(func([]EventEntry, bool) {}, one.asSink())

	check := func(typ, summary string, cost float64) bool {
		one.mu.Lock()
		one.entries = nil
		one.replays = nil
		one.callCount.Store(0)
		one.mu.Unlock()
		l.Append(EventEntry{Type: typ, Summary: summary, Cost: cost})
		got, _, ok := one.last()
		if !ok {
			return false
		}
		return got.Type == typ && got.Summary == summary && got.Cost == cost && got.UUID != ""
	}
	if err := quick.Check(check, &quick.Config{MaxCount: 32}); err != nil {
		t.Errorf("quick.Check: %v", err)
	}
}
