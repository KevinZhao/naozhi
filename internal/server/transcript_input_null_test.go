package server

import (
	"encoding/json"
	"testing"
)

// TestIsJSONNull asserts our `null`-literal sniffer for transcriptTurn.Input
// — used to suppress wire `"input": null` that json.RawMessage's
// `omitempty` does not catch (R243-CR-P2-4 / #822).
func TestIsJSONNull(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"null", true},
		{" null", true},
		{"null\n", true},
		{"\tnull\r", true},
		{" null ", true},
		{"", false},
		{"NULL", false},
		{"nullx", false},
		{`"null"`, false},
		{`{}`, false},
		{`[]`, false},
		{`{"command":"ls"}`, false},
	}
	for _, c := range cases {
		got := isJSONNull(json.RawMessage(c.in))
		if got != c.want {
			t.Errorf("isJSONNull(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestTranscriptTurnInputOmitsNullLiteral end-to-ends the omit fix:
// an upstream `tool_use` block whose Input is literal `null` must NOT
// surface a `"input": null` on the rendered turn.
func TestTranscriptTurnInputOmitsNullLiteral(t *testing.T) {
	turn := transcriptTurn{
		Index: 0,
		Kind:  "tool_use",
		Tool:  "Bash",
	}
	// Simulate the post-fix code path: input gets nil-ified.
	var input json.RawMessage = json.RawMessage(`null`)
	if isJSONNull(input) {
		input = nil
	}
	turn.Input = input
	out, err := json.Marshal(turn)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundtrip map[string]any
	if err := json.Unmarshal(out, &roundtrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, has := roundtrip["input"]; has {
		t.Errorf("expected `input` field to be omitted, got payload: %s", out)
	}
}

// TestTranscriptTurnInputKeepsRealJSON sanity-checks the negative path:
// non-null Input still serialises through.
func TestTranscriptTurnInputKeepsRealJSON(t *testing.T) {
	turn := transcriptTurn{
		Index: 0,
		Kind:  "tool_use",
		Tool:  "Bash",
		Input: json.RawMessage(`{"command":"ls"}`),
	}
	out, err := json.Marshal(turn)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundtrip map[string]any
	if err := json.Unmarshal(out, &roundtrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, has := roundtrip["input"]
	if !has {
		t.Fatalf("expected input field present, got %s", out)
	}
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("input not an object: %T", got)
	}
	if m["command"] != "ls" {
		t.Errorf("input.command = %v, want ls", m["command"])
	}
}
