package workflows

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/dashboard/httputil"
	"github.com/naozhi/naozhi/internal/session"
)

// GET /api/sessions/workflow?key=&node=&task_id=[&rows=none | &since=&epoch=]
//   → 200 WorkflowResponse | 400 bad param | 404 no such session, task or node
//     | 429 rate limited
//
// 404 means only that the session, the task or the node does not exist: the
// client drops its entry on one. A result file that cannot be read is a 200
// with result_unavailable, never a 404.

// Per-IP limiter parameters, shared with the memory preview (10 rps, burst 20).
const (
	LimiterRate  = 10
	LimiterBurst = 20
)

// How the rows of the workflow are chosen (WorkflowResponse.RowsMode).
const (
	RowsFull  = "full"  // every row
	RowsNone  = "none"  // no rows: header, phases and counts only
	RowsDelta = "delta" // the rows newer than the client's since
)

// resultWait bounds how long a request waits for the result file to be read.
const resultWait = 5 * time.Second

// taskIDRe bounds task_id to CLI's observed shapes (prefix + base36), as the
// agent_events endpoint does.
var taskIDRe = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

// epochRe is a board epoch: 16 hex digits.
var epochRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// WorkflowResponse is the body of GET /api/sessions/workflow. Version is
// the workflow's wire version, in the space of the workflow_state frames.
type WorkflowResponse struct {
	Epoch     string            `json:"epoch"`
	Version   uint64            `json:"version"`
	ServerNow int64             `json:"server_now"` // ms
	RowsMode  string            `json:"rows_mode"`
	Workflow  workflow.WireView `json:"workflow"`
	// Result and Logs come with an ended workflow whose result file was read.
	Result        *WorkflowResult `json:"result,omitempty"`
	Logs          []string        `json:"logs,omitempty"`
	LogsTruncated bool            `json:"logs_truncated,omitempty"`
	// ResultUnavailable: the workflow ended with a run id and its result file
	// is not readable now; ask again later.
	ResultUnavailable bool `json:"result_unavailable,omitempty"`
}

// WorkflowResult is the workflow's result, as JSON text.
type WorkflowResult struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

// Handler serves the workflow endpoint.
type Handler struct {
	router  SessionLookup
	limiter IPLimiter
	// boardFor and now are test seams; production reads the session's board
	// and the wall clock.
	boardFor func(key string) Board
	now      func() time.Time
}

// New constructs a Handler. The limiter is required: an endpoint reading
// the disk must not run unlimited by omission.
func New(d Deps) *Handler {
	if d.Limiter == nil {
		panic("workflows: limiter must be non-nil")
	}
	return &Handler{router: d.Router, limiter: d.Limiter, now: time.Now}
}

// board is the board of key's session; nil when there is no session. A
// session that never had a board gives a nil *WorkflowBoard, whose methods
// read as an empty one (RFC §5.8.1).
func (h *Handler) board(key string) Board {
	if h.boardFor != nil {
		return h.boardFor(key)
	}
	if h.router == nil {
		return nil
	}
	sess := h.router.SessionFor(key)
	if sess == nil {
		return nil
	}
	return sess.WorkflowBoard()
}

// rowQuery is the row mode a request asks for.
type rowQuery struct {
	none  bool
	since uint64
	epoch string // set with since
}

// parseRows reads rows / since / epoch; false for a malformed or conflicting
// combination.
func parseRows(r *http.Request) (rowQuery, bool) {
	q := r.URL.Query()
	var rq rowQuery
	if v, ok := q["rows"]; ok {
		if len(v) != 1 || v[0] != RowsNone {
			return rq, false
		}
		rq.none = true
	}
	_, hasSince := q["since"]
	_, hasEpoch := q["epoch"]
	if hasSince != hasEpoch {
		return rq, false
	}
	if hasSince {
		if rq.none {
			return rq, false
		}
		since, err := strconv.ParseUint(q.Get("since"), 10, 64)
		if err != nil || !epochRe.MatchString(q.Get("epoch")) {
			return rq, false
		}
		rq.since, rq.epoch = since, q.Get("epoch")
	}
	return rq, true
}

// HandleWorkflow serves one workflow of a session: header, phases, counts
// and the rows asked for, plus the result and logs once it has ended.
func (h *Handler) HandleWorkflow(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.AllowRequest(r) {
		w.Header().Set("Retry-After", "1")
		httputil.WriteJSONStatus(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}
	q := r.URL.Query()
	key := q.Get("key")
	if err := session.ValidateSessionKey(key); err != nil {
		http.Error(w, "invalid key parameter", http.StatusBadRequest)
		return
	}
	if node := q.Get("node"); node != "" && node != "local" {
		http.Error(w, "unknown task", http.StatusNotFound)
		return
	}
	taskID := q.Get("task_id")
	if !taskIDRe.MatchString(taskID) {
		http.Error(w, "invalid task_id parameter", http.StatusBadRequest)
		return
	}
	rq, ok := parseRows(r)
	if !ok {
		http.Error(w, "invalid rows, since or epoch parameter", http.StatusBadRequest)
		return
	}

	board := h.board(key)
	if board == nil {
		http.Error(w, "unknown task", http.StatusNotFound)
		return
	}
	pub := board.Published()
	wf := find(pub, taskID)
	if wf == nil {
		http.Error(w, "unknown task", http.StatusNotFound)
		return
	}

	var resp WorkflowResponse
	if workflow.IsTerminal(wf.Status) {
		// Reading the result file may merge rows into the board, so the
		// workflow served is the one after the call.
		ctx, cancel := context.WithTimeout(r.Context(), resultWait)
		cache, st := board.Result(ctx, taskID)
		cancel()
		switch st {
		case session.ResultReady:
			resp.Result = &WorkflowResult{Text: cache.Result, Truncated: cache.ResultTruncated}
			resp.Logs, resp.LogsTruncated = cache.Logs, cache.LogsTruncated
		case session.ResultUnavailable:
			resp.ResultUnavailable = true
		}
		if p := board.Published(); p != nil {
			if nw := find(p, taskID); nw != nil {
				pub, wf = p, nw
			}
		}
	}

	rows := wf.Agents
	resp.RowsMode = RowsFull
	switch {
	case rq.none:
		resp.RowsMode, rows = RowsNone, nil
	case rq.epoch != "" && rq.epoch == pub.Epoch && rq.since <= wf.Version:
		resp.RowsMode, rows = RowsDelta, workflow.RowsAfter(wf.Agents, rq.since)
	}
	resp.Epoch, resp.Version, resp.Workflow = pub.Epoch, wf.Version, wf.Wire(rows)
	// Stamped last: the client calibrates its clock on it, and Result may
	// have waited seconds.
	resp.ServerNow = h.now().UnixMilli()
	httputil.WriteJSON(w, resp)
}

// find is the workflow of taskID in p, nil if none.
func find(p *workflow.Published, taskID string) *workflow.Workflow {
	if p == nil {
		return nil
	}
	for _, w := range p.Workflows {
		if w.TaskID == taskID {
			return w
		}
	}
	return nil
}
