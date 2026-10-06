// Package promexport renders the process's expvar variables in the Prometheus
// text exposition format, so the naozhi_* counters that today exist only for
// /api/debug/vars can be scraped (#3436). Leaf: stdlib only. The naming
// convention internal/metrics enforces (naozhi_<subsystem>_<name>_<suffix>)
// is what makes the mapping mechanical: `_total` is a counter, everything
// else a gauge; an expvar.Map becomes one series per key with a `key` label.
package promexport

import (
	"expvar"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Prefix selects the expvar names exported; stdlib's cmdline / memstats and
// anything else are skipped.
const Prefix = "naozhi_"

// ContentType is the exposition format's media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

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
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		writeVar(&b, name, vars[name])
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
		var rows []string
		x.Do(func(kv expvar.KeyValue) {
			if val, ok := scalar(kv.Value); ok {
				rows = append(rows, fmt.Sprintf("%s{key=%s} %s", name, strconv.Quote(kv.Key), val))
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
