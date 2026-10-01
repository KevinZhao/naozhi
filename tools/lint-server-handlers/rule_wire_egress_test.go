package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures are a throwaway module written into t.TempDir: stub clievent /
// wsproto packages at the real module-relative paths, a sink package standing
// in for the egress helpers, and one package per case.
var egressStubs = map[string]string{
	"internal/cli/clievent/clievent.go": `package clievent

type EventEntry struct {
	Type      string ` + "`json:\"type\"`" + `
	JSONLPath string ` + "`json:\"jsonl_path,omitempty\"`" + `
}

func ForWire(e []EventEntry) []EventEntry {
	out := make([]EventEntry, len(e))
	for i := range e {
		out[i] = EventEntry{Type: e[i].Type}
	}
	return out
}

func ForWireOne(e *EventEntry) *EventEntry {
	if e == nil {
		return nil
	}
	return &EventEntry{Type: e.Type}
}
`,
	"internal/wsproto/wsproto.go": `package wsproto

import "example.com/fx/internal/cli/clievent"

type History struct {
	Type   string
	Events []clievent.EventEntry
}

type Event struct {
	Type  string
	Event *clievent.EventEntry
}

func NewHistory(f History) History { f.Events = clievent.ForWire(f.Events); return f }

func NewEvent(f Event) Event { f.Event = clievent.ForWireOne(f.Event); return f }
`,
	"internal/sink/sink.go": `package sink

import "example.com/fx/internal/cli/clievent"

type EventSink interface{ SendJSON(v any) }

func WriteJSON(v any) {}

type Payload struct{ Data any }

type Hidden struct {
	Events []clievent.EventEntry ` + "`json:\"-\"`" + `
	events []clievent.EventEntry
}

func Raw() []clievent.EventEntry { return nil }

func RawPair() ([]clievent.EventEntry, error) { return nil, nil }
`,
}

const egressImports = `import (
	"encoding/json"
	"slices"
	"sync"

	"example.com/fx/internal/cli/clievent"
	"example.com/fx/internal/sink"
	"example.com/fx/internal/wsproto"
)

var (
	_ = json.Marshal
	_ = slices.Max([]int{1})
	_ sync.Mutex
	_ clievent.EventEntry
	_ sink.EventSink
	_ wsproto.History
)
`

type egressCase struct {
	name string
	want string // violation | clean | nosite
	body string
}

var egressCases = []egressCase{
	// Each regression shape from the S13b design, caught.
	{"thunkliteral", "violation", `func F(ev clievent.EventEntry) func() any {
	return func() any { return wsproto.Event{Type: "event", Event: &ev} }
}`},
	{"thunkraw", "violation", `func F(ev clievent.EventEntry) func() any { return func() any { return ev } }`},
	{"ifacemethod", "violation", `func F(s sink.EventSink, entries []clievent.EventEntry) { s.SendJSON(entries) }`},
	{"writeback", "violation", `func F(s sink.EventSink, raw []clievent.EventEntry) {
	h := wsproto.NewHistory(wsproto.History{})
	h.Events = raw
	s.SendJSON(h)
}`},
	{"fieldwrite", "violation", `func F(raw []clievent.EventEntry) {
	msg := wsproto.History{Type: "events"}
	msg.Events = raw
	sink.WriteJSON(msg)
}`},
	{"mapany", "violation", `func F(entries []clievent.EventEntry) { sink.WriteJSON(map[string]any{"events": entries}) }`},
	{"varany", "violation", `func F(entries []clievent.EventEntry) { var v any = entries; sink.WriteJSON(v) }`},
	{"ifacefield", "violation", `func F(entries []clievent.EventEntry) { sink.WriteJSON(sink.Payload{Data: entries}) }`},
	{"generic", "violation", `func send[T any](s sink.EventSink, v T) { s.SendJSON(v) }

func F(s sink.EventSink, entries []clievent.EventEntry) { send(s, entries) }`},
	{"entryliteral", "violation", `func F() { sink.WriteJSON(clievent.EventEntry{Type: "text"}) }`},
	{"addrtaken", "violation", `func F(raw []clievent.EventEntry) {
	h := wsproto.NewHistory(wsproto.History{})
	p := &h
	p.Events = raw
	sink.WriteJSON(h)
}`},
	{"alias", "violation", `func F(raw []clievent.EventEntry) {
	e := clievent.ForWire(raw)
	e2 := e
	e2[0] = raw[0]
	sink.WriteJSON(e)
}`},
	{"send", "violation", `func F(entries []clievent.EventEntry) { ch := make(chan any, 1); ch <- entries }`},
	{"appendany", "violation", `func F(entries []clievent.EventEntry) []any { return append([]any{}, entries) }`},
	{"tuple", "violation", `func F() (any, error) { return sink.RawPair() }`},
	{"conversion", "violation", `func F() { _ = any(sink.Raw()) }`},
	{"rangeraw", "violation", `func F(raw []clievent.EventEntry) {
	for _, e := range raw {
		sink.WriteJSON(e)
	}
}`},
	{"otherresult", "violation", `func F() { sink.WriteJSON(sink.Raw()) }`},
	{"closureparam", "violation", `func F() {
	f := func(entries []clievent.EventEntry) { sink.WriteJSON(entries) }
	f(nil)
}`},
	{"ptrmethod", "violation", `type T struct{ Events []clievent.EventEntry }

func (t *T) Set(r []clievent.EventEntry) { t.Events = r }

func F(raw []clievent.EventEntry) {
	v := T{}
	v.Set(raw)
	sink.WriteJSON(v)
}`},
	{"encodeelsewhere", "violation", `func F(e clievent.EventEntry) { _ = json.NewEncoder(nil).Encode(e) }`},

	// Wire views, accepted.
	{"projected", "clean", `func F(raw []clievent.EventEntry) { sink.WriteJSON(clievent.ForWire(raw)) }`},
	{"constructor", "clean", `func F(s sink.EventSink, raw []clievent.EventEntry) {
	s.SendJSON(wsproto.NewHistory(wsproto.History{Events: raw}))
	s.SendJSON(wsproto.NewEvent(wsproto.Event{Event: &raw[0]}))
}`},
	{"carrier", "clean", `func F(raw []clievent.EventEntry) {
	sink.WriteJSON(wsproto.History{Type: "history", Events: clievent.ForWire(raw)})
	sink.WriteJSON(&wsproto.Event{Event: clievent.ForWireOne(&raw[0])})
}`},
	{"local", "clean", `func F(raw []clievent.EventEntry) {
	msg := wsproto.History{Type: "events"}
	msg.Type = "history"
	msg.Events = clievent.ForWire(raw)
	sink.WriteJSON(msg)
}`},
	{"empty", "clean", `func F() {
	sink.WriteJSON([]clievent.EventEntry{})
	sink.WriteJSON(wsproto.History{})
	var h *wsproto.History
	sink.WriteJSON(h)
}`},
	{"rangeprojected", "clean", `func F(raw []clievent.EventEntry) {
	for _, e := range clievent.ForWire(raw) {
		sink.WriteJSON(e)
	}
}`},
	{"decode", "clean", `func F(b []byte) { var raw []clievent.EventEntry; _ = json.Unmarshal(b, &raw) }`},
	{"pool", "clean", `var pool = sync.Pool{New: func() any { s := make([]clievent.EventEntry, 0); return &s }}

func F(p *[]clievent.EventEntry) { pool.Put(p) }`},
	{"persist", "clean", `func F(e clievent.EventEntry) { _ = json.NewEncoder(nil).Encode(e) }`},
	{"allowedgeneric", "clean", `func F(raw []clievent.EventEntry) { slices.Reverse(raw); sink.WriteJSON(clievent.ForWire(raw)) }`},
	{"hidden", "nosite", `func F(raw []clievent.EventEntry) { sink.WriteJSON(sink.Hidden{Events: raw}) }`},
}

var egressTestRules = wireEgressRules{
	Projectors: wireEgress.Projectors,
	Exemptions: []egressExemption{
		{Callee: "encoding/json.Unmarshal", Why: "decode"},
		{Callee: "(*sync.Pool).Put", Why: "pool"},
		{Callee: poolNewCallee, Why: "pool"},
		{Callee: "(*encoding/json.Encoder).Encode", File: "internal/cases/persist/c.go", Why: "persist"},
	},
	Generics:  []string{"slices.Reverse"},
	Sentinels: []egressSentinel{{"internal/cases/projected", "internal/sink.WriteJSON"}},
}

func writeFixtureModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	all := map[string]string{"go.mod": "module example.com/fx\n\ngo 1.26\n"}
	for k, v := range egressStubs {
		all[k] = v
	}
	for k, v := range files {
		all[k] = v
	}
	for name, src := range all {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func loadEgressCases(t *testing.T) *typedProgram {
	t.Helper()
	files := map[string]string{}
	for _, c := range egressCases {
		files["internal/cases/"+c.name+"/c.go"] = "package " + c.name + "\n\n" + egressImports + "\n" + c.body + "\n"
	}
	prog, err := loadTyped(writeFixtureModule(t, files))
	if err != nil {
		t.Fatal(err)
	}
	return prog
}

func TestWireEgress_Cases(t *testing.T) {
	t.Parallel()
	sites, vs := scanWireEgress(loadEgressCases(t), egressTestRules)
	for _, v := range vs {
		if !strings.HasPrefix(v.File, "internal/cases/") {
			t.Errorf("unexpected rule-level violation: %+v", v)
		}
	}
	for _, c := range egressCases {
		dir := "internal/cases/" + c.name + "/"
		var nSites, nViol int
		for _, s := range sites {
			if strings.HasPrefix(s.File, dir) {
				nSites++
			}
		}
		for _, v := range vs {
			if strings.HasPrefix(v.File, dir) {
				nViol++
			}
		}
		switch c.want {
		case "violation":
			if nViol == 0 {
				t.Errorf("%s: no violation reported (%d sites seen)", c.name, nSites)
			}
		case "clean":
			if nViol != 0 || nSites == 0 {
				t.Errorf("%s: want a clean site, got %d sites and %d violations", c.name, nSites, nViol)
			}
		case "nosite":
			if nSites != 0 {
				t.Errorf("%s: a json:\"-\" / unexported path is not JSON-reachable, got %d sites", c.name, nSites)
			}
		}
	}
}

// TestWireEgress_DeadAllowancesAndSentinels: an exemption or generic allowance
// nothing hits, and a sentinel whose package stopped producing sites, fail.
func TestWireEgress_DeadAllowancesAndSentinels(t *testing.T) {
	t.Parallel()
	rules := egressTestRules
	rules.Exemptions = append(append([]egressExemption{}, rules.Exemptions...), egressExemption{Callee: "sort.SliceStable", Why: "dead"})
	rules.Generics = []string{"slices.Reverse", "slices.Clone"}
	rules.Sentinels = []egressSentinel{{"internal/cases/projected", "internal/sink.WriteJSON"}, {"internal/cases/hidden", "internal/sink.WriteJSON"}}
	_, vs := scanWireEgress(loadEgressCases(t), rules)
	want := []string{"exemption sort.SliceStable", "generic allowance slices.Clone", "sentinel internal/sink.WriteJSON no longer receives any EventEntry-reaching value in internal/cases/hidden"}
	for _, w := range want {
		found := 0
		for _, v := range vs {
			if strings.Contains(v.Message, w) {
				found++
			}
		}
		if found != 1 {
			t.Errorf("want exactly one violation containing %q, got %d", w, found)
		}
	}
	for _, v := range vs {
		if strings.Contains(v.Message, "slices.Reverse") || strings.Contains(v.Message, "internal/cases/projected") {
			t.Errorf("a live allowance or sentinel was reported: %s", v.Message)
		}
	}
}

// TestLoadTyped_TypeErrorFails: a module that does not type-check is an
// error, never a quiet pass over the packages that did.
func TestLoadTyped_TypeErrorFails(t *testing.T) {
	t.Parallel()
	dir := writeFixtureModule(t, map[string]string{"internal/broken/b.go": "package broken\n\nvar X int = \"not an int\"\n"})
	if _, err := loadTyped(dir); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("want a load error naming the broken package, got %v", err)
	}
}
