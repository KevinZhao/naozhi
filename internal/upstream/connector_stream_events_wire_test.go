package upstream

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// The events a node streams to its primary leave through clievent.ForWire:
// no local agent-linkage field, no credential shape.
func TestStreamEvents_SendsTheWireView(t *testing.T) {
	t.Parallel()
	const key = "feishu:p2p:alice:general"
	const secret = "sk-ant-api03-EEEEEEEEEEEEEEEEEEEEEEEE"
	r := session.NewRouter(session.RouterConfig{MaxProcs: 1})
	t.Cleanup(r.Shutdown)
	proc := session.NewTestProcess()
	proc.EventLog.Append(clievent.EventEntry{Time: 1000, UUID: "a", Type: "task_start", TaskID: "t1", JSONLPath: "/home/u/x.jsonl", InternalAgentID: "agent-x"})
	proc.EventLog.Append(clievent.EventEntry{Time: 2000, UUID: "b", Type: "text", Summary: "key " + secret})
	r.InjectSession(key, proc)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	var sent []clievent.EventEntry
	writeJSON := func(v any) error {
		if msg, ok := v.(node.ReverseMsg); ok && msg.Type == "events" {
			mu.Lock()
			sent = append(sent, msg.Events...)
			if len(sent) >= 2 {
				cancel()
			}
			mu.Unlock()
		}
		return nil
	}
	notify := make(chan struct{}, 1)
	notify <- struct{}{}
	c := &Connector{router: testRouter(r)}
	c.streamEvents(ctx, writeJSON, key, notify)

	mu.Lock()
	defer mu.Unlock()
	if len(sent) < 2 {
		t.Fatalf("streamed %d events, want 2", len(sent))
	}
	for _, e := range sent {
		if e.JSONLPath != "" || e.InternalAgentID != "" || strings.Contains(e.Summary, secret) {
			t.Errorf("streamed entry is not the wire view: %+v", e)
		}
	}
}
