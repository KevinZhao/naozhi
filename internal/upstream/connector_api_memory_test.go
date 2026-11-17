package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/dashboard/ext/memory"
	"github.com/naozhi/naozhi/internal/node"
)

// TestPlannerAPIMemoriesReadOnly (planner RFC D126, decision B): over the
// reverse link a primary reads this node's 懂你 memories — the list, marked
// not writable, and one file whole — but neither edits nor deletes one,
// whatever the node's mux would serve: those routes are not api routes, and
// the in-process request is not one from loopback.
func TestPlannerAPIMemoriesReadOnly(t *testing.T) {
	claude, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(claude, "projects")
	mem := filepath.Join(projects, "-Users-me-repo", "memory")
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(mem, "style.md")
	if err := os.WriteFile(file, []byte("---\nname: style\n---\n回复先结论\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := memory.New(memory.Deps{ProjectsDir: projects})
	// As server.mountRoutes builds the node mux: only the api routes.
	mux := http.NewServeMux()
	for _, rt := range h.Routes() {
		if node.IsAPIRoute(rt.Pattern) {
			mux.HandleFunc(rt.Pattern, rt.Handler)
		}
	}
	c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(makeRouter()), nil, Discovery{}, nil)
	c.ServePlanner(Planner{API: mux})
	call := func(req node.APIRequest) node.APIResponse {
		t.Helper()
		params, _ := json.Marshal(req)
		raw, err := c.handleRequest(context.Background(), context.Background(), node.ReverseMsg{Method: "api", Params: params}, &sync.WaitGroup{})
		if err != nil {
			t.Fatal(err)
		}
		var res node.APIResponse
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := call(node.APIRequest{Method: "GET", Path: "/api/memories/file", Query: "project=-Users-me-repo&name=style"})
	var got struct {
		Text    string `json:"text"`
		Version string `json:"version"`
	}
	if res.Status != http.StatusOK || json.Unmarshal(res.Body, &got) != nil || got.Text != "---\nname: style\n---\n回复先结论\n" {
		t.Fatalf("forwarded read = %d %s", res.Status, res.Body)
	}
	res = call(node.APIRequest{Method: "GET", Path: "/api/memories"})
	var list struct {
		Writable bool              `json:"writable"`
		Groups   []json.RawMessage `json:"groups"`
	}
	if res.Status != http.StatusOK || json.Unmarshal(res.Body, &list) != nil || list.Writable || len(list.Groups) != 1 {
		t.Fatalf("forwarded list = %d %s", res.Status, res.Body)
	}

	body, _ := json.Marshal(map[string]string{"project": "-Users-me-repo", "name": "style", "text": "pwned", "version": got.Version})
	for _, req := range []node.APIRequest{
		{Method: "PUT", Path: "/api/memories/file", Body: body},
		{Method: "DELETE", Path: "/api/memories/file", Query: "project=-Users-me-repo&name=style&version=" + got.Version},
	} {
		if res := call(req); res.Status != http.StatusNotFound {
			t.Errorf("forwarded %s = %d %s, want 404", req.Method, res.Status, res.Body)
		}
	}
	if data, _ := os.ReadFile(file); string(data) != "---\nname: style\n---\n回复先结论\n" {
		t.Fatalf("a forwarded write changed the file: %q", data)
	}
}
