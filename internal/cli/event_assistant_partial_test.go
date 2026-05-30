package cli

import (
	"encoding/json"
	"testing"
)

// TestAssistantMessageUnmarshalPartialArray verifies that a content array
// containing one malformed block no longer discards the entire message
// (#1484). The good text/tool_use blocks must survive.
func TestAssistantMessageUnmarshalPartialArray(t *testing.T) {
	// Second block has a non-string "text" — strict []ContentBlock decode
	// fails on it, but the surrounding good blocks should be preserved.
	raw := `{"role":"assistant","content":[
		{"type":"text","text":"hello"},
		{"type":"text","text":12345},
		{"type":"tool_use","id":"t1","name":"Read"}
	]}`

	var m AssistantMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal returned error: %v", err)
	}
	if len(m.Content) != 2 {
		t.Fatalf("expected 2 surviving blocks, got %d: %+v", len(m.Content), m.Content)
	}
	if m.Content[0].Text != "hello" {
		t.Errorf("first block text = %q, want %q", m.Content[0].Text, "hello")
	}
	if m.Content[1].Type != "tool_use" || m.Content[1].ID != "t1" {
		t.Errorf("third block not preserved: %+v", m.Content[1])
	}
}

// TestAssistantMessageUnmarshalAllBadArray confirms that an array where every
// element is unparseable still degrades to empty content (no panic / error).
func TestAssistantMessageUnmarshalAllBadArray(t *testing.T) {
	raw := `{"role":"assistant","content":[123,456]}`
	var m AssistantMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal returned error: %v", err)
	}
	if len(m.Content) != 0 {
		t.Fatalf("expected empty content, got %d blocks", len(m.Content))
	}
}

// TestAssistantMessageUnmarshalCleanArray is a regression guard: a fully valid
// array must still take the fast strict-decode path unchanged.
func TestAssistantMessageUnmarshalCleanArray(t *testing.T) {
	raw := `{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`
	var m AssistantMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal returned error: %v", err)
	}
	if len(m.Content) != 2 || m.Content[0].Text != "a" || m.Content[1].Text != "b" {
		t.Fatalf("clean array not decoded correctly: %+v", m.Content)
	}
}
