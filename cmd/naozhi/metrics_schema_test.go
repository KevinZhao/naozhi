package main

import (
	"expvar"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/promexport"
)

// TestMetricsEveryMapHasLabelSchema fails when a naozhi_* expvar.Map reaches
// the binary without label names: /metrics would export it under the generic
// `key` label, which a dashboard author cannot tell from a typo. Register it
// with promexport.NewMap / metrics.NewLabeledCounter, or as a histogram.
func TestMetricsEveryMapHasLabelSchema(t *testing.T) {
	seen := 0
	expvar.Do(func(kv expvar.KeyValue) {
		if !strings.HasPrefix(kv.Key, promexport.Prefix) {
			return
		}
		if _, ok := kv.Value.(*expvar.Map); !ok {
			return
		}
		seen++
		if !promexport.HasSchema(kv.Key) {
			t.Errorf("%s is an expvar.Map without a label schema", kv.Key)
		}
	})
	if seen < 10 {
		t.Errorf("saw only %d naozhi_* maps; the packages that register them are not linked into this test", seen)
	}
}
