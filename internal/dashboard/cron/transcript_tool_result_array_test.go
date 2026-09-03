package cron

import (
	"encoding/json"
	"testing"
)

// #2433 P2: the CLI frequently persists tool_result content as an array of
// content blocks ([{"type":"text","text":…}]) rather than a bare string.
// decodeStringOrBlocks returned ("", blocks) for that shape and
// flattenUserEvent discarded the blocks, so the cron transcript showed an
// empty Output for every such tool_result. Text blocks must be joined
// (newline-separated, mirroring cli.flattenToolResultRaw).
func TestFlattenUserEvent_ToolResultArrayContent(t *testing.T) {
	t.Parallel()
	msg := json.RawMessage(`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"a","is_error":false,"content":[` +
		`{"type":"text","text":"line one"},` +
		`{"type":"tool_reference","tool_use_id":"x"},` +
		`{"type":"text","text":"line two"}]},` +
		`{"type":"tool_result","tool_use_id":"b","is_error":true,"content":"plain string"}]}`)
	out, _, _, parsed := flattenUserEvent(&claudeJSONLEvent{Type: "user", Message: msg}, 0, 0)
	if !parsed || len(out) != 2 {
		t.Fatalf("parsed=%v len(out)=%d (want true / 2)", parsed, len(out))
	}
	if out[0].Kind != "tool_result" || out[0].ToolUseID != "a" {
		t.Fatalf("turn 0 = %+v, want tool_result a", out[0])
	}
	if want := "line one\nline two"; out[0].Output != want {
		t.Errorf("array-content Output=%q want %q", out[0].Output, want)
	}
	if out[0].Status != "ok" {
		t.Errorf("array-content Status=%q want ok", out[0].Status)
	}
	// String form keeps working unchanged.
	if out[1].Output != "plain string" || out[1].Status != "error" {
		t.Errorf("string-content turn = %+v, want Output=plain string Status=error", out[1])
	}
}

// An array with no text blocks (pure tool_reference envelope) still yields a
// turn (the tool call happened) with an empty Output rather than dropping it.
func TestFlattenUserEvent_ToolResultArrayOnlyReferences(t *testing.T) {
	t.Parallel()
	msg := json.RawMessage(`{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"a","content":[{"type":"tool_reference","tool_use_id":"x"}]}]}`)
	out, _, _, parsed := flattenUserEvent(&claudeJSONLEvent{Type: "user", Message: msg}, 0, 0)
	if !parsed || len(out) != 1 {
		t.Fatalf("parsed=%v len(out)=%d (want true / 1)", parsed, len(out))
	}
	if out[0].Output != "" {
		t.Errorf("Output=%q want empty for reference-only array", out[0].Output)
	}
}
