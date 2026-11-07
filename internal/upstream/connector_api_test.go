package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

// TestPlannerAPI: a node running a Planner advertises node.CapPlanner and
// runs `api` calls on its mux, the route checked on the node whatever the
// mux holds; one without answers as an older binary does.
func TestPlannerAPI(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/tasks/state", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"q":"` + r.URL.RawQuery + `","body":` + string(b) + `}`))
	})
	// Not an api route: reachable on the mux, refused by the node.
	mux.HandleFunc("POST /api/sessions/send", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	wired := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(makeRouter()), nil, Discovery{}, nil)
	wired.ServePlanner(Planner{API: mux})
	bare := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(makeRouter()), nil, Discovery{}, nil)
	if !slices.Contains(wired.registerCaps(), node.CapPlanner) || slices.Contains(bare.registerCaps(), node.CapPlanner) {
		t.Fatalf("caps: wired %v, bare %v", wired.registerCaps(), bare.registerCaps())
	}

	call := func(c *Connector, req node.APIRequest) (node.APIResponse, error) {
		params, _ := json.Marshal(req)
		raw, err := c.handleRequest(context.Background(), context.Background(), node.ReverseMsg{Method: "api", Params: params}, &sync.WaitGroup{})
		var res node.APIResponse
		if err == nil {
			err = json.Unmarshal(raw, &res)
		}
		return res, err
	}
	res, err := call(wired, node.APIRequest{Method: "POST", Path: "/api/tasks/state", Query: "a=1", Body: []byte(`{"id":"task:x"}`)})
	if err != nil || res.Status != http.StatusAccepted || string(res.Body) != `{"q":"a=1","body":{"id":"task:x"}}` {
		t.Fatalf("allowed route: %+v %v", res, err)
	}
	for _, req := range []node.APIRequest{
		{Method: "POST", Path: "/api/sessions/send"},
		{Method: "GET", Path: "/api/tasks/../sessions/send"},
		{Method: "GET", Path: "/api/tasks/state"},
	} {
		if res, err := call(wired, req); err != nil || res.Status != http.StatusNotFound {
			t.Errorf("%s %s: %+v %v, want 404", req.Method, req.Path, res, err)
		}
	}
	if _, err := call(bare, node.APIRequest{Method: "GET", Path: "/api/tasks"}); err == nil || !strings.Contains(err.Error(), "unknown method") {
		t.Fatalf("bare node: %v, want unknown method", err)
	}
}

// TestFetchSessionsPurpose: fetch_sessions stamps each snapshot's purpose as
// /api/sessions does, so a dispatched session groups as one on the primary.
func TestFetchSessionsPurpose(t *testing.T) {
	r := makeRouter()
	r.InjectSession("task:abc", session.NewTestProcess())
	r.InjectSession("feishu:direct:alice:general", session.NewTestProcess())
	c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(r), nil, Discovery{}, nil)
	c.ServePlanner(Planner{Purpose: func(key string) (string, string) {
		if key == "task:abc" {
			return "task", "README 追加一行"
		}
		return "", ""
	}})
	raw, err := c.handleRequest(context.Background(), context.Background(), node.ReverseMsg{Method: "fetch_sessions"}, &sync.WaitGroup{})
	if err != nil {
		t.Fatal(err)
	}
	var snaps []sessionview.SessionSnapshot
	if err := json.Unmarshal(raw, &snaps); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range snaps {
		got[s.Key] = s.Purpose + "|" + s.PurposeTitle
	}
	if got["task:abc"] != "task|README 追加一行" || got["feishu:direct:alice:general"] != "|" {
		t.Fatalf("purposes %v", got)
	}
	if slices.Contains(c.registerCaps(), node.CapPlanner) {
		t.Fatal("a node stamping purposes without a Planner advertised CapPlanner")
	}
}
