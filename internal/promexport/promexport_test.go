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

func render(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	if err := Write(&sb); err != nil {
		t.Fatal(err)
	}
	return sb.String()
}

func TestWrite_RegisteredLabelNames(t *testing.T) {
	m := NewMap("naozhi_zz_lbl_rpc_total", "backend", "method", "code")
	m.Add("acp|session/new|-32000", 4)
	m.Add("acp|"+"_empty_"+"|7", 1)
	m.Add("_overflow_", 2)
	m.Add("_empty_", 3)
	out := render(t)
	for _, want := range []string{
		"# TYPE naozhi_zz_lbl_rpc_total counter\n",
		`naozhi_zz_lbl_rpc_total{backend="acp",method="session/new",code="-32000"} 4` + "\n",
		`naozhi_zz_lbl_rpc_total{backend="acp",method="_empty_",code="7"} 1` + "\n",
		`naozhi_zz_lbl_rpc_total{backend="_overflow_",method="_overflow_",code="_overflow_"} 2` + "\n",
		`naozhi_zz_lbl_rpc_total{backend="_empty_",method="_empty_",code="_empty_"} 3` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n---\n%s", want, out)
		}
	}
	if !HasSchema("naozhi_zz_lbl_rpc_total") || HasSchema("naozhi_zz_never_registered") {
		t.Error("HasSchema does not reflect label registrations")
	}
}

func TestWrite_ArityMismatchIsOverflowNotDuplicate(t *testing.T) {
	m := NewMap("naozhi_zz_arity_total", "a", "b")
	m.Add("x", 1)
	m.Add("x|_empty_", 2)
	m.Add("x|y|z", 4)
	m.Add("_overflow_", 8)
	m.Add("_empty_", 16)
	m.Add("_empty_|_empty_", 32)
	out := render(t)
	for _, want := range []string{
		`naozhi_zz_arity_total{a="x",b="_empty_"} 2` + "\n",
		`naozhi_zz_arity_total{a="_overflow_",b="_overflow_"} 13` + "\n",
		`naozhi_zz_arity_total{a="_empty_",b="_empty_"} 48` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n---\n%s", want, out)
		}
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "naozhi_zz_arity_total{") {
			continue
		}
		series := line[:strings.LastIndexByte(line, ' ')]
		if seen[series] {
			t.Errorf("duplicate series %s\n%s", series, out)
		}
		seen[series] = true
	}
}

func TestWrite_DuplicateSeriesSumAsFloat(t *testing.T) {
	m := NewMap("naozhi_zz_fsum_total", "a", "b")
	f := new(expvar.Float)
	f.Set(0.5)
	m.Set("p", f)
	m.Add("q", 1)
	if out := render(t); !strings.Contains(out, `naozhi_zz_fsum_total{a="_overflow_",b="_overflow_"} 1.5`+"\n") {
		t.Errorf("colliding float and int rows should sum\n%s", out)
	}
}

func TestWrite_RegisteredCounterTypeIgnoresSuffix(t *testing.T) {
	RegisterCounter("naozhi_zz_ctr_total_by_x", "x")
	expvar.NewMap("naozhi_zz_ctr_total_by_x").Add("a", 1)
	RegisterLabels("naozhi_zz_gauge_by_x", "x")
	expvar.NewMap("naozhi_zz_gauge_by_x").Add("a", 1)
	out := render(t)
	for _, want := range []string{
		"# TYPE naozhi_zz_ctr_total_by_x counter\n",
		"# TYPE naozhi_zz_gauge_by_x gauge\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q\n%s", want, out)
		}
	}
}

func TestWrite_UnregisteredMapKeepsKeyLabel(t *testing.T) {
	expvar.NewMap("naozhi_zz_plain_total").Add("a|b", 1)
	if out := render(t); !strings.Contains(out, `naozhi_zz_plain_total{key="a|b"} 1`+"\n") {
		t.Errorf("unregistered map should keep one key label\n%s", out)
	}
}

func TestWrite_LabelValueEscaping(t *testing.T) {
	NewMap("naozhi_zz_esc_total", "v").Add("a\\b\"c\nd\te\xff", 1)
	want := `naozhi_zz_esc_total{v="a\\b\"c\nd` + "\te�" + `"} 1` + "\n"
	if out := render(t); !strings.Contains(out, want) {
		t.Errorf("output lacks %q\n%s", want, out)
	}
}

func TestWrite_Histogram(t *testing.T) {
	bm := expvar.NewMap("naozhi_zz_hist_ms_bucket")
	sum := expvar.NewInt("naozhi_zz_hist_ms_sum")
	RegisterHistogram("naozhi_zz_hist_ms", []string{"10", "100", "+Inf"})

	empty := render(t)
	if !strings.Contains(empty, "naozhi_zz_hist_ms_bucket{le=\"+Inf\"} 0\n") ||
		!strings.Contains(empty, "naozhi_zz_hist_ms_count 0\n") {
		t.Errorf("an unobserved histogram should export zeros\n%s", empty)
	}

	if !HasSchema("naozhi_zz_hist_ms_bucket") {
		t.Error("HasSchema should cover a histogram's bucket map")
	}
	bm.Add("10", 1)
	bm.Add("100", 2)
	bm.Add("+Inf", 3)
	sum.Add(155)
	out := render(t)
	want := "# TYPE naozhi_zz_hist_ms histogram\n" +
		"naozhi_zz_hist_ms_bucket{le=\"10\"} 1\n" +
		"naozhi_zz_hist_ms_bucket{le=\"100\"} 2\n" +
		"naozhi_zz_hist_ms_bucket{le=\"+Inf\"} 3\n" +
		"naozhi_zz_hist_ms_sum 155\n" +
		"naozhi_zz_hist_ms_count 3\n"
	if !strings.Contains(out, want) {
		t.Errorf("output lacks histogram family\n--- want\n%s--- got\n%s", want, out)
	}
	for _, absent := range []string{"# TYPE naozhi_zz_hist_ms_bucket", "# TYPE naozhi_zz_hist_ms_sum", "naozhi_zz_hist_ms_bucket{key"} {
		if strings.Contains(out, absent) {
			t.Errorf("backing var leaked into the generic pass: %q", absent)
		}
	}
}

func TestWrite_HistogramMissingBucketKeyIsZero(t *testing.T) {
	expvar.NewMap("naozhi_zz_hist2_ms_bucket").Add("+Inf", 1)
	expvar.NewInt("naozhi_zz_hist2_ms_sum")
	RegisterHistogram("naozhi_zz_hist2_ms", []string{"10", "+Inf"})
	if out := render(t); !strings.Contains(out, "naozhi_zz_hist2_ms_bucket{le=\"10\"} 0\n") {
		t.Errorf("a bound never hit must export 0\n%s", out)
	}
}

func TestRegisterPanics(t *testing.T) {
	for name, fn := range map[string]func(){
		"labels without names":   func() { RegisterLabels("naozhi_zz_p") },
		"histogram without +Inf": func() { RegisterHistogram("naozhi_zz_p", []string{"10"}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: want panic", name)
				}
			}()
			fn()
		}()
	}
}
