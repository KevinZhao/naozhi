package clievent

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// TestWorkflowDecodeFromError covers both Field spellings encoding/json uses
// for an error inside a slice element: go1.26 drops the index
// ("workflow_progress.tokens"), go1.27 keeps it ("workflow_progress.0.tokens").
// The CI toolchain and the local one differ, so both must grade the same.
func TestWorkflowDecodeFromError(t *testing.T) {
	t.Parallel()
	failed, partial := WorkflowDecodeFailed, WorkflowDecodePartial
	cases := []struct {
		field  string
		want   WorkflowDecode
		wantOK bool
	}{
		{"workflow_progress", failed, true},   // not an array; [1,2] on go1.26
		{"workflow_progress.0", failed, true}, // [1,2] on go1.27
		{"workflow_progress.12", failed, true},
		{"workflow_progress.type", failed, true},
		{"workflow_progress.0.type", failed, true},
		{"workflow_progress.index", failed, true},
		{"workflow_progress.3.index", failed, true},
		{"workflow_progress.phaseIndex", failed, true},
		{"workflow_progress.agentId", failed, true},
		{"workflow_progress.1.state", failed, true},
		{"workflow_progress.tokens", partial, true},
		{"workflow_progress.0.tokens", partial, true},
		{"workflow_progress.7.label", partial, true},
		{"workflow_progress.blocked", partial, true},
		{"status", 0, false},
		{"workflow_progressx", 0, false},
		{"usage.total_tokens", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		err := fmt.Errorf("wrapped: %w", &json.UnmarshalTypeError{Value: "string", Field: tc.field})
		got, ok := WorkflowDecodeFromError(err)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("Field %q: got (%v, %v), want (%v, %v)", tc.field, got, ok, tc.want, tc.wantOK)
		}
	}
	if _, ok := WorkflowDecodeFromError(errors.New("json: syntax")); ok {
		t.Error("a non-type error was tolerated")
	}
	var syn *json.SyntaxError
	if err := json.Unmarshal([]byte(`{"workflow_progress":[`), &struct{}{}); !errors.As(err, &syn) {
		t.Fatalf("precondition: want a SyntaxError, got %v", err)
	} else if _, ok := WorkflowDecodeFromError(err); ok {
		t.Error("a syntax error was tolerated")
	}
}

func TestWorkflowItemsValid(t *testing.T) { // not parallel: AllocsPerRun
	ph := func(i int) WorkflowItem { return WorkflowItem{Type: WorkflowItemPhase, Index: i} }
	ag := func(i int) WorkflowItem { return WorkflowItem{Type: WorkflowItemAgent, Index: i} }
	cases := []struct {
		name  string
		items []WorkflowItem
		want  bool
	}{
		{"empty snapshot", []WorkflowItem{}, true},
		{"probe shape", []WorkflowItem{ph(1), ph(2), ag(1), ag(2), ag(3)}, true},
		{"unordered but unique", []WorkflowItem{ag(3), ph(2), ag(1), ph(1), ag(2)}, true},
		{"phase and agent share an index", []WorkflowItem{ph(1), ag(1)}, true},
		{"phase index 0 is allowed", []WorkflowItem{ph(0), ag(1)}, true},
		{"unknown type is ignored", []WorkflowItem{ag(1), {Type: "workflow_log"}, {Type: "workflow_log"}}, true},
		{"empty type", []WorkflowItem{ag(1), {}}, false},
		{"agent index 0", []WorkflowItem{ag(0)}, false},
		{"negative agent index", []WorkflowItem{ag(-1)}, false},
		{"duplicate agent, ascending run broken", []WorkflowItem{ag(1), ag(2), ag(2)}, false},
		{"duplicate agent, unordered", []WorkflowItem{ag(2), ag(1), ag(2)}, false},
		{"duplicate phase, adjacent", []WorkflowItem{ph(1), ph(1)}, false},
		{"duplicate phase, unordered", []WorkflowItem{ph(2), ph(1), ph(2)}, false},
	}
	for _, tc := range cases {
		if got := WorkflowItemsValid(tc.items); got != tc.want {
			t.Errorf("%s: WorkflowItemsValid = %v, want %v", tc.name, got, tc.want)
		}
	}
	probe := []WorkflowItem{ph(1), ph(2), ag(1), ag(2), ag(3)}
	if n := testing.AllocsPerRun(10, func() { WorkflowItemsValid(probe) }); n != 0 {
		t.Errorf("ascending snapshot: %v allocs, want 0 (the common shape must not build a set)", n)
	}
}

func TestWorkflowDecodeString(t *testing.T) {
	t.Parallel()
	for d, want := range map[WorkflowDecode]string{
		WorkflowDecodeOK: "ok", WorkflowDecodePartial: "partial", WorkflowDecodeFailed: "failed", 9: "unknown",
	} {
		if got := d.String(); got != want {
			t.Errorf("WorkflowDecode(%d).String() = %q, want %q", d, got, want)
		}
	}
}

// TestWorkflowItemTags pins every json key against the CC 2.1.288 item
// builders, and that the preview fields stay undeclared: they are two thirds
// of a snapshot's bytes.
func TestWorkflowItemTags(t *testing.T) {
	t.Parallel()
	want := []string{
		"type", "index", "title", "label", "phaseIndex", "phaseTitle", "agentId", "model", "state",
		"attempt", "cached", "blocked", "skipped", "queuedAt", "startedAt", "lastProgressAt",
		"durationMs", "tokens", "toolCalls", "lastToolName", "lastToolSummary", "error",
	}
	typ := reflect.TypeFor[WorkflowItem]()
	var got []string
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Tag.Get("json"))
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("WorkflowItem json keys = %v, want %v", got, want)
	}
}
