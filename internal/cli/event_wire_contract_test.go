package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEvent_StreamJSONParseContract_R217_ARCH_1 anchors #617: cli.Event is
// simultaneously the stream-json parse target AND the field set every
// downstream consumer (server / discovery / dispatch / session / eventlog)
// reads. The issue's root symptom is "any cli internal field tweak ripples
// through 9 packages" with no fail-fast guard. This test pins the load-
// bearing JSON tags of the parse contract so an accidental rename of an
// internal field surfaces here in CI rather than as a silent stream-json
// parse miss in production.
//
// It is intentionally a CONTRACT test, not a behaviour test: it unmarshals a
// representative claude stream-json line and asserts each tagged field lands
// where consumers expect. Adding a field is fine; renaming/removing a pinned
// tag is the regression we catch.
func TestEvent_StreamJSONParseContract_R217_ARCH_1(t *testing.T) {
	t.Parallel()

	// A system/init frame: model + session_id are the load-bearing fields
	// readLoop forwards to Process.setModel / SetContext.
	const initLine = `{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-x"}`
	var initEv Event
	if err := json.Unmarshal([]byte(initLine), &initEv); err != nil {
		t.Fatalf("init unmarshal: %v", err)
	}
	if initEv.Type != "system" || initEv.SubType != "init" {
		t.Fatalf("type/subtype tag drift: %+v", initEv)
	}
	if initEv.SessionID != "sess-1" || initEv.Model != "claude-x" {
		t.Fatalf("session_id/model tag drift: %+v", initEv)
	}

	// A task_started frame: the agent-linking correlation fields.
	const taskLine = `{"type":"system","subtype":"task_started","task_id":"t1",` +
		`"tool_use_id":"tu1","description":"d","task_type":"in_process_teammate",` +
		`"status":"running","last_tool_name":"Bash",` +
		`"usage":{"total_tokens":42,"tool_uses":3,"duration_ms":1500}}`
	var taskEv Event
	if err := json.Unmarshal([]byte(taskLine), &taskEv); err != nil {
		t.Fatalf("task unmarshal: %v", err)
	}
	switch {
	case taskEv.TaskID != "t1":
		t.Fatalf("task_id tag drift: %q", taskEv.TaskID)
	case taskEv.ToolUseID != "tu1":
		t.Fatalf("tool_use_id tag drift: %q", taskEv.ToolUseID)
	case taskEv.Description != "d":
		t.Fatalf("description tag drift: %q", taskEv.Description)
	case taskEv.TaskType != "in_process_teammate":
		t.Fatalf("task_type tag drift: %q", taskEv.TaskType)
	case taskEv.Status != "running":
		t.Fatalf("status tag drift: %q", taskEv.Status)
	case taskEv.LastToolName != "Bash":
		t.Fatalf("last_tool_name tag drift: %q", taskEv.LastToolName)
	case taskEv.Usage == nil:
		t.Fatal("usage tag drift: nil")
	case taskEv.Usage.TotalTokens != 42 || taskEv.Usage.ToolUses != 3 || taskEv.Usage.DurationMS != 1500:
		t.Fatalf("usage sub-tag drift: %+v", *taskEv.Usage)
	}

	// An assistant frame with a tool_use content block: the Message →
	// Content[].{type,id,name,input} shape that EventEntriesFromEventAt
	// and SubagentLinker depend on.
	const asstLine = `{"type":"assistant","message":{"role":"assistant",` +
		`"content":[{"type":"tool_use","id":"b1","name":"Agent",` +
		`"input":{"description":"go"}}]}}`
	var asstEv Event
	if err := json.Unmarshal([]byte(asstLine), &asstEv); err != nil {
		t.Fatalf("assistant unmarshal: %v", err)
	}
	if asstEv.Message == nil || len(asstEv.Message.Content) != 1 {
		t.Fatalf("message/content tag drift: %+v", asstEv)
	}
	blk := asstEv.Message.Content[0]
	if blk.Type != "tool_use" || blk.ID != "b1" || blk.Name != "Agent" {
		t.Fatalf("content block tag drift: %+v", blk)
	}
	if !strings.Contains(string(blk.Input), `"description":"go"`) {
		t.Fatalf("content block input tag drift: %s", blk.Input)
	}

	// A result frame: total_cost_usd is the one field process tracks.
	const resLine = `{"type":"result","result":"done","total_cost_usd":0.05}`
	var resEv Event
	if err := json.Unmarshal([]byte(resLine), &resEv); err != nil {
		t.Fatalf("result unmarshal: %v", err)
	}
	if resEv.Result != "done" || resEv.CostUSD != 0.05 {
		t.Fatalf("result/total_cost_usd tag drift: %+v", resEv)
	}

	// Passthrough fields: uuid round-trips and isReplay distinguishes ack
	// echoes. Both are pinned because the passthrough slot-matcher silently
	// mis-routes if either tag drifts.
	const replayLine = `{"type":"user","uuid":"u-9","isReplay":true}`
	var rEv Event
	if err := json.Unmarshal([]byte(replayLine), &rEv); err != nil {
		t.Fatalf("replay unmarshal: %v", err)
	}
	if rEv.UUID != "u-9" || !rEv.IsReplay {
		t.Fatalf("uuid/isReplay tag drift: %+v", rEv)
	}
}
