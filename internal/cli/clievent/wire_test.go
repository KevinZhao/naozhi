package clievent

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const wireSecret = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAA"

func linked() EventEntry {
	return EventEntry{Type: "task_start", TaskID: "t1", TaskType: "in_process_teammate",
		InternalAgentID: "agent-abc", JSONLPath: "/home/u/.claude/projects/p/s/subagents/agent-abc.jsonl", FirstPromptID: "p1"}
}

// ForWire clears the local agent-linkage fields and redacts credential shapes,
// and leaves the rest of the entry alone.
func TestForWire_StripsAndRedacts(t *testing.T) {
	in := []EventEntry{linked(), {Type: "text", Summary: "key is " + wireSecret, Detail: "also " + wireSecret}}
	out := ForWire(in)
	if e := out[0]; e.TaskType != "" || e.InternalAgentID != "" || e.JSONLPath != "" || e.FirstPromptID != "" {
		t.Errorf("linkage fields survived: %+v", e)
	}
	if out[0].TaskID != "t1" || out[0].Type != "task_start" {
		t.Errorf("wire fields lost: %+v", out[0])
	}
	if strings.Contains(out[1].Summary, wireSecret) || strings.Contains(out[1].Detail, wireSecret) {
		t.Errorf("secret survived: %+v", out[1])
	}
}

// Entries alias EventLog's shared ring: ForWire never writes through them, and
// clean input comes back aliased, not copied.
func TestForWire_CopyOnWrite(t *testing.T) {
	in := []EventEntry{{Type: "text", Summary: "clean"}, linked()}
	want := linked()
	out := ForWire(in)
	if !reflect.DeepEqual(in[1], want) {
		t.Errorf("input mutated: %+v", in[1])
	}
	if &out[0] == &in[0] {
		t.Error("a changed slice was returned aliased")
	}
	if out[0].Summary != "clean" {
		t.Errorf("clean entry before the first change corrupted: %+v", out[0])
	}
	clean := []EventEntry{{Type: "text", Summary: "nothing here"}, {Type: "result"}}
	if got := ForWire(clean); &got[0] != &clean[0] {
		t.Error("clean input was copied")
	}
	if got := ForWire(nil); len(got) != 0 {
		t.Errorf("nil input gave %d entries", len(got))
	}
}

func TestForWireOne(t *testing.T) {
	if ForWireOne(nil) != nil {
		t.Error("nil did not stay nil")
	}
	e := linked()
	got := ForWireOne(&e)
	if got == &e || got.JSONLPath != "" || e.JSONLPath == "" {
		t.Errorf("ForWireOne aliased or mutated its input: got=%+v in=%+v", got, e)
	}
}

// The WS schema drops WireOmittedFields from the EventEntry def, so the list
// must be exactly the keys the projection clears: an entry with every field
// set, put through ForWireOne, loses those keys and no others.
func TestWireOmittedFields_MatchProjection(t *testing.T) {
	var full EventEntry
	v := reflect.ValueOf(&full).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("x")
		case reflect.Int, reflect.Int64:
			f.SetInt(1)
		case reflect.Float64:
			f.SetFloat(1)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Slice:
			f.Set(reflect.Append(f, reflect.ValueOf("x")))
		case reflect.Pointer:
			f.Set(reflect.New(f.Type().Elem()))
		default:
			t.Fatalf("field %s: kind %s has no non-zero value here — extend the switch", v.Type().Field(i).Name, f.Kind())
		}
	}
	before, after := jsonKeys(t, &full), jsonKeys(t, ForWireOne(&full))
	if len(before) != v.NumField() {
		t.Fatalf("the full entry marshals %d keys for %d fields: %v", len(before), v.NumField(), before)
	}
	var cleared []string
	for _, k := range before {
		if !slices.Contains(after, k) {
			cleared = append(cleared, k)
		}
	}
	want := slices.Sorted(slices.Values(WireOmittedFields()))
	if !slices.Equal(cleared, want) {
		t.Errorf("ForWireOne clears %v, WireOmittedFields lists %v", cleared, want)
	}
}

func jsonKeys(t *testing.T, e *EventEntry) []string {
	t.Helper()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return slices.Sorted(maps.Keys(m))
}
