package upstream

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/clievent"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
)

// TestHandleRequest_FetchEvents_AfterReadmitsWatermarkMillisecond (#2456):
// fetch_events serves the relay's second-subscriber catch-up
// (node/relay.go sendHistoryToClient) and the primary's HTTP proxy for
// ?node=<peer>&after=. Both forward the dashboard's last-rendered ms, so the
// RPC must re-admit that millisecond exactly like the local WS subscribe path
// (server.entriesSinceReconnect) — otherwise a same-ms sibling appended on
// the peer after the client's last delivery is never replayed. The
// dashboard dedups the replayed entry by uuid.
func TestHandleRequest_FetchEvents_AfterReadmitsWatermarkMillisecond(t *testing.T) {
	const key = "feishu:direct:alice:general"
	router := makeRouter()
	proc := session.NewTestProcess()
	proc.EventLog.Append(clievent.EventEntry{Time: 1000, UUID: "old", Type: "user", Summary: "hi"})
	proc.EventLog.Append(clievent.EventEntry{Time: 2000, UUID: "a", Type: "thinking", Summary: "..."})
	router.InjectSession(key, proc)
	c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, router, nil, nil)

	// The dashboard rendered "a" (cursor 2000); its same-ms sibling lands next.
	proc.EventLog.Append(clievent.EventEntry{Time: 2000, UUID: "b", Type: "text", Summary: "answer"})

	params, _ := json.Marshal(map[string]any{"key": key, "after": int64(2000)})
	req := node.ReverseMsg{Method: "fetch_events", Params: params}
	result, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("fetch_events: %v", err)
	}
	var got []clievent.EventEntry
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("decode %s: %v", result, err)
	}
	uuids := make([]string, 0, len(got))
	for _, e := range got {
		uuids = append(uuids, e.UUID)
	}
	if len(got) != 2 || got[0].UUID != "a" || got[1].UUID != "b" {
		t.Fatalf("#2456 regression: fetch_events after=2000 returned %v, want [a b]", uuids)
	}
}
