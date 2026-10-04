package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/naozhi/naozhi/internal/node"
)

// fakeSubmitter records what the "send" RPC hands the turn pipeline.
type fakeSubmitter struct {
	mu     sync.Mutex
	calls  []fakeSubmit
	status string
	err    error
}

type fakeSubmit struct {
	ctx                  context.Context
	key, text, workspace string
}

func (f *fakeSubmitter) SubmitRelayed(ctx context.Context, key, text, workspace string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeSubmit{ctx: ctx, key: key, text: text, workspace: workspace})
	return f.status, f.err
}

func (f *fakeSubmitter) called() []fakeSubmit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeSubmit(nil), f.calls...)
}

type connCtxMarker struct{}

func sendReq(t *testing.T, params map[string]string) node.ReverseMsg {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return node.ReverseMsg{Method: "send", Params: raw}
}

// The send RPC hands the message to the turn pipeline on connCtx and answers
// with the pipeline's status.
func TestHandleRequest_Send_SubmitsToTheTurnPipeline(t *testing.T) {
	sub := &fakeSubmitter{status: "queued"}
	c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(makeRouter()), nil, nil, Discovery{}, sub)
	appCtx := context.Background()
	connCtx := context.WithValue(context.Background(), connCtxMarker{}, "conn")

	result, err := c.handleRequest(appCtx, connCtx, sendReq(t, map[string]string{"key": "feishu:direct:alice:general", "text": "/new"}), &sync.WaitGroup{})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(result, &got); err != nil || got["status"] != "queued" {
		t.Fatalf("result = %s (%v), want status queued", result, err)
	}
	calls := sub.called()
	if len(calls) != 1 {
		t.Fatalf("submitter calls = %d, want 1", len(calls))
	}
	if c0 := calls[0]; c0.key != "feishu:direct:alice:general" || c0.text != "/new" || c0.workspace != "" {
		t.Errorf("submitted %+v, want the key and the text verbatim, no workspace", c0)
	}
	if calls[0].ctx.Value(connCtxMarker{}) != "conn" {
		t.Error("submitter did not get connCtx")
	}
}

// A workspace override reaches the pipeline as the connector's resolved path.
func TestHandleRequest_Send_PassesTheSanitizedWorkspace(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(root, "proj")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	sub := &fakeSubmitter{status: "accepted"}
	c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(makeRouter()), nil, nil, Discovery{}, sub)
	c.defaultWorkspace = root

	if _, err := c.handleRequest(context.Background(), context.Background(), sendReq(t, map[string]string{"key": "feishu:direct:alice:general", "text": "hi", "workspace": proj + "/"}), &sync.WaitGroup{}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if calls := sub.called(); len(calls) != 1 || calls[0].workspace != proj {
		t.Fatalf("submitter calls = %+v, want one with workspace %q", calls, proj)
	}
}

// A refusal from the pipeline is the RPC's error.
func TestHandleRequest_Send_PipelineErrorIsTheRPCError(t *testing.T) {
	boom := errors.New("会话正忙，消息未送达，请稍后重试")
	sub := &fakeSubmitter{err: boom}
	c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(makeRouter()), nil, nil, Discovery{}, sub)

	_, err := c.handleRequest(context.Background(), context.Background(), sendReq(t, map[string]string{"key": "feishu:direct:alice:general", "text": "hi"}), &sync.WaitGroup{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the pipeline's error", err)
	}
}

// Without a pipeline the connector refuses to send rather than start a turn
// of its own.
func TestHandleRequest_Send_NoPipelineRefuses(t *testing.T) {
	c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(makeRouter()), nil, nil, Discovery{}, nil)

	_, err := c.handleRequest(context.Background(), context.Background(), sendReq(t, map[string]string{"key": "feishu:direct:alice:general", "text": "hi"}), &sync.WaitGroup{})
	if err == nil || !strings.Contains(err.Error(), "send unavailable") {
		t.Fatalf("err = %v, want send unavailable", err)
	}
}

// Every trust-boundary rejection happens before the pipeline sees the send.
func TestHandleRequest_Send_RejectsBeforeThePipeline(t *testing.T) {
	for name, params := range map[string]map[string]string{
		"bad key":                {"key": "bad\x00key", "text": "hi"},
		"text too long":          {"key": "feishu:direct:alice:general", "text": strings.Repeat("x", 5*1024*1024)},
		"workspace traversal":    {"key": "feishu:direct:alice:general", "text": "hi", "workspace": "/home/../etc"},
		"workspace control byte": {"key": "feishu:direct:alice:general", "text": "hi", "workspace": "/home/user\nproj"},
		"workspace with no root": {"key": "feishu:direct:alice:general", "text": "hi", "workspace": "/etc"},
	} {
		t.Run(name, func(t *testing.T) {
			sub := &fakeSubmitter{status: "accepted"}
			c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(makeRouter()), nil, nil, Discovery{}, sub)
			if _, err := c.handleRequest(context.Background(), context.Background(), sendReq(t, params), &sync.WaitGroup{}); err == nil {
				t.Fatal("send accepted, want a rejection")
			}
			if calls := sub.called(); len(calls) != 0 {
				t.Fatalf("pipeline saw a rejected send: %+v", calls)
			}
		})
	}
}

// A workspace outside the node's root is rejected before the pipeline too.
func TestHandleRequest_Send_RejectsWorkspaceOutsideRootBeforeThePipeline(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sub := &fakeSubmitter{status: "accepted"}
	c := New(&Config{URL: "wss://x", NodeID: "n", Token: "t"}, testRouter(makeRouter()), nil, nil, Discovery{}, sub)
	c.defaultWorkspace = root
	if _, err := c.handleRequest(context.Background(), context.Background(), sendReq(t, map[string]string{"key": "feishu:direct:alice:general", "text": "hi", "workspace": outside}), &sync.WaitGroup{}); err == nil {
		t.Fatal("workspace outside the root accepted")
	}
	if calls := sub.called(); len(calls) != 0 {
		t.Fatalf("pipeline saw a rejected send: %+v", calls)
	}
}
