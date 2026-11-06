package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
)

// apiNode is a node answering `api` calls; it records the last one.
type apiNode struct {
	fakeCapNode
	status string
	got    node.APIRequest
	res    node.APIResponse
	err    error
}

func (n *apiNode) Status() string { return n.status }
func (n *apiNode) CallAPI(_ context.Context, req node.APIRequest) (node.APIResponse, error) {
	n.got = req
	return n.res, n.err
}

func remoteAPIMux(nc node.Conn) *http.ServeMux {
	s := &Server{nodes: newNodeRegistry(map[string]node.Conn{"pc": nc})}
	mux := http.NewServeMux()
	for _, m := range []string{"GET", "POST", "PUT", "DELETE"} {
		mux.HandleFunc(m+" /api/remote/{node}/{path...}", s.handleRemoteAPI)
	}
	return mux
}

func TestHandleRemoteAPI(t *testing.T) {
	planner := map[string]bool{node.CapPlanner: true}
	cases := []struct {
		name, method, url, body string
		node                    *apiNode
		want                    int
		forwarded               bool
	}{
		{name: "forwards a Planner route", method: "POST", url: "/api/remote/pc/tasks/state?x=1", body: `{"id":"task:a","state":"done"}`,
			node: &apiNode{fakeCapNode: fakeCapNode{id: "pc", caps: planner}, status: "ok", res: node.APIResponse{Status: 200, Body: []byte(`{"ok":true}`)}},
			want: 200, forwarded: true},
		{name: "passes the node's own refusal", method: "GET", url: "/api/remote/pc/planner",
			node: &apiNode{fakeCapNode: fakeCapNode{id: "pc", caps: planner}, status: "ok", res: node.APIResponse{Status: 409, Body: []byte(`{"error":"off"}`)}},
			want: 409, forwarded: true},
		{name: "refuses a route outside the list", method: "POST", url: "/api/remote/pc/sessions/send", body: `{}`,
			node: &apiNode{fakeCapNode: fakeCapNode{id: "pc", caps: planner}, status: "ok"}, want: 404},
		{name: "refuses a method the route lacks", method: "DELETE", url: "/api/remote/pc/tasks",
			node: &apiNode{fakeCapNode: fakeCapNode{id: "pc", caps: planner}, status: "ok"}, want: 404},
		{name: "refuses uploads to the node's Planner", method: "POST", url: "/api/remote/pc/planner/send", body: `{"text":"hi","file_ids":["f1"]}`,
			node: &apiNode{fakeCapNode: fakeCapNode{id: "pc", caps: planner}, status: "ok"}, want: 400},
		{name: "a node without a Planner", method: "GET", url: "/api/remote/pc/tasks",
			node: &apiNode{fakeCapNode: fakeCapNode{id: "pc"}, status: "ok"}, want: 501},
		{name: "a node offline", method: "GET", url: "/api/remote/pc/tasks",
			node: &apiNode{fakeCapNode: fakeCapNode{id: "pc", caps: planner}, status: "error"}, want: 503},
		{name: "a node that does not answer in time", method: "GET", url: "/api/remote/pc/tasks",
			node: &apiNode{fakeCapNode: fakeCapNode{id: "pc", caps: planner}, status: "ok", err: context.DeadlineExceeded}, want: 504, forwarded: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body *strings.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			} else {
				body = strings.NewReader("")
			}
			rec := httptest.NewRecorder()
			remoteAPIMux(tc.node).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.url, body))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if forwarded := tc.node.got.Path != ""; forwarded != tc.forwarded {
				t.Fatalf("forwarded = %v, want %v", forwarded, tc.forwarded)
			}
			if !tc.forwarded {
				return
			}
			wantPath := "/api/" + strings.SplitN(strings.TrimPrefix(tc.url, "/api/remote/pc/"), "?", 2)[0]
			if tc.node.got.Method != tc.method || tc.node.got.Path != wantPath || string(tc.node.got.Body) != tc.body {
				t.Fatalf("forwarded %+v", tc.node.got)
			}
			if rec.Code < 300 && rec.Body.String() != string(tc.node.res.Body) {
				t.Fatalf("body = %q", rec.Body.String())
			}
		})
	}
}
