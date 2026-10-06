package workflows

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/session"
)

const (
	storeSID   = "04a8fc10-6fa5-4b8e-82ba-621974425917"
	storeRun   = "wf_2997921d-435"
	storeTask  = "w113pvmto"
	storeEnded = int64(1_791_170_031_458)
)

// restoredRouter is a Router started on a sessions store holding testKey
// in workspace ws with refs on its board, reading claudeDir's projects.
func restoredRouter(t *testing.T, claudeDir, storePath, ws string, refs []workflow.Ref) *session.Router {
	t.Helper()
	entry := map[string]any{"key": testKey, "session_id": storeSID, "workspace": ws, "workflows": refs}
	data, err := json.Marshal([]any{entry})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r := session.NewRouter(session.RouterConfig{MaxProcs: 3, StorePath: storePath, ClaudeDir: claudeDir})
	t.Cleanup(r.Shutdown)
	return r
}

func endedRef() workflow.Ref {
	return workflow.Ref{
		TaskID: storeTask, RunID: storeRun, Name: "probe", SessionID: storeSID, Status: workflow.StatusCompleted,
		StartedAt: storeEnded - 12_699, EndedAt: storeEnded, Counts: workflow.Counts{Total: 3, Done: 3},
	}
}

// TestHandleWorkflow_RestoredRouter serves a workflow a real Router
// restored from its sessions store, with no seam between them: the header
// comes from the store, and with no run directory on disk the ended
// workflow is a 200 with result_unavailable, not a 404.
func TestHandleWorkflow_RestoredRouter(t *testing.T) {
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	router := restoredRouter(t, filepath.Join(tmp, "claude"), filepath.Join(tmp, "sessions.json"), ws, []workflow.Ref{endedRef()})
	h := New(Deps{Router: router, Limiter: allowAll{}})

	resp, _ := decode(t, get(h, q("task_id", storeTask, "rows", "none")))
	w := resp.Workflow
	if w.TaskID != storeTask || w.Name != "probe" || w.Status != workflow.StatusCompleted || w.Counts.Total != 3 || len(w.Agents) != 0 {
		t.Errorf("workflow %+v; want the restored header", w)
	}
	if !resp.ResultUnavailable || resp.Result != nil || resp.RowsMode != RowsNone || len(resp.Epoch) != 16 {
		t.Errorf("unavailable %v result %+v rows_mode %q epoch %q", resp.ResultUnavailable, resp.Result, resp.RowsMode, resp.Epoch)
	}
	if rec := get(h, q("task_id", "w2")); rec.Code != http.StatusNotFound {
		t.Errorf("task not restored: %d, want 404", rec.Code)
	}
}
