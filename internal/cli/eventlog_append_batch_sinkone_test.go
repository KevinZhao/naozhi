package cli

// R20260602190132-PERF-3 regression tests: AppendBatch len==1 fast path
// for the captureForSink branch.
//
// When AppendBatch is called with exactly one entry and a persist sink is
// wired (captureForSink==true), the previous code allocated
// make([]EventEntry, 1) for sinkCopy. The fast path avoids that allocation
// by routing through invokePersistSinkOne (when the pair sink is wired) or
// falling back to []EventEntry{sinkOne} (batch-only sink).
//
// These tests verify:
//  1. len==1 with SinkPair: entry reaches the single sink (not the batch sink)
//  2. len==1 with batch-only sink: entry reaches the batch sink
//  3. len>1 with SinkPair: still uses batch sink (fast path not triggered)
//  4. len==1 fast path preserves stampUUID in-place on caller's slice
//  5. len==1 fast path does NOT mutate caller's Time (same contract as no-sink path)
//  6. len==1 with captureForSink: entry is correctly written to ring buffer

import (
	"testing"
)

// TestAppendBatch_Len1_SinkPair_UsesSingleSink verifies that the len==1
// captureForSink fast path dispatches through invokePersistSinkOne when
// SetPersistSinkPair is used — the key allocation-saving path.
func TestAppendBatch_Len1_SinkPair_UsesSingleSink(t *testing.T) {
	t.Parallel()
	l := NewEventLog(16)
	batch := &captureSink{}
	one := &captureSinkOne{}
	l.SetPersistSinkPair(batch.asSink(), one.asSink())

	l.AppendBatch([]EventEntry{{Type: "user", Summary: "hello"}})

	if one.count() != 1 {
		t.Errorf("single sink call count = %d, want 1", one.count())
	}
	if batch.batchCount() != 0 {
		t.Errorf("batch sink call count = %d, want 0 (single sink should be used for len==1)", batch.batchCount())
	}
	got, replay, ok := one.last()
	if !ok {
		t.Fatal("single sink received no entries")
	}
	if got.Summary != "hello" {
		t.Errorf("single sink entry Summary = %q, want %q", got.Summary, "hello")
	}
	if replay {
		t.Errorf("single sink replayPhase = true, want false for live AppendBatch")
	}
}

// TestAppendBatch_Len1_BatchOnlySink_UsesBatchSink verifies that when only
// a batch sink is wired (SetPersistSink, no single sink), the len==1 fast
// path falls back to the slice-form batch sink correctly.
func TestAppendBatch_Len1_BatchOnlySink_UsesBatchSink(t *testing.T) {
	t.Parallel()
	l := NewEventLog(16)
	batch := &captureSink{}
	l.SetPersistSink(batch.asSink())

	l.AppendBatch([]EventEntry{{Type: "user", Summary: "world"}})

	if batch.batchCount() != 1 {
		t.Errorf("batch sink call count = %d, want 1", batch.batchCount())
	}
	got, replay, ok := batch.lastBatch()
	if !ok {
		t.Fatal("batch sink received no entries")
	}
	if len(got) != 1 {
		t.Fatalf("batch sink received %d entries, want 1", len(got))
	}
	if got[0].Summary != "world" {
		t.Errorf("batch sink entry Summary = %q, want %q", got[0].Summary, "world")
	}
	if replay {
		t.Errorf("batch sink replayPhase = true, want false for live AppendBatch")
	}
}

// TestAppendBatch_LenGT1_SinkPair_UsesBatchSink confirms the fast path is
// NOT triggered for len>1: the batch sink is used as before.
func TestAppendBatch_LenGT1_SinkPair_UsesBatchSink(t *testing.T) {
	t.Parallel()
	l := NewEventLog(16)
	batch := &captureSink{}
	one := &captureSinkOne{}
	l.SetPersistSinkPair(batch.asSink(), one.asSink())

	l.AppendBatch([]EventEntry{
		{Type: "user", Summary: "a"},
		{Type: "text", Summary: "b"},
	})

	if batch.batchCount() != 1 {
		t.Errorf("batch sink call count = %d, want 1 for len>1", batch.batchCount())
	}
	if one.count() != 0 {
		t.Errorf("single sink call count = %d, want 0 for len>1 path", one.count())
	}
	got, _, ok := batch.lastBatch()
	if !ok || len(got) != 2 {
		t.Errorf("batch sink received %d entries, want 2", len(got))
	}
}

// TestAppendBatch_Len1_SinkPair_UUIDStampedInPlace verifies that stampUUID
// still writes through &entries[i] (the historical in-place contract) on
// the len==1 fast path — the fast path must not break the UUID stamp.
func TestAppendBatch_Len1_SinkPair_UUIDStampedInPlace(t *testing.T) {
	t.Parallel()
	l := NewEventLog(16)
	one := &captureSinkOne{}
	l.SetPersistSinkPair(func([]EventEntry, bool) {}, one.asSink())

	in := []EventEntry{{Type: "user", Summary: "stamp-test"}}
	l.AppendBatch(in)

	if in[0].UUID == "" {
		t.Errorf("caller slice UUID not stamped; stampUUID must still write through &entries[i] on the len==1 fast path")
	}
}

// TestAppendBatch_Len1_SinkPair_CallerTimeNotMutated verifies that the
// default Time is NOT written back into the caller's slice — only the ring
// buffer copy and the sink copy carry the defaulted time. Same contract as
// the no-sink path (TestAppendBatch_NoSink_CallerSliceNotMutated).
func TestAppendBatch_Len1_SinkPair_CallerTimeNotMutated(t *testing.T) {
	t.Parallel()
	l := NewEventLog(16)
	one := &captureSinkOne{}
	l.SetPersistSinkPair(func([]EventEntry, bool) {}, one.asSink())

	in := []EventEntry{{Type: "user", Summary: "time-test"}} // Time==0
	l.AppendBatch(in)

	if in[0].Time != 0 {
		t.Errorf("caller slice Time mutated to %d on len==1 fast path, want 0", in[0].Time)
	}
	// The single-sink entry must carry the defaulted non-zero time.
	got, _, ok := one.last()
	if !ok {
		t.Fatal("single sink received no entries")
	}
	if got.Time == 0 {
		t.Errorf("single sink entry Time = 0, want a non-zero defaulted wall-clock value")
	}
}

// TestAppendBatch_Len1_SinkPair_EntryInRingBuffer confirms the entry is
// written to the ring buffer (Entries() returns it) even when the fast path
// is active — the ring write must not be skipped by the optimization.
func TestAppendBatch_Len1_SinkPair_EntryInRingBuffer(t *testing.T) {
	t.Parallel()
	l := NewEventLog(16)
	one := &captureSinkOne{}
	l.SetPersistSinkPair(func([]EventEntry, bool) {}, one.asSink())

	l.AppendBatch([]EventEntry{{Type: "user", Summary: "ring-check"}})

	entries := l.Entries()
	if len(entries) != 1 {
		t.Fatalf("ring buffer has %d entries, want 1", len(entries))
	}
	if entries[0].Summary != "ring-check" {
		t.Errorf("ring buffer entry Summary = %q, want %q", entries[0].Summary, "ring-check")
	}
}
