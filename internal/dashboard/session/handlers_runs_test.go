package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	sessionpkg "github.com/naozhi/naozhi/internal/session"
)

const runsTestKey = "feishu:p2p:alice"

// newRunsHandler builds a Handlers backed by a real Router whose run-history
// store persists under a temp dir, then generates `n` completed runs through
// the Send path so the store is populated exactly as production would.
func newRunsHandler(t *testing.T, n int) *Handlers {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	r := sessionpkg.NewRouter(sessionpkg.RouterConfig{MaxProcs: 4, StorePath: storePath})
	t.Cleanup(r.Shutdown)

	sess := r.InjectSession(runsTestKey, &sessionpkg.TestProcess{
		AliveVal: true,
		SendFunc: func(ctx context.Context, text string, imgs []cli.ImageData, on cli.EventCallback) (*cli.SendResult, error) {
			return &cli.SendResult{Text: "ok", CostUSD: 0.01}, nil
		},
	})
	for i := 0; i < n; i++ {
		if _, err := sess.Send(context.Background(), "hi", nil, nil); err != nil {
			t.Fatalf("seed Send: %v", err)
		}
	}
	// Flush the async write worker so the records are on disk / in the ring
	// before the handler reads them. Shutdown (deferred) also closes it, but
	// we need them visible now.
	r.Shutdown()

	return New(Deps{Router: r})
}

func doRuns(t *testing.T, h *Handlers, query string) (*http.Response, runsListResp) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/runs?"+query, nil)
	rec := httptest.NewRecorder()
	h.HandleRuns(rec, req)
	res := rec.Result()
	var body runsListResp
	if res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return res, body
}

func TestHandleRuns_ReturnsRunsAndStats(t *testing.T) {
	h := newRunsHandler(t, 3)
	res, body := doRuns(t, h, "key="+runsTestKey)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if len(body.Runs) != 3 {
		t.Errorf("want 3 runs, got %d", len(body.Runs))
	}
	if body.Stats.Count != 3 || body.Stats.CompletedCnt != 3 {
		t.Errorf("stats wrong: %+v", body.Stats)
	}
	// unix-ms timestamps populated
	if body.Runs[0].StartedAt == 0 {
		t.Error("started_at should be unix-ms, got 0")
	}
	if body.Runs[0].Outcome != "completed" {
		t.Errorf("outcome = %s", body.Runs[0].Outcome)
	}
}

func TestHandleRuns_MissingKey(t *testing.T) {
	h := newRunsHandler(t, 0)
	res, _ := doRuns(t, h, "")
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("missing key: status = %d, want 400", res.StatusCode)
	}
}

func TestHandleRuns_InvalidKey(t *testing.T) {
	h := newRunsHandler(t, 0)
	res, _ := doRuns(t, h, "key=bad%00key")
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("control-char key: status = %d, want 400", res.StatusCode)
	}
}

func TestHandleRuns_EmptySession(t *testing.T) {
	h := newRunsHandler(t, 0)
	res, body := doRuns(t, h, "key=feishu:p2p:nobody")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if len(body.Runs) != 0 || body.Stats.Count != 0 {
		t.Errorf("empty session should yield no runs / zero stats, got %+v", body)
	}
}

func TestHandleRuns_LimitCap(t *testing.T) {
	h := newRunsHandler(t, 5)
	_, body := doRuns(t, h, "key="+runsTestKey+"&limit=2")
	if len(body.Runs) != 2 {
		t.Errorf("limit=2 returned %d runs", len(body.Runs))
	}
	// stats still reflect the full window, not the paginated slice
	if body.Stats.Count != 5 {
		t.Errorf("stats should cover full window: %+v", body.Stats)
	}
}

func TestHandleRuns_BeforePagination(t *testing.T) {
	h := newRunsHandler(t, 5)
	_, all := doRuns(t, h, "key="+runsTestKey)
	if len(all.Runs) < 3 {
		t.Fatalf("need >=3 runs to test pagination, got %d", len(all.Runs))
	}
	pivot := all.Runs[2].StartedAt
	_, older := doRuns(t, h, "key="+runsTestKey+"&before="+itoa(pivot))
	for _, r := range older.Runs {
		if r.StartedAt >= pivot {
			t.Errorf("before=%d returned a run started at %d (not strictly before)", pivot, r.StartedAt)
		}
	}
}

func TestHandleRuns_InvalidLimitAndBefore(t *testing.T) {
	h := newRunsHandler(t, 1)
	if res, _ := doRuns(t, h, "key="+runsTestKey+"&limit=-1"); res.StatusCode != http.StatusBadRequest {
		t.Errorf("limit=-1 status = %d, want 400", res.StatusCode)
	}
	if res, _ := doRuns(t, h, "key="+runsTestKey+"&before=notanum"); res.StatusCode != http.StatusBadRequest {
		t.Errorf("before=notanum status = %d, want 400", res.StatusCode)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
