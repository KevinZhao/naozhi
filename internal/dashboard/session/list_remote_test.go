package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/node"
)

// peerAccessor knows one connected node, pc.
type peerAccessor struct {
	noNodeAccessor
	nc node.Conn
}

func (p peerAccessor) NodesSnapshot() map[string]node.Conn { return map[string]node.Conn{"pc": p.nc} }
func (p peerAccessor) KnownNodes() map[string]string       { return map[string]string{"pc": "PC"} }

// TestHandleList_HidesNodePlannerKeys: a node lists its Planner, cron and
// scout sessions like any other; the primary keeps them out of the list as
// it does its own, and keeps the node's dispatched session with its purpose.
func TestHandleList_HidesNodePlannerKeys(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sessions":[{"key":"planner:main"},{"key":"cron:j1"},{"key":"scout:r1"},{"key":"sys:d"},` +
			`{"key":"task:abc","purpose":"task","purpose_title":"整理"},{"key":"dashboard:direct:x:general"}]}`))
	}))
	defer peer.Close()
	nc := node.NewHTTPClient("pc", peer.URL, "", "PC")
	cache := node.NewCacheManager(func() map[string]node.Conn { return map[string]node.Conn{"pc": nc} }, func() {})
	cache.RefreshAll()
	h := New(Deps{
		Router:        newListRouter("feishu:direct:a:general"),
		NodeAccess:    peerAccessor{nc: nc},
		NodeCache:     cache,
		StartedAt:     time.Now(),
		WatchdogNoOut: &atomic.Int64{},
		WatchdogTotal: &atomic.Int64{},
		Planner:       true,
	})
	rec := doList(h, "")
	if got := rec.Header().Get(node.CapsHeader); got != node.CapSendIdempotent+","+node.CapPlanner {
		t.Errorf("%s = %q", node.CapsHeader, got)
	}
	var body struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, s := range body.Sessions {
		keys = append(keys, s["key"].(string))
	}
	want := []string{"feishu:direct:a:general", "task:abc", "dashboard:direct:x:general"}
	if len(keys) != len(want) {
		t.Fatalf("keys %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys %v, want %v", keys, want)
		}
	}
}
