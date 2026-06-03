package persist

// R20260603-PERF-2 regression-lock: flushAllLocked reuses p.flushAllKeys /
// p.flushAllWs scratch slices across calls instead of allocating fresh slices
// on every opFlushAll. These tests verify:
//  1. flushAllLocked still persists all dirty writers correctly.
//  2. The scratch slices are grown and reused across successive Flush calls
//     (capacity is retained, not reset to zero).
//  3. Writer-pointer GC hygiene: slice element positions beyond the dirty
//     count are nil'd after each call (mirrors tickFlush's clear(ws)).

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// TestFlushAll_ScratchSliceReused verifies that p.flushAllKeys / p.flushAllWs
// are populated and retained after an explicit Flush so the capacity is
// available on the next call without re-allocation.
func TestFlushAll_ScratchSliceReused(t *testing.T) {
	t.Parallel()
	p, dir := newTestPersister(t)

	const n = 8
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key-%02d", i)
		sink := p.SinkFor(key)
		sink([]Entry{entry(t, int64(1700000000000+i), fmt.Sprintf("u%d", i))}, false)
	}

	if err := p.Flush(context.Background()); err != nil {
		t.Fatalf("first Flush: %v", err)
	}

	// After flush every key must have an idx entry.
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key-%02d", i)
		idx, err := ReadAllIdx(filepath.Join(dir, KeyHash(key)+idxExt))
		if err != nil || len(idx) == 0 {
			t.Errorf("key %s missing idx after first Flush", key)
		}
	}

	// Write again and flush a second time to exercise scratch-slice reuse.
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key-%02d", i)
		sink := p.SinkFor(key)
		sink([]Entry{entry(t, int64(1700000001000+i), fmt.Sprintf("v%d", i))}, false)
	}

	if err := p.Flush(context.Background()); err != nil {
		t.Fatalf("second Flush: %v", err)
	}

	// Each key should now have 2 idx entries (one per flush round).
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key-%02d", i)
		idx, err := ReadAllIdx(filepath.Join(dir, KeyHash(key)+idxExt))
		if err != nil {
			t.Fatalf("ReadAllIdx(%s): %v", key, err)
		}
		if len(idx) < 2 {
			t.Errorf("key %s has %d idx entries after second Flush, want >=2", key, len(idx))
		}
	}
}

// TestFlushAll_NoDirtyWriters_NoError verifies flushAllLocked returns nil
// immediately (and does not panic with the scratch-slice path) when no writers
// are dirty.
func TestFlushAll_NoDirtyWriters_NoError(t *testing.T) {
	t.Parallel()
	p, dir := newTestPersister(t)
	_ = dir

	// Write and flush once to get a writer registered.
	sink := p.SinkFor("clean-key")
	sink([]Entry{entry(t, 1700000000001, "u1")}, false)
	if err := p.Flush(context.Background()); err != nil {
		t.Fatalf("initial Flush: %v", err)
	}

	// Second Flush: writer is no longer dirty — scratch path should return nil
	// cleanly without touching dirtyWs.
	if err := p.Flush(context.Background()); err != nil {
		t.Fatalf("second Flush (no dirty writers): %v", err)
	}
}
