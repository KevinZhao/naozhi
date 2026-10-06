package wsproto_test

import (
	"encoding/json"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/wsproto"
)

// The workflow frames' wire shape: a full frame names no base and carries
// an empty agents array; a delta its base, rows and omitted count; an empty
// board's set is [] rather than null, so the dashboard can tell "no tasks"
// from a missing field.
func TestWorkflowFrames_Shape(t *testing.T) {
	t.Parallel()
	w := &workflow.Workflow{TaskID: "w1", Status: workflow.StatusRunning, Source: workflow.SourceStream, Version: 7}
	cases := []struct {
		name  string
		frame any
		want  string
	}{
		{"full", wsproto.NewWorkflowState(wsproto.WorkflowState{Key: "k", TaskID: "w1", Epoch: "e", Version: 7, Full: true, ServerNow: 9, Workflow: w.Wire(nil)}),
			`{"type":"workflow_state","key":"k","task_id":"w1","epoch":"e","version":7,"full":true,"server_now":9,` +
				`"workflow":{"task_id":"w1","status":"running","counts":{"total":0,"queued":0,"running":0,"done":0,"failed":0,"skipped":0,"stopped":0},"phases":[],"agents":[],"source":"stream","version":7}}`},
		{"delta", wsproto.NewWorkflowState(wsproto.WorkflowState{Key: "k", TaskID: "w1", Epoch: "e", Version: 7, BaseVersion: 5, ServerNow: 9, RowsOmitted: 2,
			Workflow: w.Wire([]workflow.Agent{{Index: 3, Label: "a", State: workflow.AgentDone, Rev: 7}})}),
			`{"type":"workflow_state","key":"k","task_id":"w1","epoch":"e","version":7,"base_version":5,"full":false,"server_now":9,"rows_omitted":2,` +
				`"workflow":{"task_id":"w1","status":"running","counts":{"total":0,"queued":0,"running":0,"done":0,"failed":0,"skipped":0,"stopped":0},"phases":[],` +
				`"agents":[{"index":3,"label":"a","state":"done","rev":7}],"source":"stream","version":7}}`},
		{"empty set", wsproto.NewWorkflowSet(wsproto.WorkflowSet{Key: "k", Epoch: "e", ServerNow: 9}),
			`{"type":"workflow_set","key":"k","epoch":"e","task_ids":[],"server_now":9}`},
		{"set", wsproto.NewWorkflowSet(wsproto.WorkflowSet{Key: "k", Epoch: "e", TaskIDs: []string{"w1", "w2"}, ServerNow: 9}),
			`{"type":"workflow_set","key":"k","epoch":"e","task_ids":["w1","w2"],"server_now":9}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.frame)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, b, c.want)
		}
	}
}
