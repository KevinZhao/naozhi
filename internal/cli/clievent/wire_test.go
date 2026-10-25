package clievent

import (
	"reflect"
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
