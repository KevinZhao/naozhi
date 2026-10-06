package workflow

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestWireViewMirrorsWorkflow pins WireView to Workflow's json fields, in
// both directions, and Wire to copying every one of them.
func TestWireViewMirrorsWorkflow(t *testing.T) {
	t.Parallel()
	wt, vt := reflect.TypeOf(Workflow{}), reflect.TypeOf(WireView{})
	wire := map[string]reflect.StructField{}
	for i := 0; i < wt.NumField(); i++ {
		if f := wt.Field(i); f.Tag.Get("json") != "-" {
			wire[f.Name] = f
		}
	}
	if vt.NumField() != len(wire) {
		t.Fatalf("WireView has %d fields, Workflow %d wire fields", vt.NumField(), len(wire))
	}
	for i := 0; i < vt.NumField(); i++ {
		v := vt.Field(i)
		w, ok := wire[v.Name]
		if !ok || w.Type != v.Type || w.Tag != v.Tag {
			t.Errorf("WireView.%s %s %q has no twin in Workflow (%v)", v.Name, v.Type, v.Tag, w)
		}
	}

	// Every wire field non-zero: Wire must carry each into the JSON.
	src := Workflow{}
	rv := reflect.ValueOf(&src).Elem()
	for i := 0; i < rv.NumField(); i++ {
		setNonZero(rv.Field(i), i)
	}
	rows := []Agent{{Index: 9, Label: "only"}}
	got, err := json.Marshal(src.Wire(rows))
	if err != nil {
		t.Fatal(err)
	}
	src.Agents = rows
	want, _ := json.Marshal(src)
	if string(got) != string(want) {
		t.Fatalf("Wire JSON differs from the Workflow's:\n got %s\nwant %s", got, want)
	}
}

func setNonZero(v reflect.Value, seed int) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(fmt.Sprint("s", seed))
	case reflect.Int, reflect.Int64:
		v.SetInt(int64(seed + 1))
	case reflect.Uint64, reflect.Uint8:
		v.SetUint(uint64(seed + 1))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		setNonZero(v.Index(0), seed)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			setNonZero(v.Field(i), seed+i)
		}
	}
}

func TestWireEmptyArrays(t *testing.T) {
	t.Parallel()
	b, _ := json.Marshal((&Workflow{TaskID: "w"}).Wire(nil))
	if !strings.Contains(string(b), `"phases":[]`) || !strings.Contains(string(b), `"agents":[]`) {
		t.Fatalf("nil phases / rows not encoded as []: %s", b)
	}
}

func wf(id string, st Status, ended int64, rows ...Agent) *Workflow {
	return &Workflow{TaskID: id, Status: st, EndedAt: ended, Agents: rows}
}

func TestNewPublished(t *testing.T) {
	t.Parallel()
	w1 := wf("w1", StatusRunning, 0, Agent{Index: 1, AgentID: "a2", PrevAgentIDs: []string{"a1"}, State: AgentRunning}, Agent{Index: 2, State: AgentQueued})
	w2 := wf("w2", StatusCompleted, 5, Agent{Index: 1, AgentID: "b1", State: AgentDone})
	p := NewPublished("e1", []*Workflow{w2, w1}, nil)
	if p.Epoch != "e1" || p.Workflows[0] != w1 || p.Workflows[1] != w2 {
		t.Fatalf("not ordered unsettled first: %v %v", p.Workflows[0].TaskID, p.Workflows[1].TaskID)
	}
	for id, want := range map[string]AgentLoc{
		"a2": {TaskID: "w1", Index: 1, Current: true},
		"a1": {TaskID: "w1", Index: 1},
		"b1": {TaskID: "w2", Index: 1, Current: true},
	} {
		if got, ok := p.Agent(id); !ok || got != want {
			t.Errorf("Agent(%s) = %+v %v, want %+v", id, got, ok, want)
		}
	}
	if _, ok := p.Agent(""); ok {
		t.Error("empty agentId found (row 2 never started)")
	}
	cases := []struct {
		loc  AgentLoc
		want AgentState
		ok   bool
	}{
		{AgentLoc{TaskID: "w1", Index: 1, Current: true}, AgentRunning, true},
		{AgentLoc{TaskID: "w1", Index: 1}, AgentDone, true}, // an earlier attempt is over
		{AgentLoc{TaskID: "w1", Index: 2, Current: true}, AgentQueued, true},
		{AgentLoc{TaskID: "w1", Index: 3, Current: true}, "", false},
		{AgentLoc{TaskID: "w9", Index: 1, Current: true}, "", false},
	}
	for _, c := range cases {
		if got, ok := p.AgentState(c.loc); got != c.want || ok != c.ok {
			t.Errorf("AgentState(%+v) = %q %v, want %q %v", c.loc, got, ok, c.want, c.ok)
		}
	}

	// Same agentIds in the same places: the index is shared, not rebuilt.
	w1b := *w1
	w1b.Tokens = 99
	q := NewPublished("e1", []*Workflow{&w1b, w2}, p)
	if reflect.ValueOf(q.byAgentID).Pointer() != reflect.ValueOf(p.byAgentID).Pointer() {
		t.Error("unchanged agentIds rebuilt the index")
	}
	// A new attempt id, a moved id, a vanished id: rebuilt.
	for name, rows := range map[string][]Agent{
		"new id":      {{Index: 1, AgentID: "a3", PrevAgentIDs: []string{"a1", "a2"}}},
		"id flips":    {{Index: 1, AgentID: "a1", PrevAgentIDs: []string{"a2"}}},
		"id vanishes": {{Index: 1, AgentID: "a2"}},
	} {
		w := *w1
		w.Agents = rows
		r := NewPublished("e1", []*Workflow{&w, w2}, p)
		if reflect.ValueOf(r.byAgentID).Pointer() == reflect.ValueOf(p.byAgentID).Pointer() {
			t.Errorf("%s: stale index reused", name)
		}
		for _, a := range rows {
			if loc, ok := r.Agent(a.AgentID); !ok || !loc.Current {
				t.Errorf("%s: Agent(%s) = %+v %v", name, a.AgentID, loc, ok)
			}
		}
	}

	var nilP *Published
	if _, ok := nilP.Agent("a1"); ok {
		t.Error("nil Published found an agent")
	}
	if _, ok := nilP.AgentState(AgentLoc{}); ok {
		t.Error("nil Published reported a state")
	}
}

// TestNewPublished_DuplicateIDPrefersCurrent: a resumed run's cached rows
// can carry the earlier attempt's agentIds.
func TestNewPublished_DuplicateIDPrefersCurrent(t *testing.T) {
	t.Parallel()
	old := wf("wold", StatusFailed, 1, Agent{Index: 1, AgentID: "a2", PrevAgentIDs: []string{"a1"}})
	resumed := wf("wnew", StatusRunning, 0, Agent{Index: 1, AgentID: "a1"})
	p := NewPublished("e", []*Workflow{old, resumed}, nil)
	if loc, _ := p.Agent("a1"); loc != (AgentLoc{TaskID: "wnew", Index: 1, Current: true}) {
		t.Errorf("a1 → %+v, want the resumed run's current row", loc)
	}
	q := NewPublished("e", []*Workflow{old, resumed}, p)
	if loc, _ := q.Agent("a1"); loc.TaskID != "wnew" {
		t.Errorf("rebuild with duplicates: a1 → %+v", loc)
	}
	// The earlier attempt's place comes first in the walk this time.
	first := wf("w1", StatusFailed, 9, Agent{Index: 1, AgentID: "a2", PrevAgentIDs: []string{"a1"}})
	later := wf("w2", StatusFailed, 1, Agent{Index: 4, AgentID: "a1"})
	if p := NewPublished("e", []*Workflow{later, first}, nil); p.Workflows[0] != first {
		t.Fatal("fixture order: the earlier attempt must be walked first")
	} else if loc, _ := p.Agent("a1"); loc != (AgentLoc{TaskID: "w2", Index: 4, Current: true}) {
		t.Errorf("a1 → %+v, want the current row walked second", loc)
	}
}

// TestAgentEqualIgnoringRev pins the field count and that every field but
// Rev takes part in the comparison.
func TestAgentEqualIgnoringRev(t *testing.T) {
	t.Parallel()
	if n := reflect.TypeOf(Agent{}).NumField(); n != 21 {
		t.Fatalf("Agent has %d fields: compare the new one in AgentEqualIgnoringRev, then update this pin", n)
	}
	base := Agent{Index: 1, AgentID: "a", PrevAgentIDs: []string{"x"}, State: AgentRunning}
	for i := 0; i < reflect.TypeOf(base).NumField(); i++ {
		b := base
		b.PrevAgentIDs = []string{"x"}
		f := reflect.ValueOf(&b).Elem().Field(i)
		name := reflect.TypeOf(base).Field(i).Name
		switch f.Kind() {
		case reflect.String:
			f.SetString(f.String() + "!")
		case reflect.Int, reflect.Int64:
			f.SetInt(f.Int() + 1)
		case reflect.Uint64:
			f.SetUint(f.Uint() + 1)
		case reflect.Bool:
			f.SetBool(!f.Bool())
		case reflect.Slice:
			b.PrevAgentIDs = []string{"x", "y"} // only the attempt history changes
		}
		if eq := AgentEqualIgnoringRev(&base, &b); eq != (name == "Rev") {
			t.Errorf("changing %s: equal = %v", name, eq)
		}
	}
	same := base
	same.PrevAgentIDs = []string{"x"}
	if !AgentEqualIgnoringRev(&base, &same) {
		t.Error("equal rows with distinct PrevAgentIDs slices compared unequal")
	}
}
