package wsproto

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
)

// Every WS frame that carries an EventEntry is built by one of these
// constructors, and each applies clievent.ForWire.
func TestEventFrames_CarryTheWireView(t *testing.T) {
	const secret = "sk-ant-api03-DDDDDDDDDDDDDDDDDDDDDDDD"
	linked := clievent.EventEntry{Type: "task_start", TaskID: "t1", JSONLPath: "/home/u/x.jsonl", InternalAgentID: "agent-x", Summary: "k " + secret}
	for name, frame := range map[string]any{
		"history":     NewHistory(History{Key: "k", Events: []clievent.EventEntry{linked}}),
		"event":       NewEvent(Event{Key: "k", Event: &linked}),
		"agent_event": NewAgentEvent(AgentEvent{Key: "k", Event: &linked, TaskID: "t1"}),
	} {
		data, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{secret, "jsonl_path", "internal_agent_id"} {
			if strings.Contains(string(data), leak) {
				t.Errorf("%s frame carries %q: %s", name, leak, data)
			}
		}
	}
	if linked.JSONLPath == "" || !strings.Contains(linked.Summary, secret) {
		t.Error("a constructor modified the caller's entry")
	}
}
