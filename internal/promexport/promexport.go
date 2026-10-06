// Package promexport renders the process's expvar variables in the Prometheus
// text exposition format, so the naozhi_* counters that today exist only for
// /api/debug/vars can be scraped (#3436). Leaf: stdlib only. The naming
// convention internal/metrics enforces (naozhi_<subsystem>_<name>_<suffix>)
// is what makes the mapping mechanical: `_total` is a counter, everything
// else a gauge; an expvar.Map becomes one series per key, labelled by the
// schema its owner registered (RegisterLabels) or a single `key` label.
package promexport

import (
	"expvar"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Prefix selects the expvar names exported; stdlib's cmdline / memstats and
// anything else are skipped.
const Prefix = "naozhi_"

// ContentType is the exposition format's media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Sentinel keys internal/metrics writes for an empty or over-long label
// tuple; a tuple that carries one is exported with that value in every label.
const (
	labelEmpty    = "_empty_"
	labelOverflow = "_overflow_"
)

type histogram struct {
	les []string
}

var (
	regMu      sync.RWMutex
	mapLabels  = map[string][]string{}
	histograms = map[string]histogram{}
)

// RegisterLabels names the labels of expvar.Map name. A key is split on `|`
// into one value per label (internal/metrics.labelKey's tuple format); a map
// keyed by a single string registers one label. Maps without a registration
// export a single `key` label. Call it at package init, next to expvar.NewMap.
func RegisterLabels(name string, labels ...string) {
	if len(labels) == 0 {
		panic("promexport: RegisterLabels " + name + " without labels")
	}
	regMu.Lock()
	defer regMu.Unlock()
	mapLabels[name] = append([]string(nil), labels...)
}

// NewMap registers expvar.Map name (panicking on duplicates, as expvar.NewMap)
// together with its label schema, for counters keyed by a plain string.
func NewMap(name string, labels ...string) *expvar.Map {
	m := expvar.NewMap(name)
	RegisterLabels(name, labels...)
	return m
}

// HasSchema reports whether expvar.Map name is exported with registered
// label names or as part of a registered histogram, rather than the generic
// `key` label.
func HasSchema(name string) bool {
	regMu.RLock()
	defer regMu.RUnlock()
	if _, ok := mapLabels[name]; ok {
		return true
	}
	_, ok := histograms[strings.TrimSuffix(name, "_bucket")]
	return ok
}

// RegisterHistogram exports the cumulative-bucket pair base+"_bucket"
// (expvar.Map keyed by upper bound) and base+"_sum" (expvar.Int) as one
// Prometheus histogram family base. les lists the bucket bounds in ascending
// order and ends with "+Inf"; a bound with no observations yet exports as 0.
// _count is the "+Inf" bucket.
func RegisterHistogram(base string, les []string) {
	if len(les) == 0 || les[len(les)-1] != "+Inf" {
		panic("promexport: RegisterHistogram " + base + " bounds must end in +Inf")
	}
	regMu.Lock()
	defer regMu.Unlock()
	histograms[base] = histogram{les: append([]string(nil), les...)}
}

// Write renders every expvar whose name starts with Prefix to w.
func Write(w io.Writer) error {
	var names []string
	vars := map[string]expvar.Var{}
	expvar.Do(func(kv expvar.KeyValue) {
		if strings.HasPrefix(kv.Key, Prefix) {
			names = append(names, kv.Key)
			vars[kv.Key] = kv.Value
		}
	})
	regMu.RLock()
	defer regMu.RUnlock()
	// A histogram's two backing vars are rendered as one family at its base
	// name; fold them out of the generic pass.
	for base := range histograms {
		delete(vars, base+"_bucket")
		delete(vars, base+"_sum")
		names = append(names, base)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		if h, ok := histograms[name]; ok {
			writeHistogram(&b, name, h)
			continue
		}
		if v, ok := vars[name]; ok {
			writeVar(&b, name, v)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// writeVar renders one variable; a value that is neither a number nor a map
// of numbers is skipped.
func writeVar(b *strings.Builder, name string, v expvar.Var) {
	switch x := v.(type) {
	case *expvar.Int:
		header(b, name)
		fmt.Fprintf(b, "%s %d\n", name, x.Value())
	case *expvar.Float:
		header(b, name)
		fmt.Fprintf(b, "%s %s\n", name, strconv.FormatFloat(x.Value(), 'g', -1, 64))
	case *expvar.Map:
		labels := mapLabels[name]
		if labels == nil {
			labels = []string{"key"}
		}
		var rows []string
		x.Do(func(kv expvar.KeyValue) {
			if val, ok := scalar(kv.Value); ok {
				rows = append(rows, name+labelSet(labels, kv.Key)+" "+val)
			}
		})
		if len(rows) == 0 {
			return
		}
		sort.Strings(rows)
		header(b, name)
		for _, r := range rows {
			b.WriteString(r)
			b.WriteByte('\n')
		}
	case expvar.Func:
		if val, ok := scalar(x); ok {
			header(b, name)
			fmt.Fprintf(b, "%s %s\n", name, val)
		}
	}
}

// writeHistogram renders the family registered under base. The bucket Map and
// sum Int are looked up by name, so a histogram whose owner package is not
// linked in exports nothing.
func writeHistogram(b *strings.Builder, base string, h histogram) {
	bm, _ := expvar.Get(base + "_bucket").(*expvar.Map)
	sum, _ := expvar.Get(base + "_sum").(*expvar.Int)
	if bm == nil || sum == nil {
		return
	}
	fmt.Fprintf(b, "# TYPE %s histogram\n", base)
	var count string
	for _, le := range h.les {
		val := "0"
		if v, ok := scalar(bm.Get(le)); ok {
			val = v
		}
		fmt.Fprintf(b, "%s_bucket{le=%q} %s\n", base, le, val)
		count = val
	}
	fmt.Fprintf(b, "%s_sum %d\n%s_count %s\n", base, sum.Value(), base, count)
}

// labelSet renders {l1="v1",l2="v2"} for a map key. The key is split on `|`
// into len(labels) values; a missing value becomes the empty sentinel, and a
// bare sentinel key fills every label so an overflow row stays recognisable.
func labelSet(labels []string, key string) string {
	parts := strings.SplitN(key, "|", len(labels))
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range labels {
		v := labelEmpty
		switch {
		case key == labelOverflow:
			v = labelOverflow
		case i < len(parts):
			v = parts[i]
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(v))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// escapeLabel applies the exposition format's three escapes; strconv.Quote
// would emit \x and \u forms parsers reject.
func escapeLabel(v string) string {
	v = strings.ToValidUTF8(v, "\uFFFD")
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// scalar renders a numeric expvar as text; false for anything else.
func scalar(v expvar.Var) (string, bool) {
	switch x := v.(type) {
	case *expvar.Int:
		return strconv.FormatInt(x.Value(), 10), true
	case *expvar.Float:
		return strconv.FormatFloat(x.Value(), 'g', -1, 64), true
	case expvar.Func:
		switch n := x.Value().(type) {
		case int:
			return strconv.Itoa(n), true
		case int64:
			return strconv.FormatInt(n, 10), true
		case float64:
			return strconv.FormatFloat(n, 'g', -1, 64), true
		}
	}
	return "", false
}

// header writes the TYPE line; `_total` names a counter, anything else a gauge.
func header(b *strings.Builder, name string) {
	typ := "gauge"
	if strings.HasSuffix(name, "_total") {
		typ = "counter"
	}
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
}
