package upstream

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
)

// The primary tags a list body as it decodes it off the wire; asking again
// with that tag must get the short unchanged answer from the real connector,
// so the bytes the node hashes are the bytes the primary received.
func TestHandleConn_ListFetchUnchangedOverWire(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proj<&>"), 0o755); err != nil {
		t.Fatal(err)
	}
	mgr, _ := project.NewManager(root, project.PlannerDefaults{})
	mgr.Scan()
	disc, _ := json.Marshal([]map[string]any{{"session_id": "s1", "summary": "a <b> & c"}})

	type outcome struct {
		method      string
		first, next json.RawMessage
	}
	results := make(chan outcome, 3)
	srv := newFakeServer(t, func(conn *websocket.Conn) {
		defer conn.Close()
		handshake(t, conn)
		ask := func(id, method string, params json.RawMessage) json.RawMessage {
			if err := conn.WriteJSON(node.ReverseMsg{Type: "request", ReqID: id, Method: method, Params: params}); err != nil {
				return nil
			}
			conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			for {
				_, data, err := conn.ReadMessage()
				if err != nil {
					return nil
				}
				var resp node.ReverseMsg
				if json.Unmarshal(data, &resp) == nil && resp.Type == "response" && resp.ReqID == id {
					return resp.Result
				}
			}
		}
		for i, m := range []string{"fetch_sessions", "fetch_projects", "fetch_discovered"} {
			first := ask("a"+string(rune('0'+i)), m, nil)
			params, _ := json.Marshal(map[string]string{"etag": node.ListTag(first)})
			results <- outcome{m, first, ask("b"+string(rune('0'+i)), m, params)}
		}
	})

	cfg := &Config{URL: wsURL(srv), NodeID: "node1", Token: "tok"}
	c := New(cfg, testRouter(makeRouter()), mgr, Discovery{
		Sessions: func() (json.RawMessage, error) { return disc, nil },
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runOnce(ctx) //nolint:errcheck
	}()
	for range 3 {
		select {
		case r := <-results:
			var rows []any
			if err := json.Unmarshal(r.first, &rows); err != nil {
				t.Errorf("%s: first answer %s is not a list: %v", r.method, r.first, err)
			}
			if string(r.next) != `{"not_modified":true}` {
				t.Errorf("%s: asked with the tag of what it got, answer = %s", r.method, r.next)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for list answers")
		}
	}
	cancel()
	<-done
}
