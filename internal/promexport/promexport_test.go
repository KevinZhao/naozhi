package promexport

import (
	"expvar"
	"strings"
	"testing"
)

func TestWrite_RendersNaozhiVarsOnly(t *testing.T) {
	// expvar is process-global: names are unique to this test and stay registered.
	c := expvar.NewInt("naozhi_zz_test_events_total")
	c.Add(3)
	g := expvar.NewInt("naozhi_zz_test_procs_active")
	g.Set(2)
	m := expvar.NewMap("naozhi_zz_test_denied_total")
	m.Add("feishu:not_allowed", 1)
	m.Add(`slack:"quoted"`, 2)
	f := expvar.NewFloat("naozhi_zz_test_cost_ms")
	f.Set(1.5)
	expvar.Publish("naozhi_zz_test_goroutines_active", expvar.Func(func() any { return 7 }))
	expvar.Publish("naozhi_zz_test_skipped", expvar.Func(func() any { return "text" }))
	expvar.NewString("other_zz_ignored").Set("x")

	var sb strings.Builder
	if err := Write(&sb); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"# TYPE naozhi_zz_test_events_total counter\nnaozhi_zz_test_events_total 3\n",
		"# TYPE naozhi_zz_test_procs_active gauge\nnaozhi_zz_test_procs_active 2\n",
		"# TYPE naozhi_zz_test_denied_total counter\n",
		"naozhi_zz_test_denied_total{key=\"feishu:not_allowed\"} 1\n",
		"naozhi_zz_test_denied_total{key=\"slack:\\\"quoted\\\"\"} 2\n",
		"naozhi_zz_test_cost_ms 1.5\n",
		"naozhi_zz_test_goroutines_active 7\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n---\n%s", want, out)
		}
	}
	for _, absent := range []string{"other_zz_ignored", "naozhi_zz_test_skipped", "memstats", "cmdline"} {
		if strings.Contains(out, absent) {
			t.Errorf("output should not contain %q", absent)
		}
	}
	// Every non-comment line is `name[{labels}] value` with a numeric value.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 || strings.ContainsAny(line[sp+1:], "\" {}") {
			t.Errorf("malformed sample line %q", line)
		}
	}
}
