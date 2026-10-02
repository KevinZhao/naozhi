package upstream

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// TestHandleRequest_FetchEvents_SendsTheWireView: the fetch_events RPC answer
// crosses to the primary, so it carries the wire view — no agent-linkage
// fields (the primary never reads jsonl_path) and no credential text.
func TestHandleRequest_FetchEvents_SendsTheWireView(t *testing.T) {
	const (
		key    = "feishu:direct:alice:general"
		secret = "sk-ant-api03-FFFFFFFFFFFFFFFFFFFFFFFF"
	)
	router := makeRouter()
	proc := session.NewTestProcess()
	proc.EventLog.Append(clievent.EventEntry{
		Time: 1000, UUID: "t", Type: "task_start", Summary: "token " + secret,
		TaskType: "local_agent", InternalAgentID: "agent-abc", JSONLPath: "/home/u/.claude/projects/p/s/subagents/agent-abc.jsonl", FirstPromptID: "fp",
	})
	router.InjectSession(key, proc)
	c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(router), nil, nil, Discovery{})

	params, _ := json.Marshal(map[string]any{"key": key, "after": int64(0)})
	result, err := c.handleRequest(context.Background(), context.Background(), node.ReverseMsg{Method: "fetch_events", Params: params}, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("fetch_events: %v", err)
	}
	if !strings.Contains(string(result), `"task_start"`) {
		t.Fatalf("fetch_events lost the entry: %s", result)
	}
	for _, leak := range []string{"jsonl_path", "internal_agent_id", "agent-abc", secret} {
		if strings.Contains(string(result), leak) {
			t.Errorf("fetch_events result carries %q: %s", leak, result)
		}
	}
}
