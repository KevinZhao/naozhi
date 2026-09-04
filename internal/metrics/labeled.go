// Labeled counters / gauges built on stdlib expvar.Map, keeping the package's
// zero-dependency commitment. Each labeled metric is a JSON object at
// /debug/vars whose keys are the label-value tuples joined with `|`. Metrics
// with a `backend` dimension (Multi-Backend RFC §10) double-write into the
// legacy unlabeled expvar.Int and the labeled map.
//
// Cardinality: callers MUST sanitize variable label values (RPC method
// strings from agent JSON, user-supplied data) before passing them in.
// labelKey caps each segment and the joined tuple; over-length keys collapse
// into the LabelOverflow sentinel bucket operators can alert on.

package metrics

import (
	"expvar"
	"strings"
	"sync"
)

// LabelOverflow is the sentinel key for a label tuple exceeding
// maxLabelKeyLen; counting overflows keeps total volume intact and alertable.
const LabelOverflow = "_overflow_"

// LabelEmpty is the key for an empty label value, distinct from the overflow
// sentinel so dashboards can tell "missing" from "too long".
const LabelEmpty = "_empty_"

// maxLabelKeyLen caps the joined label tuple.
const maxLabelKeyLen = 256

// maxLabelSegmentLen caps any single label value — enough for canonical IDs
// while bounding per-tuple cardinality.
const maxLabelSegmentLen = 64

// LabeledCounter wraps an expvar.Map; Add is the sole production write
// entrypoint (read helpers live in labeled_test_helpers_test.go — operators
// read via /debug/vars). Zero value not usable; call NewLabeledCounter.
type LabeledCounter struct {
	m *expvar.Map
}

// NewLabeledCounter registers a labeled counter under name. Panics on
// duplicate registration (as expvar.NewMap).
func NewLabeledCounter(name string) *LabeledCounter {
	return &LabeledCounter{m: expvar.NewMap(name)}
}

// Add increments the counter for the label tuple by delta. Empty labels
// become LabelEmpty; over-long tuples collapse into LabelOverflow.
func (lc *LabeledCounter) Add(delta int64, labels ...string) {
	lc.m.Add(labelKey(labels), delta)
}

// LabeledGauge is the gauge counterpart to LabeledCounter (same expvar.Map
// backing; callers Inc / Dec).
type LabeledGauge struct {
	m *expvar.Map
}

// NewLabeledGauge registers a labeled gauge under name; panics on duplicate.
func NewLabeledGauge(name string) *LabeledGauge {
	return &LabeledGauge{m: expvar.NewMap(name)}
}

// Inc bumps the gauge for the label tuple by 1.
func (lg *LabeledGauge) Inc(labels ...string) { lg.m.Add(labelKey(labels), 1) }

// Dec decrements the gauge for the label tuple by 1. The gauge may go
// negative — that is itself a signal that bookkeeping is off (a clamp would
// mask it, and a CAS-free clamp is racy under concurrent Dec). The router
// reconcile pass drives gauges back to authoritative truth on bulk paths.
func (lg *LabeledGauge) Dec(labels ...string) { lg.m.Add(labelKey(labels), -1) }

// Add applies delta in a single expvar.Map operation so the router
// reconciliation path exposes one transition to scrapers rather than N
// intermediate Inc/Dec values.
func (lg *LabeledGauge) Add(delta int64, labels ...string) {
	if delta == 0 {
		return
	}
	lg.m.Add(labelKey(labels), delta)
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

// ForEachKey calls fn with every label-tuple key present. Reconciliation
// (session.Router.countActive) uses it to zero out backends whose last
// session exited. fn must not call other methods on lg (deadlock).
func (lg *LabeledGauge) ForEachKey(fn func(key string)) {
	lg.m.Do(func(kv expvar.KeyValue) { fn(kv.Key) })
}

// labelKey joins label values into an expvar.Map key ("v1|v2|v3"). Pooled
// builders amortize allocs on the hot path (every Add).
func labelKey(labels []string) string {
	if len(labels) == 0 {
		return LabelEmpty
	}
	// A single label needs no join; clipLabelSegment's cap is below
	// maxLabelKeyLen, so the overflow check is unreachable here.
	if len(labels) == 1 {
		return clipLabelSegment(labels[0])
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
		// Many segments each within the per-segment cap still blew the joined budget.
		overflowCount.Add(1)
		return LabelOverflow
	}
	return b.String()
}

// clipLabelSegment truncates a label value to the per-segment cap (byte-based;
// labels are expected to be ASCII) and maps "" to LabelEmpty. A literal `|`
// is replaced with `_` because it is the tuple separator: otherwise
// ("a|b","c") and ("a","b|c") would share a bucket and silently merge streams.
func clipLabelSegment(v string) string {
	if v == "" {
		return LabelEmpty
	}
	if strings.IndexByte(v, '|') >= 0 {
		v = strings.ReplaceAll(v, "|", "_")
	}
	if len(v) > maxLabelSegmentLen {
		return v[:maxLabelSegmentLen]
	}
	return v
}

var keyBuilderPool = sync.Pool{
	New: func() any { return new(strings.Builder) },
}

// overflowCount counts labelKey calls that produced LabelOverflow — the
// cumulative total across maps, unlike the per-map sentinel bucket.
var overflowCount = expvar.NewInt("naozhi_metrics_label_overflow_total")
