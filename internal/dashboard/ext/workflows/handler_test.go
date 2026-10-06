package workflows

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/session"
)

const (
	testKey   = "dashboard:direct:u1:general"
	testEpoch = "00112233aabbccdd"
	nowMS     = int64(1_791_000_123_000)
)

type allowAll struct{}

func (allowAll) Allow(string) bool               { return true }
func (allowAll) AllowRequest(*http.Request) bool { return true }

type denyAll struct{}

func (denyAll) Allow(string) bool               { return false }
func (denyAll) AllowRequest(*http.Request) bool { return false }

// fakeBoard publishes what a test sets and answers Result as configured.
// merge, when set, replaces the publication during Result, as a read that
// merged the file's rows would.
type fakeBoard struct {
	mu      sync.Mutex
	pub     *workflow.Published
	cache   *workflow.ResultCache
	status  session.ResultStatus
	merge   *workflow.Published
	calls   int
	lastCtx context.Context
}

func (b *fakeBoard) Published() *workflow.Published {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pub
}

func (b *fakeBoard) Result(ctx context.Context, _ string) (*workflow.ResultCache, session.ResultStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.lastCtx = ctx
	if b.merge != nil {
		b.pub = b.merge
	}
	return b.cache, b.status
}

func rowsAt(revs ...uint64) []workflow.Agent {
	rows := make([]workflow.Agent, len(revs))
	for i, rev := range revs {
		rows[i] = workflow.Agent{Index: i + 1, Label: "agent", State: workflow.AgentDone, Rev: rev}
	}
	return rows
}

func wf(id string, st workflow.Status, version uint64, rows []workflow.Agent) *workflow.Workflow {
	return &workflow.Workflow{
		TaskID: id, RunID: "wf_2997921d-435", Name: "probe", Status: st, Version: version, Agents: rows,
		Source: workflow.SourceStream, Counts: workflow.Counts{Total: len(rows), Done: len(rows)},
	}
}

func pub(wfs ...*workflow.Workflow) *workflow.Published {
	return workflow.NewPublished(testEpoch, wfs, nil)
}

func newHandler(b Board) *Handler {
	h := New(Deps{Limiter: allowAll{}})
	h.boardFor = func(string) Board { return b }
	h.now = func() time.Time { return time.UnixMilli(nowMS) }
	return h
}

func get(h *Handler, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/workflow?"+query, nil)
	rec := httptest.NewRecorder()
	h.HandleWorkflow(rec, req)
	return rec
}

func q(extra ...string) string {
	v := url.Values{"key": {testKey}, "task_id": {"w1"}}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	return v.Encode()
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) (WorkflowResponse, map[string]json.RawMessage) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp WorkflowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	return resp, raw
}

func TestHandleWorkflow_RowModes(t *testing.T) {
	board := &fakeBoard{pub: pub(wf("w1", workflow.StatusRunning, 9, rowsAt(3, 7, 9)))}
	h := newHandler(board)
	cases := []struct {
		name     string
		query    string
		mode     string
		wantRows int
	}{
		{"default is full", q(), RowsFull, 3},
		{"rows=none", q("rows", "none"), RowsNone, 0},
		{"since inside the epoch", q("since", "3", "epoch", testEpoch), RowsDelta, 2},
		{"since at the version", q("since", "9", "epoch", testEpoch), RowsDelta, 0},
		{"since ahead of the version", q("since", "10", "epoch", testEpoch), RowsFull, 3},
		{"since from another epoch", q("since", "3", "epoch", "ffffffffffffffff"), RowsFull, 3},
		{"since zero", q("since", "0", "epoch", testEpoch), RowsDelta, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := decode(t, get(h, tc.query))
			if resp.RowsMode != tc.mode || len(resp.Workflow.Agents) != tc.wantRows {
				t.Errorf("rows_mode %q with %d rows, want %q with %d", resp.RowsMode, len(resp.Workflow.Agents), tc.mode, tc.wantRows)
			}
			if string(raw["rows_mode"]) != `"`+tc.mode+`"` {
				t.Errorf("rows_mode on the wire: %s", raw["rows_mode"])
			}
			var wv map[string]json.RawMessage
			if err := json.Unmarshal(raw["workflow"], &wv); err != nil {
				t.Fatal(err)
			}
			if string(wv["agents"]) == "null" {
				t.Error("agents encoded as null; an empty set is []")
			}
			if resp.Epoch != testEpoch || resp.Version != 9 || resp.Workflow.Version != 9 || resp.ServerNow != nowMS {
				t.Errorf("epoch %q version %d/%d server_now %d", resp.Epoch, resp.Version, resp.Workflow.Version, resp.ServerNow)
			}
		})
	}
	// A delta carries exactly the rows past since, whole.
	resp, _ := decode(t, get(h, q("since", "3", "epoch", testEpoch)))
	if got := resp.Workflow.Agents; got[0].Rev != 7 || got[1].Rev != 9 {
		t.Errorf("delta rows %+v, want revs 7 and 9", got)
	}
	if board.calls != 0 {
		t.Errorf("Result called %d times for a running workflow", board.calls)
	}
}

func TestHandleWorkflow_Validation(t *testing.T) {
	h := newHandler(&fakeBoard{pub: pub(wf("w1", workflow.StatusRunning, 1, nil))})
	bad := map[string]string{
		"missing key":             "task_id=w1",
		"key with a control byte": "key=a%01b&task_id=w1",
		"key too long":            "key=" + strings.Repeat("k", 1000) + "&task_id=w1",
		"missing task_id":         "key=" + url.QueryEscape(testKey),
		"task_id upper case":      q("task_id", "W1"),
		"task_id too long":        q("task_id", strings.Repeat("a", 33)),
		"task_id with a slash":    q("task_id", "w/1"),
		"rows other than none":    q("rows", "full"),
		"rows empty":              q("rows", ""),
		"rows with since":         q("rows", "none", "since", "1", "epoch", testEpoch),
		"since without epoch":     q("since", "1"),
		"epoch without since":     q("epoch", testEpoch),
		"since not a number":      q("since", "x", "epoch", testEpoch),
		"since negative":          q("since", "-1", "epoch", testEpoch),
		"epoch malformed":         q("since", "1", "epoch", "zz"),
		"epoch with upper case":   q("since", "1", "epoch", strings.ToUpper(testEpoch)),
		"since overflowing":       q("since", "99999999999999999999", "epoch", testEpoch),
	}
	for name, query := range bad {
		if rec := get(h, query); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
	// A bad key is a 400 even for a remote node: validation comes first.
	if rec := get(h, "key=a%01b&task_id=w1&node=other"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad key with a remote node: %d, want 400", rec.Code)
	}
	if rec := get(h, q("node", "local")); rec.Code != http.StatusOK {
		t.Errorf("node=local: %d, want 200", rec.Code)
	}
}

func TestHandleWorkflow_NotFound(t *testing.T) {
	board := &fakeBoard{pub: pub(wf("w1", workflow.StatusRunning, 1, nil))}
	h := newHandler(board)
	if rec := get(h, q("node", "peer-2")); rec.Code != http.StatusNotFound {
		t.Errorf("remote node: %d, want 404", rec.Code)
	}
	// The node is judged before the task_id shape.
	if rec := get(h, q("node", "peer-2", "task_id", "W")); rec.Code != http.StatusNotFound {
		t.Errorf("remote node with a bad task_id: %d, want 404", rec.Code)
	}
	if rec := get(h, q("task_id", "w2")); rec.Code != http.StatusNotFound {
		t.Errorf("task not on the board: %d, want 404", rec.Code)
	}
	h.boardFor = func(string) Board { return nil }
	if rec := get(h, q()); rec.Code != http.StatusNotFound {
		t.Errorf("no board: %d, want 404", rec.Code)
	}
	empty := &fakeBoard{}
	h.boardFor = func(string) Board { return empty }
	if rec := get(h, q()); rec.Code != http.StatusNotFound {
		t.Errorf("board that never published: %d, want 404", rec.Code)
	}
}

// TestHandleWorkflow_RealSessions runs the production lookup against a
// router: a key with no session, and a session that never held a process
// (nil board), are both 404 and neither panics.
func TestHandleWorkflow_RealSessions(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{MaxProcs: 3})
	router.InjectSession(testKey, nil)
	h := New(Deps{Router: router, Limiter: allowAll{}})
	if rec := get(h, q()); rec.Code != http.StatusNotFound {
		t.Errorf("session without a board: %d, want 404", rec.Code)
	}
	if rec := get(h, "key=dashboard:direct:u2:general&task_id=w1"); rec.Code != http.StatusNotFound {
		t.Errorf("no such session: %d, want 404", rec.Code)
	}
	var nilRouter SessionLookup
	if rec := get(New(Deps{Router: nilRouter, Limiter: allowAll{}}), q()); rec.Code != http.StatusNotFound {
		t.Errorf("no router: %d, want 404", rec.Code)
	}
}

func TestHandleWorkflow_Result(t *testing.T) {
	ended := wf("w1", workflow.StatusCompleted, 12, rowsAt(5, 12))
	cache := &workflow.ResultCache{Result: `{"answer":"Paris"}`, Logs: []string{"a", "b"}, LogsTruncated: true}
	t.Run("ready", func(t *testing.T) {
		board := &fakeBoard{pub: pub(ended), cache: cache, status: session.ResultReady}
		for _, query := range []string{q(), q("rows", "none"), q("since", "5", "epoch", testEpoch)} {
			resp, _ := decode(t, get(newHandler(board), query))
			if resp.Result == nil || resp.Result.Text != `{"answer":"Paris"}` || resp.Result.Truncated ||
				len(resp.Logs) != 2 || !resp.LogsTruncated || resp.ResultUnavailable {
				t.Errorf("%s: result %+v logs %v truncated %v unavailable %v", query, resp.Result, resp.Logs, resp.LogsTruncated, resp.ResultUnavailable)
			}
		}
	})
	t.Run("truncated result", func(t *testing.T) {
		board := &fakeBoard{pub: pub(ended), cache: &workflow.ResultCache{Result: "x", ResultTruncated: true}, status: session.ResultReady}
		resp, raw := decode(t, get(newHandler(board), q()))
		if !resp.Result.Truncated || string(raw["logs"]) != "" {
			t.Errorf("truncated %v, logs %s", resp.Result.Truncated, raw["logs"])
		}
	})
	t.Run("unavailable is a 200", func(t *testing.T) {
		board := &fakeBoard{pub: pub(ended), status: session.ResultUnavailable}
		resp, raw := decode(t, get(newHandler(board), q("rows", "none")))
		if !resp.ResultUnavailable || resp.Result != nil || resp.Logs != nil {
			t.Errorf("unavailable %v result %+v logs %v", resp.ResultUnavailable, resp.Result, resp.Logs)
		}
		if _, ok := raw["result"]; ok {
			t.Error("result key present")
		}
	})
	t.Run("an ended workflow with nothing to read", func(t *testing.T) {
		board := &fakeBoard{pub: pub(ended), status: session.ResultNone}
		resp, raw := decode(t, get(newHandler(board), q()))
		if resp.ResultUnavailable || resp.Result != nil {
			t.Errorf("unavailable %v result %+v", resp.ResultUnavailable, resp.Result)
		}
		for _, k := range []string{"result", "logs", "logs_truncated", "result_unavailable"} {
			if _, ok := raw[k]; ok {
				t.Errorf("%s present", k)
			}
		}
	})
	t.Run("never asked for a running workflow", func(t *testing.T) {
		board := &fakeBoard{pub: pub(wf("w1", workflow.StatusRunning, 3, nil)), cache: cache, status: session.ResultReady}
		resp, _ := decode(t, get(newHandler(board), q()))
		if board.calls != 0 || resp.Result != nil || resp.ResultUnavailable {
			t.Errorf("Result called %d times, result %+v", board.calls, resp.Result)
		}
	})
	t.Run("the wait is bounded", func(t *testing.T) {
		board := &fakeBoard{pub: pub(ended), status: session.ResultUnavailable}
		get(newHandler(board), q())
		dl, ok := board.lastCtx.Deadline()
		if !ok || time.Until(dl) > resultWait {
			t.Errorf("Result's context deadline %v (set %v)", dl, ok)
		}
	})
}

// TestHandleWorkflow_ServesTheMergedState: the first read of a restored
// workflow's file merges its rows into the board while Result runs; the
// response carries them, with the version they brought.
func TestHandleWorkflow_ServesTheMergedState(t *testing.T) {
	before := wf("w1", workflow.StatusCompleted, 4, nil)
	after := wf("w1", workflow.StatusCompleted, 6, rowsAt(6, 6, 6))
	board := &fakeBoard{pub: pub(before), merge: pub(after), cache: &workflow.ResultCache{Result: "{}"}, status: session.ResultReady}
	resp, _ := decode(t, get(newHandler(board), q()))
	if len(resp.Workflow.Agents) != 3 || resp.Version != 6 || resp.Result == nil {
		t.Errorf("%d rows at version %d, result %+v; want the merged state", len(resp.Workflow.Agents), resp.Version, resp.Result)
	}
	// A task evicted while its result was read is served as it was.
	board = &fakeBoard{pub: pub(before), merge: pub(), status: session.ResultUnavailable}
	if resp, _ := decode(t, get(newHandler(board), q())); resp.Version != 4 {
		t.Errorf("evicted during Result: version %d, want the one read before", resp.Version)
	}
}

func TestHandleWorkflow_RateLimited(t *testing.T) {
	board := &fakeBoard{pub: pub(wf("w1", workflow.StatusRunning, 1, nil))}
	h := newHandler(board)
	h.limiter = denyAll{}
	rec := get(h, q())
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("status %d Retry-After %q, want 429 with a Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	// Limited before anything is looked up, a bad request included.
	if rec := get(h, "key=a%01b"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("bad request under the limit: %d, want 429", rec.Code)
	}
}

func TestNewRequiresALimiter(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New without a limiter did not panic")
		}
	}()
	New(Deps{})
}

func TestRoutes(t *testing.T) {
	routes := New(Deps{Limiter: allowAll{}}).Routes()
	if len(routes) != 1 || routes[0].Pattern != "GET /api/sessions/workflow" {
		t.Errorf("routes %+v, want the one workflow endpoint (v4's /workflow_agent is gone)", routes)
	}
}
