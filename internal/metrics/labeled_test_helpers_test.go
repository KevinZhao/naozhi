package metrics

// Test-only read helpers for LabeledCounter / LabeledGauge.
//
// Issue #1208: these four methods have no production callers — they exist
// only so tests can assert on the labeled metric state. Keeping them in
// labeled.go bloated the production export surface (and made the file
// look like it had a richer read API than it actually offers). Moving
// them into a same-package _test.go preserves the test ergonomics
// (callers stay `counter.Get(...)`) while keeping them out of the
// production binary's export table.
//
// In production, read /debug/vars for the canonical labeled-metric view —
// that's the supported observability surface.

import "expvar"

// Get returns the current value for the given label tuple. Returns 0 if
// the tuple was never seen (never incremented). Test-only helper; in
// production read /debug/vars for the canonical view.
func (lc *LabeledCounter) Get(labels ...string) int64 {
	v := lc.m.Get(labelKey(labels))
	if v == nil {
		return 0
	}
	if iv, ok := v.(*expvar.Int); ok {
		return iv.Value()
	}
	return 0
}

// Sum returns the total across all label tuples. Useful in tests to
// assert "the sum of labeled increments equals the legacy unlabeled
// counter" during the double-write migration window.
func (lc *LabeledCounter) Sum() int64 {
	var total int64
	lc.m.Do(func(kv expvar.KeyValue) {
		if iv, ok := kv.Value.(*expvar.Int); ok {
			total += iv.Value()
		}
	})
	return total
}

// Sum returns the total across all label tuples.
func (lg *LabeledGauge) Sum() int64 {
	var total int64
	lg.m.Do(func(kv expvar.KeyValue) {
		if iv, ok := kv.Value.(*expvar.Int); ok {
			total += iv.Value()
		}
	})
	return total
}

// ForEachKey on LabeledCounter for symmetry with LabeledGauge; rarely
// needed (counters only grow) but useful in tests that want to enumerate
// every recorded label tuple.
func (lc *LabeledCounter) ForEachKey(fn func(key string)) {
	lc.m.Do(func(kv expvar.KeyValue) { fn(kv.Key) })
}
