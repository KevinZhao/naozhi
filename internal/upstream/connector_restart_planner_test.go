package upstream

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session/sessionview"
)

type stubPlannerResolver struct {
	key  string
	opts sessionview.AgentOpts
	ok   bool
}

func (s stubPlannerResolver) ResolveForPlannerKey(string) (string, sessionview.AgentOpts, bool) {
	return s.key, s.opts, s.ok
}

// resetRecorder records ResetAndRecreate instead of spawning a CLI.
type resetRecorder struct {
	testRouterAdapter
	resets int
	key    string
	opts   sessionview.AgentOpts
}

func (r *resetRecorder) ResetAndRecreate(_ context.Context, key string, opts sessionview.AgentOpts) (Session, error) {
	r.resets++
	r.key, r.opts = key, opts
	return nil, nil
}

// TestHandleRequest_RestartPlanner_ResolverIsTheOnlyOptsSource: restart_planner
// spawns with exactly the resolver's key and opts, and without a resolver it
// refuses even when the project manager knows the project (#3300).
func TestHandleRequest_RestartPlanner_ResolverIsTheOnlyOptsSource(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "myproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "myproj", "CLAUDE.md"), []byte("# p"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr, err := project.NewManager(root, project.PlannerDefaults{Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	mgr.Scan()
	if mgr.Get("myproj") == nil {
		t.Fatal("fixture project not scanned")
	}
	cfg := &Config{URL: "wss://x", NodeID: "n", Token: "t"}
	params, _ := json.Marshal(map[string]string{"project_name": "myproj"})
	req := node.ReverseMsg{Method: "restart_planner", Params: params}

	t.Run("nil resolver refuses", func(t *testing.T) {
		rec := &resetRecorder{testRouterAdapter: testRouterAdapter{makeRouter()}}
		c := New(cfg, rec, mgr, nil, Discovery{}, nil)
		_, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{})
		if err == nil || !strings.Contains(err.Error(), "projects not configured") {
			t.Fatalf("err = %v, want projects not configured", err)
		}
		if rec.resets != 0 {
			t.Fatalf("ResetAndRecreate called %d times without a resolver", rec.resets)
		}
	})

	t.Run("resolver opts pass through", func(t *testing.T) {
		want := sessionview.AgentOpts{
			Exempt: true, Workspace: filepath.Join(root, "myproj"), Model: "opus",
			Backend: "kiro", AccessProfile: "1p", SystemPrompt: "plan",
		}
		res := stubPlannerResolver{key: "project:myproj:planner", opts: want, ok: true}
		rec := &resetRecorder{testRouterAdapter: testRouterAdapter{makeRouter()}}
		c := New(cfg, rec, mgr, res, Discovery{}, nil)
		if _, err := c.handleRequest(context.Background(), context.Background(), req, &sync.WaitGroup{}); err != nil {
			t.Fatalf("restart_planner: %v", err)
		}
		if rec.resets != 1 || rec.key != res.key || !reflect.DeepEqual(rec.opts, want) {
			t.Fatalf("ResetAndRecreate(%d calls, key %q, opts %+v), want 1 call, %q, %+v",
				rec.resets, rec.key, rec.opts, res.key, want)
		}
	})
}
