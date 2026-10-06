//go:build unix

package workflows

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/testhelper"
)

// TestHandleWorkflow_RestoredRouterRealRun is §14 PR-10's acceptance on a
// real projects root: a restored ended workflow whose result file is on
// disk is served with its rows, result and logs in every row mode; after a
// restart with the file deleted it is still a 200, with result_unavailable.
func TestHandleWorkflow_RestoredRouterRealRun(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	claudeDir, ws, storePath := filepath.Join(tmp, "claude"), filepath.Join(tmp, "ws"), filepath.Join(tmp, "sessions.json")
	projectDir := filepath.Join(claudefs.ProjectsRoot(claudeDir), claudefs.ProjectSlug(ws))
	resultPath := claudefs.WorkflowResultFile(projectDir, storeSID, storeRun)
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "cli", "workflow", "testdata", "run", storeRun+".json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{ws, claudefs.WorkflowRunDir(claudefs.SubagentsDir(projectDir, storeSID), storeRun), filepath.Dir(resultPath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(resultPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	router := restoredRouter(t, claudeDir, storePath, ws, []workflow.Ref{endedRef()})
	h := New(Deps{Router: router, Limiter: allowAll{}})
	var resp WorkflowResponse
	testhelper.Eventually(t, func() bool {
		resp, _ = decode(t, get(h, q("task_id", storeTask, "rows", "none")))
		return resp.Result != nil
	}, 5*time.Second, "the result file was never served")
	if resp.RowsMode != RowsNone || len(resp.Workflow.Agents) != 0 || resp.Workflow.Source != workflow.SourceResultFile ||
		len(resp.Logs) != 1 || resp.ResultUnavailable || resp.Workflow.Tokens != 53217 {
		t.Errorf("rows=none: mode %q, %d rows, source %s, logs %v, unavailable %v, tokens %d",
			resp.RowsMode, len(resp.Workflow.Agents), resp.Workflow.Source, resp.Logs, resp.ResultUnavailable, resp.Workflow.Tokens)
	}
	full, _ := decode(t, get(h, q("task_id", storeTask)))
	if full.RowsMode != RowsFull || len(full.Workflow.Agents) != 3 || full.Result == nil || full.Result.Text != resp.Result.Text {
		t.Errorf("full: mode %q, %d rows, result %+v", full.RowsMode, len(full.Workflow.Agents), full.Result)
	}

	router.Shutdown()
	if err := os.Remove(resultPath); err != nil {
		t.Fatal(err)
	}
	h = New(Deps{Router: restoredRouter(t, claudeDir, storePath, ws, []workflow.Ref{endedRef()}), Limiter: allowAll{}})
	gone, _ := decode(t, get(h, q("task_id", storeTask, "rows", "none")))
	if !gone.ResultUnavailable || gone.Result != nil || gone.Workflow.Status != workflow.StatusCompleted {
		t.Errorf("after the file was deleted: unavailable %v result %+v status %s", gone.ResultUnavailable, gone.Result, gone.Workflow.Status)
	}
}
