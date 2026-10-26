package server

import (
	"bytes"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// TestMarshalHistoryFrame_Redacts: the history frame's bytes never carry a
// credential shape or a local agent-linkage field (wsproto.NewHistory applies
// clievent.ForWire on every marshal).
func TestMarshalHistoryFrame_Redacts(t *testing.T) {
	const secret = "sk-ant-api03-BBBBBBBBBBBBBBBBBBBBBBBB"
	entries := []clievent.EventEntry{
		{Type: "text", Time: 100, Summary: "leak " + secret, Detail: "detail " + secret},
		{Type: "task_start", Time: 101, JSONLPath: "/home/u/.claude/projects/p/s/subagents/agent-x.jsonl", InternalAgentID: "agent-x"},
	}

	assertNoSecret := func(t *testing.T, data []byte) {
		t.Helper()
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("secret survived in marshalled history frame: %q", data)
		}
		for _, internal := range []string{"jsonl_path", "internal_agent_id", "/home/u/"} {
			if bytes.Contains(data, []byte(internal)) {
				t.Fatalf("%q survived in marshalled history frame: %q", internal, data)
			}
		}
	}

	data, err := marshalHistoryFrame("k", entries)
	if err != nil {
		t.Fatalf("marshalHistoryFrame: %v", err)
	}
	assertNoSecret(t, data)
}
