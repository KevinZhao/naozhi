package persist

import (
	"sync"
	"sync/atomic"
	"testing"
)

// R040034-PERF-13 (#1408): pins parallelFsync's contract — every entry
// in (keys, ws) is processed exactly once and the helper joins all
// workers before returning so the caller can safely mutate the
// underlying writers map afterward.

func TestParallelFsyncProcessesEveryEntryExactlyOnce(t *testing.T) {
	const n = 32
	keys := make([]string, n)
	ws := make([]*perKeyWriter, n)
	for i := 0; i < n; i++ {
		keys[i] = string(rune('A' + (i % 26)))
		ws[i] = &perKeyWriter{}
	}

	var (
		hits sync.Map // perKeyWriter pointer → seen count
		p    Persister
	)
	p.parallelFsync(keys, ws, func(k string, w *perKeyWriter) {
		// Use the writer pointer as the dedup key. String keys collide on
		// purpose above so a buggy fan-out keyed by string would surface.
		v, _ := hits.LoadOrStore(w, new(atomic.Int64))
		v.(*atomic.Int64).Add(1)
	})

	for i, w := range ws {
		v, ok := hits.Load(w)
		if !ok {
			t.Fatalf("ws[%d] (key=%s) was never processed by parallelFsync", i, keys[i])
		}
		if got := v.(*atomic.Int64).Load(); got != 1 {
			t.Fatalf("ws[%d] (key=%s) processed %d times; want exactly 1 — fan-out double-counted",
				i, keys[i], got)
		}
	}
}

func TestParallelFsyncSingleEntryStaysSerial(t *testing.T) {
	// n=1 must skip the WaitGroup path and just call fn directly. Pin
	// this so the fast path doesn't regress into spawning a goroutine
	// for a single-writer Stop (the typical idle-shutdown case).
	var calls atomic.Int64
	w := &perKeyWriter{}
	var p Persister
	p.parallelFsync([]string{"k"}, []*perKeyWriter{w}, func(string, *perKeyWriter) {
		calls.Add(1)
	})
	if got := calls.Load(); got != 1 {
		t.Fatalf("single-entry parallelFsync called fn %d times; want 1", got)
	}
}

func TestParallelFsyncEmptyIsNoOp(t *testing.T) {
	// Empty input must not spawn any worker. Otherwise idle-shutdown on
	// a Persister with zero open writers would still pay WaitGroup
	// allocation; protect against that regression.
	var calls atomic.Int64
	var p Persister
	p.parallelFsync(nil, nil, func(string, *perKeyWriter) {
		calls.Add(1)
	})
	if got := calls.Load(); got != 0 {
		t.Fatalf("empty parallelFsync called fn %d times; want 0", got)
	}
}

func TestParallelFsyncRespectsWorkerOverride(t *testing.T) {
	// parallelFsyncWorkers=1 must force serial execution (in deterministic
	// caller-supplied order). Tests that need stable ordering rely on this
	// hook; pin the contract so a future "always parallel" simplification
	// breaks visibly.
	prev := parallelFsyncWorkers
	parallelFsyncWorkers = 1
	defer func() { parallelFsyncWorkers = prev }()

	const n = 5
	keys := []string{"a", "b", "c", "d", "e"}
	ws := make([]*perKeyWriter, n)
	for i := range ws {
		ws[i] = &perKeyWriter{}
	}
	var seen []string
	var p Persister
	p.parallelFsync(keys, ws, func(k string, _ *perKeyWriter) {
		seen = append(seen, k)
	})
	if len(seen) != n {
		t.Fatalf("seen=%v; want 5 entries", seen)
	}
	for i := range keys {
		if seen[i] != keys[i] {
			t.Fatalf("workers=1 ordering not preserved: seen=%v want=%v", seen, keys)
		}
	}
}
