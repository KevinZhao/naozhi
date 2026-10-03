package main

import (
	"slices"
	"testing"
)

func gates(rs []raise) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Gate)
	}
	return out
}

func TestRaises_GoConstants(t *testing.T) {
	t.Parallel()
	base, head := metrics{}, metrics{}
	goConsts(map[string]string{"a_test.go": "const bareSleepBaseline = 138\nconst (\n\tfooBaseline = 5\n\tbarBaseline = 2\n)\n"}, base)
	goConsts(map[string]string{"a_test.go": "const bareSleepBaseline = 139\nconst (\n\tfooBaseline = 4\n\tbarBaseline = 3\n)\nconst newBaseline = 9\n"}, head)
	rs := raises(base, head)
	if want := []string{"go:a_test.go#barBaseline", "go:a_test.go#bareSleepBaseline"}; !slices.Equal(gates(rs), want) {
		t.Fatalf("raises = %v, want %v (a lowered one and a new ratchet are not raises)", rs, want)
	}
	if rs[1].From != 138 || rs[1].To != 139 {
		t.Errorf("raise = %+v", rs[1])
	}
}

func TestRaises_JSRatchet(t *testing.T) {
	t.Parallel()
	base, head := metrics{}, metrics{}
	if err := jsRatchet(`{"a.js":{"lines":100,"maxFnLines":50,"fnOver100":0},"b.js":{"lines":10,"maxFnLines":5,"fnOver100":0}}`, base); err != nil {
		t.Fatal(err)
	}
	// 20 lines move from a.js to b.js, and a new c.js adds 5: only the sum
	// rose. b.js's longest function grew.
	if err := jsRatchet(`{"a.js":{"lines":80,"maxFnLines":50,"fnOver100":0},"b.js":{"lines":30,"maxFnLines":6,"fnOver100":0},"c.js":{"lines":5,"maxFnLines":1,"fnOver100":0}}`, head); err != nil {
		t.Fatal(err)
	}
	want := []string{"js-ratchet:TOTAL.lines", "js-ratchet:b.js.maxFnLines"}
	if got := gates(raises(base, head)); !slices.Equal(got, want) {
		t.Errorf("raises = %v, want %v", got, want)
	}
}

// A long function or an injection moved into a new file is a new key there,
// so only the totals can see it.
func TestRaises_JSRatchet_NewFileCannotAbsorbALongFunction(t *testing.T) {
	t.Parallel()
	base, head := metrics{}, metrics{}
	if err := jsRatchet(`{"a.js":{"lines":100,"maxFnLines":80,"fnOver100":0,"configureDeps":10}}`, base); err != nil {
		t.Fatal(err)
	}
	if err := jsRatchet(`{"a.js":{"lines":100,"maxFnLines":80,"fnOver100":0,"configureDeps":10},"n.js":{"lines":0,"maxFnLines":150,"fnOver100":1,"configureDeps":3}}`, head); err != nil {
		t.Fatal(err)
	}
	want := []string{"js-ratchet:MAX.maxFnLines", "js-ratchet:TOTAL.configureDeps", "js-ratchet:TOTAL.fnOver100"}
	if got := gates(raises(base, head)); !slices.Equal(got, want) {
		t.Errorf("raises = %v, want %v", got, want)
	}
}

// configureDeps, deadInjections, innerHTMLAssign, htmlInsert and
// lateBindings are gated like lines: only their sum (S20a, #3026 D-S20-4).
// Moving an innerHTML assignment with its function, or counting an injection
// where it lands instead of where it is passed, is not a raise; one more of
// any of them anywhere is.
func TestRaises_JSRatchet_TotalOnlyMetrics(t *testing.T) {
	t.Parallel()
	const b = `{"a.js":{"lines":10,"configureDeps":5,"deadInjections":1,"innerHTMLAssign":2,"htmlInsert":1,"lateBindings":3},` +
		`"b.js":{"lines":10,"configureDeps":0,"deadInjections":0,"innerHTMLAssign":0,"htmlInsert":0,"lateBindings":0}}`
	base := metrics{}
	if err := jsRatchet(b, base); err != nil {
		t.Fatal(err)
	}
	t.Run("moving every one of them to another file", func(t *testing.T) {
		head := metrics{}
		const h = `{"a.js":{"lines":10,"configureDeps":0,"deadInjections":0,"innerHTMLAssign":1,"htmlInsert":0,"lateBindings":0},` +
			`"b.js":{"lines":10,"configureDeps":5,"deadInjections":1,"innerHTMLAssign":1,"htmlInsert":1,"lateBindings":3}}`
		if err := jsRatchet(h, head); err != nil {
			t.Fatal(err)
		}
		if rs := raises(base, head); len(rs) != 0 {
			t.Errorf("raises = %v, want none", rs)
		}
	})
	t.Run("one more of each", func(t *testing.T) {
		head := metrics{}
		const h = `{"a.js":{"lines":10,"configureDeps":5,"deadInjections":1,"innerHTMLAssign":2,"htmlInsert":1,"lateBindings":3},` +
			`"b.js":{"lines":10,"configureDeps":1,"deadInjections":1,"innerHTMLAssign":1,"htmlInsert":1,"lateBindings":1}}`
		if err := jsRatchet(h, head); err != nil {
			t.Fatal(err)
		}
		want := []string{
			"js-ratchet:TOTAL.configureDeps",
			"js-ratchet:TOTAL.deadInjections",
			"js-ratchet:TOTAL.htmlInsert",
			"js-ratchet:TOTAL.innerHTMLAssign",
			"js-ratchet:TOTAL.lateBindings",
		}
		if got := gates(raises(base, head)); !slices.Equal(got, want) {
			t.Errorf("raises = %v, want %v", got, want)
		}
	})
}

// The S20a revision itself: configureDeps moves from the files that pass the
// dependencies (dashboard.js) to the ones that receive them, the sum
// unchanged, and the _global entry and the new metrics appear. None of it is
// a raise.
func TestRaises_JSRatchet_CountingInjectionsWhereTheyLand(t *testing.T) {
	t.Parallel()
	base, head := metrics{}, metrics{}
	if err := jsRatchet(`{"dashboard.js":{"lines":100,"configureDeps":5},"r.js":{"lines":10,"configureDeps":0}}`, base); err != nil {
		t.Fatal(err)
	}
	const h = `{"dashboard.js":{"lines":100,"configureDeps":0,"innerHTMLAssign":4},"r.js":{"lines":10,"configureDeps":5,"deadInjections":1},` +
		`"_global":{"htmlSinks":9,"importCycles":1}}`
	if err := jsRatchet(h, head); err != nil {
		t.Fatal(err)
	}
	if rs := raises(base, head); len(rs) != 0 {
		t.Errorf("raises = %v, want none", rs)
	}
}

// _global is not a file: its metrics are GLOBAL.<name>, ordered, and losing
// them (or the entry) is a raise to -1.
func TestRaises_JSRatchet_Global(t *testing.T) {
	t.Parallel()
	base := metrics{}
	if err := jsRatchet(`{"a.js":{"lines":1},"_global":{"htmlSinks":10,"importCycles":0,"unresolvedImports":0}}`, base); err != nil {
		t.Fatal(err)
	}
	if _, ok := base["js-ratchet:_global.htmlSinks"]; ok {
		t.Error("_global read as a file")
	}
	t.Run("a new import cycle", func(t *testing.T) {
		head := metrics{}
		if err := jsRatchet(`{"a.js":{"lines":1},"_global":{"htmlSinks":9,"importCycles":1,"unresolvedImports":0}}`, head); err != nil {
			t.Fatal(err)
		}
		rs := raises(base, head)
		if want := []string{"js-ratchet:GLOBAL.importCycles"}; !slices.Equal(gates(rs), want) {
			t.Fatalf("raises = %v, want %v (htmlSinks went down)", rs, want)
		}
		if rs[0].From != 0 || rs[0].To != 1 {
			t.Errorf("raise = %+v, want From 0 To 1", rs[0])
		}
	})
	t.Run("deleting the entry", func(t *testing.T) {
		head := metrics{}
		if err := jsRatchet(`{"a.js":{"lines":1}}`, head); err != nil {
			t.Fatal(err)
		}
		want := []string{"js-ratchet:GLOBAL.htmlSinks", "js-ratchet:GLOBAL.importCycles", "js-ratchet:GLOBAL.unresolvedImports"}
		rs := raises(base, head)
		if got := gates(rs); !slices.Equal(got, want) {
			t.Fatalf("raises = %v, want %v", got, want)
		}
		for _, r := range rs {
			if r.To != -1 {
				t.Errorf("gate %s: To = %d, want -1", r.Gate, r.To)
			}
		}
	})
}

// The revision that introduces a metric has no total for it at the base, so
// the new total is a new ratchet, not a raise from zero.
func TestRaises_JSRatchet_IntroducingAMetric(t *testing.T) {
	t.Parallel()
	base, head := metrics{}, metrics{}
	if err := jsRatchet(`{"a.js":{"lines":100,"maxFunctionLines":80}}`, base); err != nil {
		t.Fatal(err)
	}
	if err := jsRatchet(`{"a.js":{"lines":100,"maxFnLines":300,"fnOver100":2}}`, head); err != nil {
		t.Fatal(err)
	}
	if rs := raises(base, head); len(rs) != 0 {
		t.Errorf("raises = %v, want none", rs)
	}
}

func TestRaises_JSDeps(t *testing.T) {
	t.Parallel()
	base, head := metrics{}, metrics{}
	if err := jsDeps(`{"matrix":{"a.js":{"u.js":["esc"]}},"tdz":{},"typeofGuards":{"x.js":1},"bridgeRefs":{}}`, base); err != nil {
		t.Fatal(err)
	}
	if err := jsDeps(`{"matrix":{"a.js":{"u.js":["esc","fetchJSON"]}},"tdz":{},"typeofGuards":{"x.js":0,"y.js":2},"bridgeRefs":{"z.js":1}}`, head); err != nil {
		t.Fatal(err)
	}
	want := []string{"js-deps:bridgeRefs:z.js", "js-deps:matrix:a.js>u.js:fetchJSON", "js-deps:typeofGuards:y.js"}
	if got := gates(raises(base, head)); !slices.Equal(got, want) {
		t.Errorf("raises = %v, want %v", got, want)
	}
}

func TestRaises_Exemptions(t *testing.T) {
	t.Parallel()
	const b = `file_size:
  - path: a.go
    current: 690
    limit: 500
    until: "2027-03-31"
  - path: b.go
    current: 783
    limit: 500
    until: "2027-03-31"
handle_baseline:
  - Server.handleDashboard
`
	const h = `file_size:
  - path: a.go
    current: 691
    limit: 500
    until: "2027-03-31"
  - path: b.go
    current: 700
    limit: 500
    until: "2027-06-30"
  - path: c.go
    current: 600
    limit: 500
    until: "2027-03-31"
handle_baseline:
  - Server.handleDashboard
  - Server.handleNew
`
	base, head := metrics{}, metrics{}
	if err := exemptions(b, base); err != nil {
		t.Fatal(err)
	}
	if err := exemptions(h, head); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"exemptions:file_size:a.go.current",
		"exemptions:file_size:b.go.until",
		"exemptions:file_size:c.go",
		"exemptions:handle_baseline:Server.handleNew",
	}
	if got := gates(raises(base, head)); !slices.Equal(got, want) {
		t.Errorf("raises = %v, want %v", got, want)
	}
}

// Deleting a file's metrics (and TOTAL.lines with them) must surface as a
// raise to -1 even though head no longer carries the key at all (#3025).
func TestRaises_JSRatchet_DeletingBaselineIsARaise(t *testing.T) {
	t.Parallel()
	base, head := metrics{}, metrics{}
	if err := jsRatchet(`{"a.js":{"lines":100,"maxFnLines":80,"fnOver100":0,"configureDeps":2}}`, base); err != nil {
		t.Fatal(err)
	}
	// head carries nothing at all: the whole baseline file was deleted. Only
	// the totals/max are goneIsRaise — js-ratchet --check still holds each
	// file's own metrics shrink-only, so losing a.js's own keys is covered
	// there, not by ratchet-raises.
	want := []string{
		"js-ratchet:MAX.maxFnLines",
		"js-ratchet:TOTAL.configureDeps",
		"js-ratchet:TOTAL.fnOver100",
		"js-ratchet:TOTAL.lines",
	}
	got := gates(raises(base, head))
	if !slices.Equal(got, want) {
		t.Fatalf("raises = %v, want %v", got, want)
	}
	for _, r := range raises(base, head) {
		if r.To != -1 {
			t.Errorf("gate %s: To = %d, want -1 (gone)", r.Gate, r.To)
		}
	}
}

func TestRaises_JSCaps(t *testing.T) {
	t.Parallel()
	base := metrics{}
	const b = `{"maxFnLines":{"default":120,"exempt":["a.js"]},"lines":{"dashboard.js":6511},"sideEffectLegacy":["a.js"],"cycleLegacy":[]}`
	if err := jsCaps(b, base); err != nil {
		t.Fatal(err)
	}
	t.Run("raising the default", func(t *testing.T) {
		head := metrics{}
		const h = `{"maxFnLines":{"default":121,"exempt":["a.js"]},"lines":{"dashboard.js":6511},"sideEffectLegacy":["a.js"],"cycleLegacy":[]}`
		if err := jsCaps(h, head); err != nil {
			t.Fatal(err)
		}
		want := []string{"js-caps:maxFnLines.default"}
		if got := gates(raises(base, head)); !slices.Equal(got, want) {
			t.Errorf("raises = %v, want %v", got, want)
		}
	})
	t.Run("a new exemption", func(t *testing.T) {
		head := metrics{}
		const h = `{"maxFnLines":{"default":120,"exempt":["a.js","b.js"]},"lines":{"dashboard.js":6511},"sideEffectLegacy":["a.js"],"cycleLegacy":[]}`
		if err := jsCaps(h, head); err != nil {
			t.Fatal(err)
		}
		want := []string{"js-caps:exempt:b.js"}
		if got := gates(raises(base, head)); !slices.Equal(got, want) {
			t.Errorf("raises = %v, want %v", got, want)
		}
	})
	t.Run("dropping a clean exemption is free", func(t *testing.T) {
		head := metrics{}
		const h = `{"maxFnLines":{"default":120,"exempt":[]},"lines":{"dashboard.js":6511},"sideEffectLegacy":["a.js"],"cycleLegacy":[]}`
		if err := jsCaps(h, head); err != nil {
			t.Fatal(err)
		}
		if rs := raises(base, head); len(rs) != 0 {
			t.Errorf("raises = %v, want none", rs)
		}
	})
	t.Run("deleting the dashboard.js line cap", func(t *testing.T) {
		head := metrics{}
		const h = `{"maxFnLines":{"default":120,"exempt":["a.js"]},"lines":{},"sideEffectLegacy":["a.js"],"cycleLegacy":[]}`
		if err := jsCaps(h, head); err != nil {
			t.Fatal(err)
		}
		rs := raises(base, head)
		want := []string{"js-caps:lines.dashboard.js"}
		if got := gates(rs); !slices.Equal(got, want) {
			t.Fatalf("raises = %v, want %v", got, want)
		}
		if rs[0].From != 6511 || rs[0].To != -1 {
			t.Errorf("raise = %+v, want From 6511 To -1", rs[0])
		}
	})
	t.Run("a new sideEffectLegacy entry", func(t *testing.T) {
		head := metrics{}
		const h = `{"maxFnLines":{"default":120,"exempt":["a.js"]},"lines":{"dashboard.js":6511},"sideEffectLegacy":["a.js","b.js"],"cycleLegacy":[]}`
		if err := jsCaps(h, head); err != nil {
			t.Fatal(err)
		}
		want := []string{"js-caps:sideEffectLegacy:b.js"}
		if got := gates(raises(base, head)); !slices.Equal(got, want) {
			t.Errorf("raises = %v, want %v", got, want)
		}
	})
	t.Run("a new cycleLegacy entry", func(t *testing.T) {
		head := metrics{}
		const h = `{"maxFnLines":{"default":120,"exempt":["a.js"]},"lines":{"dashboard.js":6511},"sideEffectLegacy":["a.js"],"cycleLegacy":["c.js"]}`
		if err := jsCaps(h, head); err != nil {
			t.Fatal(err)
		}
		want := []string{"js-caps:cycleLegacy:c.js"}
		if got := gates(raises(base, head)); !slices.Equal(got, want) {
			t.Errorf("raises = %v, want %v", got, want)
		}
	})
	// A default no int64 holds must be a decode error, not a float64 the
	// conversion maps to MinInt64 on amd64 (a 120 -> MinInt64 "lowering"
	// that would wave the cap's removal through).
	for _, d := range []string{"1e19", "9223372036854775808", "120.5", "1.2e2"} {
		t.Run("default "+d+" fails closed", func(t *testing.T) {
			head := metrics{}
			h := `{"maxFnLines":{"default":` + d + `,"exempt":[]},"lines":{"dashboard.js":6511},"sideEffectLegacy":["a.js"],"cycleLegacy":[]}`
			if err := jsCaps(h, head); err == nil {
				t.Fatalf("jsCaps accepted default %s: %v", d, head)
			}
		})
	}
	for _, v := range []string{"1e19", "6511.5"} {
		t.Run("lines "+v+" fails closed", func(t *testing.T) {
			head := metrics{}
			h := `{"maxFnLines":{"default":120,"exempt":[]},"lines":{"dashboard.js":` + v + `},"sideEffectLegacy":["a.js"],"cycleLegacy":[]}`
			if err := jsCaps(h, head); err == nil {
				t.Fatalf("jsCaps accepted lines %s: %v", v, head)
			}
		})
	}
	t.Run("leaves: a new one is free, a dropped one raises", func(t *testing.T) {
		lb, lh := metrics{}, metrics{}
		if err := jsCaps(`{"maxFnLines":{"default":120,"exempt":[]},"lines":{},"sideEffectLegacy":[],"cycleLegacy":[],"leaves":["a.js","b.js"]}`, lb); err != nil {
			t.Fatal(err)
		}
		if err := jsCaps(`{"maxFnLines":{"default":120,"exempt":[]},"lines":{},"sideEffectLegacy":[],"cycleLegacy":[],"leaves":["b.js","c.js"]}`, lh); err != nil {
			t.Fatal(err)
		}
		rs := raises(lb, lh)
		if want := []string{"js-caps:leaf:a.js"}; !slices.Equal(gates(rs), want) {
			t.Fatalf("raises = %v, want %v", rs, want)
		}
		if rs[0].To != -1 {
			t.Errorf("raise = %+v, want To -1", rs[0])
		}
	})
	// S20k: injectionLegacy is shrink-only; a receiver put back on it is a
	// raise, one drained off it is free.
	t.Run("injectionLegacy: a new entry raises, a dropped one is free", func(t *testing.T) {
		lb, lh := metrics{}, metrics{}
		if err := jsCaps(`{"maxFnLines":{"default":120,"exempt":[]},"lines":{},"sideEffectLegacy":[],"cycleLegacy":[],"injectionLegacy":["a.js:configureA","b.js:configureB"]}`, lb); err != nil {
			t.Fatal(err)
		}
		if err := jsCaps(`{"maxFnLines":{"default":120,"exempt":[]},"lines":{},"sideEffectLegacy":[],"cycleLegacy":[],"injectionLegacy":["b.js:configureB","c.js:wireC"]}`, lh); err != nil {
			t.Fatal(err)
		}
		rs := raises(lb, lh)
		if want := []string{"js-caps:injectionLegacy:c.js:wireC"}; !slices.Equal(gates(rs), want) {
			t.Fatalf("raises = %v, want %v", rs, want)
		}
	})
	t.Run("deleting the whole caps file", func(t *testing.T) {
		head := metrics{}
		// jsCaps(\"\", head) is a no-op, same as the file being gone.
		rs := raises(base, head)
		want := []string{"js-caps:lines.dashboard.js", "js-caps:maxFnLines.default"}
		if got := gates(rs); !slices.Equal(got, want) {
			t.Fatalf("raises = %v, want %v", got, want)
		}
		for _, r := range rs {
			if r.To != -1 {
				t.Errorf("gate %s: To = %d, want -1", r.Gate, r.To)
			}
		}
	})
}

// A pin's sha is not an ordered quantity: ratchet-raises must catch a change
// in either direction, and must not require an entry just because the file
// was created (the first set of pins is recorded, not raised). A pin added
// to an existing document is covered at run() level, in run_test.go.
func TestRaises_GoldenPins(t *testing.T) {
	t.Parallel()
	base := metrics{}
	if err := goldenPins(`{"event_render_unknown.json":"aaaaaaaaaaaa0000"}`, base); err != nil {
		t.Fatal(err)
	}
	t.Run("changing a pin either direction raises", func(t *testing.T) {
		for _, sha := range []string{"ffffffffffff0000", "000000000001ffff"} {
			head := metrics{}
			if err := goldenPins(`{"event_render_unknown.json":"`+sha+`"}`, head); err != nil {
				t.Fatal(err)
			}
			want := []string{"golden:event_render_unknown.json"}
			if got := gates(raises(base, head)); !slices.Equal(got, want) {
				t.Errorf("sha %s: raises = %v, want %v", sha, got, want)
			}
		}
	})
	t.Run("deleting a pin raises", func(t *testing.T) {
		head := metrics{}
		want := []string{"golden:event_render_unknown.json"}
		rs := raises(base, head)
		if got := gates(rs); !slices.Equal(got, want) {
			t.Fatalf("raises = %v, want %v", got, want)
		}
		if rs[0].To != -1 {
			t.Errorf("raise = %+v, want To -1", rs[0])
		}
	})
	t.Run("an unchanged pin does not raise", func(t *testing.T) {
		head := metrics{}
		if err := goldenPins(`{"event_render_unknown.json":"aaaaaaaaaaaa0000"}`, head); err != nil {
			t.Fatal(err)
		}
		if rs := raises(base, head); len(rs) != 0 {
			t.Errorf("raises = %v, want none", rs)
		}
	})
	t.Run("creating the file for the first time does not raise", func(t *testing.T) {
		base, head := metrics{}, metrics{}
		if err := goldenPins(`{"event_render_known.json":"112233445566"}`, head); err != nil {
			t.Fatal(err)
		}
		if rs := raises(base, head); len(rs) != 0 {
			t.Errorf("raises = %v, want none", rs)
		}
	})
}
