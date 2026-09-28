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

// A long function moved into a new file is a new key there, so only the
// totals can see it.
func TestRaises_JSRatchet_NewFileCannotAbsorbALongFunction(t *testing.T) {
	t.Parallel()
	base, head := metrics{}, metrics{}
	if err := jsRatchet(`{"a.js":{"lines":100,"maxFnLines":80,"fnOver100":0}}`, base); err != nil {
		t.Fatal(err)
	}
	if err := jsRatchet(`{"a.js":{"lines":100,"maxFnLines":80,"fnOver100":0},"n.js":{"lines":0,"maxFnLines":150,"fnOver100":1}}`, head); err != nil {
		t.Fatal(err)
	}
	want := []string{"js-ratchet:MAX.maxFnLines", "js-ratchet:TOTAL.fnOver100"}
	if got := gates(raises(base, head)); !slices.Equal(got, want) {
		t.Errorf("raises = %v, want %v", got, want)
	}
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
