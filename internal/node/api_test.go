package node

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckAPIRequest(t *testing.T) {
	cases := []struct {
		req APIRequest
		ok  bool
	}{
		{APIRequest{Method: "GET", Path: "/api/tasks"}, true},
		{APIRequest{Method: "POST", Path: "/api/planner/send", Body: []byte(`{"text":"hi"}`)}, true},
		{APIRequest{Method: "GET", Path: "/api/sessions/notes", Query: "key=task%3Aabc"}, true},
		{APIRequest{Method: "GET", Path: "/api/sessions/digest", Query: "key=task%3Aabc"}, true},
		{APIRequest{Method: "POST", Path: "/api/tasks"}, false},
		{APIRequest{Method: "POST", Path: "/api/sessions/send"}, false},
		{APIRequest{Method: "GET", Path: "/api/tasks/../config"}, false},
		{APIRequest{Method: "GET", Path: "/api//tasks"}, false},
		{APIRequest{Method: "GET", Path: "/api/tasks?x=1"}, false},
		{APIRequest{Method: "GET", Path: "/api/tasks\n"}, false},
		{APIRequest{Method: "GET", Path: "/api/tasks", Query: "a\r\nb"}, false},
		{APIRequest{Method: "POST", Path: "/api/planner/send", Body: make([]byte, MaxAPIBody+1)}, false},
	}
	for _, tc := range cases {
		if err := CheckAPIRequest(tc.req); (err == nil) != tc.ok {
			t.Errorf("%s %q %q: err = %v, want ok %v", tc.req.Method, tc.req.Path, tc.req.Query, err, tc.ok)
		}
	}
}

// TestHTTPClientCallAPI: a direct node declares CapPlanner on its
// /api/sessions answers poll by poll; only then are calls forwarded, with
// the path, query, body and token.
func TestHTTPClientCallAPI(t *testing.T) {
	planner := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sessions":
			if planner {
				w.Header().Set(CapsHeader, CapSendIdempotent+","+CapPlanner)
			}
			_, _ = w.Write([]byte(`{"sessions":[]}`))
		case "/api/tasks/state":
			b, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(r.Header.Get("Authorization") + "|" + r.URL.RawQuery + "|" + string(b)))
		}
	}))
	defer srv.Close()
	n := NewHTTPClient("pc", srv.URL, "tok", "PC")
	ctx := context.Background()
	req := APIRequest{Method: "POST", Path: "/api/tasks/state", Query: "a=1", Body: []byte(`{}`)}
	if _, err := n.CallAPI(ctx, req); !errors.Is(err, ErrAPIUnsupported) {
		t.Fatalf("before a poll: %v", err)
	}
	if _, err := n.FetchSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if !n.Meta().HasCap(CapPlanner) {
		t.Fatal("CapPlanner not learned")
	}
	res, err := n.CallAPI(ctx, req)
	if err != nil || res.Status != http.StatusAccepted || string(res.Body) != "Bearer tok|a=1|{}" {
		t.Fatalf("%+v %v", res, err)
	}
	planner = false
	if _, err := n.FetchSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if n.Meta().HasCap(CapPlanner) {
		t.Fatal("CapPlanner kept after the node stopped declaring it")
	}
}

func TestReverseConnCallAPIWithoutCap(t *testing.T) {
	c := &ReverseConn{}
	if _, err := c.CallAPI(context.Background(), APIRequest{Method: "GET", Path: "/api/tasks"}); !errors.Is(err, ErrAPIUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(ErrAPIUnsupported.Error(), "Planner") {
		t.Fatal("message")
	}
}
