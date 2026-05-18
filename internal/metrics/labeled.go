// Labeled-counter / labeled-gauge support built on stdlib expvar.Map.
//
// Why expvar.Map (not Prometheus): naozhi's metrics package commits to
// "zero deps, stdlib-stable" (see metrics.go top docstring). Adding a
// real metrics library only to gain labels would conflict with that
// design and force operators to re-tool dashboards. expvar.Map keeps
// the JSON shape exposed at /debug/vars consistent — each labeled
// metric becomes a JSON object whose keys are the label-value tuples
// joined with `|`.
//
// Multi-Backend RFC §10 (Sprint 6a) requires four metrics to gain a
// `backend` dimension:
//
//   - naozhi_cli_spawn_total{backend}
//   - naozhi_session_active{backend}                 (gauge)
//   - naozhi_protocol_rpc_error_total{backend,method,code}
//   - naozhi_acp_cancel_total{backend}
//
// Strategy: keep the legacy unlabeled expvar.Int counters (so existing
// docs/ops/pprof.md jq queries continue to work) while ALSO writing into
// a new expvar.Map keyed by the label tuple. After a 4-week double-write
// migration window, the legacy ints can be removed in a follow-up.
//
// Cardinality: callers MUST sanitize variable label values (e.g. RPC
// method strings come from agent JSON, user-supplied attachments, etc.)
// before passing them in. labelKey enforces a hard MAX_KEY_LEN cap so a
// runaway agent can't blow up the map; over-length keys collapse into a
// sentinel "_overflow_" bucket that operators can alert on.

package metrics

import (
	"expvar"
	"strings"
	"sync"
)

// LabelOverflow is the sentinel key used when a label tuple exceeds
// maxLabelKeyLen. Counting overflows preserves total volume integrity
// (no silent drops) and the operator alerting story is "any non-zero
// rate on the _overflow_ key means a caller is mis-sanitizing labels".
const LabelOverflow = "_overflow_"

// LabelEmpty is the key used when a caller passes "" for a label slot
// (e.g. legacy session records without backend metadata). Distinct from
// the overflow sentinel so dashboards can tell "missing data" apart
// from "label was too long to record".
const LabelEmpty = "_empty_"

// maxLabelKeyLen caps the joined label tuple at 256 bytes. Each label
// segment is also independently capped via clipLabelSegment so a
// single attacker-controlled value can't dominate the budget.
const maxLabelKeyLen = 256

// maxLabelSegmentLen caps any single label value at 64 bytes — enough
// for canonical IDs ("claude", "kiro", "session/cancel", "INVALID_PARAMS")
// while keeping the per-tuple cardinality bounded.
const maxLabelSegmentLen = 64

// LabeledCounter wraps an expvar.Map and exposes Add(labels, delta) plus
// a Sum() helper for tests. Zero value not usable — call NewLabeledCounter.
type LabeledCounter struct {
	m *expvar.Map
}

// NewLabeledCounter registers a new labeled counter under the given
// expvar name. Panics on duplicate registration (same as expvar.NewMap)
// to surface accidental double-registration.
func NewLabeledCounter(name string) *LabeledCounter {
	return &LabeledCounter{m: expvar.NewMap(name)}
}

// Add increments the counter for the given label tuple by delta. delta
// is an int64 to match expvar.Int.Add semantics; callers normally pass 1.
//
// Empty labels (the caller passed "") become "_empty_" to keep the JSON
// keys strictly non-empty (jq doesn't tolerate "" keys nicely). Over-long
// keys collapse into LabelOverflow — see package doc.
func (lc *LabeledCounter) Add(delta int64, labels ...string) {
	lc.m.Add(labelKey(labels), delta)
}

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

// LabeledGauge is the gauge counterpart to LabeledCounter. Same map
// backing; the API differs only in that callers Inc / Dec rather than
// Add a fixed-delta count of events. Internally still expvar.Map of
// *expvar.Int, so dashboards see the same JSON shape.
type LabeledGauge struct {
	m *expvar.Map
}

// NewLabeledGauge registers a new labeled gauge under the given expvar
// name. Same panic-on-duplicate semantics as NewLabeledCounter.
func NewLabeledGauge(name string) *LabeledGauge {
	return &LabeledGauge{m: expvar.NewMap(name)}
}

// Inc bumps the gauge for the label tuple by 1.
func (lg *LabeledGauge) Inc(labels ...string) { lg.m.Add(labelKey(labels), 1) }

// Dec decrements the gauge for the label tuple by 1. The gauge is allowed
// to go negative — that is itself an operator signal that bookkeeping is
// off. (Matching activeCount.Add(-1) < 0 → Store(0) clamp would mask the
// bug.) For aggressive clamping, callers can call ClampNonNegative.
func (lg *LabeledGauge) Dec(labels ...string) { lg.m.Add(labelKey(labels), -1) }

// ClampNonNegative ensures the gauge for the given labels is at least
// zero. Used by Router.activeCount-style sites that prefer "stuck at 0"
// over "drifts negative". Returns the post-clamp value.
func (lg *LabeledGauge) ClampNonNegative(labels ...string) int64 {
	key := labelKey(labels)
	v := lg.m.Get(key)
	if v == nil {
		return 0
	}
	iv, ok := v.(*expvar.Int)
	if !ok {
		return 0
	}
	if iv.Value() < 0 {
		// expvar.Int has no Set when value is private; use Add to bump
		// up to zero. No single-CAS atomic — between Value() read and
		// Add() write a concurrent Inc/Dec may slip in. Acceptable: the
		// race only briefly violates the non-negative invariant, which
		// is the exact slack we'd be enforcing anyway.
		iv.Add(-iv.Value())
	}
	return iv.Value()
}

// Get returns the current value for the given label tuple.
func (lg *LabeledGauge) Get(labels ...string) int64 {
	v := lg.m.Get(labelKey(labels))
	if v == nil {
		return 0
	}
	if iv, ok := v.(*expvar.Int); ok {
		return iv.Value()
	}
	return 0
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

// ForEachKey calls fn with every label-tuple key present in the gauge.
// Used by reconciliation paths (e.g. session.Router.countActive) that
// need to zero out previously-seen backends whose last session exited
// — without this, a "kiro just disappeared" bucket would stay at its
// last value forever. Fn must not call other methods on lg (deadlock).
func (lg *LabeledGauge) ForEachKey(fn func(key string)) {
	lg.m.Do(func(kv expvar.KeyValue) { fn(kv.Key) })
}

// ForEachKey on LabeledCounter for symmetry; rarely needed (counters
// only grow) but useful in tests.
func (lc *LabeledCounter) ForEachKey(fn func(key string)) {
	lc.m.Do(func(kv expvar.KeyValue) { fn(kv.Key) })
}

// labelKey joins label values into a single string for use as an
// expvar.Map key. Format: "v1|v2|v3". Empty / overflow handling is per
// segment to keep the cardinality bound tight.
//
// Pooled builders amortize allocs on the hot path (every Add).
func labelKey(labels []string) string {
	if len(labels) == 0 {
		return LabelEmpty
	}
	b := keyBuilderPool.Get().(*strings.Builder)
	b.Reset()
	defer keyBuilderPool.Put(b)
	for i, v := range labels {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteString(clipLabelSegment(v))
	}
	if b.Len() > maxLabelKeyLen {
		// Pathological: many segments each <= maxLabelSegmentLen still
		// blow the joined budget. Track via a counter the operator can
		// alert on — see overflowCount.
		overflowCount.Add(1)
		return LabelOverflow
	}
	return b.String()
}

// clipLabelSegment truncates a single label value to the per-segment cap
// and replaces empty values with LabelEmpty. Truncation is byte-based
// (not rune-aware) since labels are expected to be ASCII identifiers;
// the worst case for an unexpected UTF-8 input is a key that ends with
// half a rune which is still a valid expvar.Map key.
func clipLabelSegment(v string) string {
	if v == "" {
		return LabelEmpty
	}
	if len(v) > maxLabelSegmentLen {
		return v[:maxLabelSegmentLen]
	}
	return v
}

var keyBuilderPool = sync.Pool{
	New: func() any { return new(strings.Builder) },
}

// overflowCount counts labelKey calls that produced LabelOverflow. Exposed
// via expvar so operators can alert on >0 rate. Distinct from the per-map
// LabelOverflow bucket (which only captures the last 1 of N tuples that
// share the overflow sentinel) — overflowCount is the cumulative total.
var overflowCount = expvar.NewInt("naozhi_metrics_label_overflow_total")
