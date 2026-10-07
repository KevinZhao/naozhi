//go:build unix

package agentevents

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/claudefs"
	"github.com/naozhi/naozhi/internal/cli/workflow"
	"github.com/naozhi/naozhi/internal/session"
	"github.com/naozhi/naozhi/internal/testhelper"
)

const wfRun = "wf_2997921d-435"

// TestAgentEvents_WorkflowAgentFIFO: a FIFO where the transcript should be
// is a 404, at once.
func TestAgentEvents_WorkflowAgentFIFO(t *testing.T) {
	t.Parallel()
	r := newWFRig(t, nil)
	if err := syscall.Mkfifo(r.path, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	done := make(chan int, 1)
	go func() { done <- r.get(t, wfAgent).Code }()
	select {
	case code := <-done:
		if code != http.StatusNotFound {
			t.Errorf("status %d, want 404", code)
		}
	case <-time.After(5 * time.Second):
		t.Error("agent_events blocked on a FIFO")
		if w, err := os.OpenFile(r.path, os.O_WRONLY, 0); err == nil {
			w.Close()
		}
	}
}

// TestAgentEvents_RestoredRouterWorkflowAgent is §14 PR-13's restart
// acceptance on a real Router: a session restored from its store, with no
// process, drills into an agent of its ended workflow once the result file
// has given the rows their agentIds; a listed agent without a transcript on
// disk is pending.
func TestAgentEvents_RestoredRouterWorkflowAgent(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	claudeDir, ws := filepath.Join(tmp, "claude"), filepath.Join(tmp, "ws")
	projectDir := filepath.Join(claudefs.ProjectsRoot(claudeDir), claudefs.ProjectSlug(ws))
	runDir := claudefs.WorkflowRunDir(claudefs.SubagentsDir(projectDir, wfSID), wfRun)
	resultPath := claudefs.WorkflowResultFile(projectDir, wfSID, wfRun)
	for _, dir := range []string{ws, runDir, filepath.Dir(resultPath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	result, err := os.ReadFile(filepath.Join("..", "..", "..", "cli", "workflow", "testdata", "run", wfRun+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, result, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudefs.SubagentJSONL(runDir, wfAgent), probeTranscript(t), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := workflow.Ref{TaskID: "w113pvmto", RunID: wfRun, Name: "probe", SessionID: wfSID, Status: workflow.StatusCompleted, EndedAt: 1_791_170_031_458}
	entry := map[string]any{"key": testAgentEventsKey, "session_id": wfSID, "workspace": ws, "workflows": []workflow.Ref{ref}}
	store, _ := json.Marshal([]any{entry})
	storePath := filepath.Join(tmp, "sessions.json")
	if err := os.WriteFile(storePath, store, 0o600); err != nil {
		t.Fatal(err)
	}
	router := session.NewRouter(session.RouterConfig{MaxProcs: 3, StorePath: storePath, ClaudeDir: claudeDir})
	t.Cleanup(router.Shutdown)
	h := New(Deps{Router: router, ProjectsRoot: claudefs.ResolvedProjectsRoot(claudeDir)})

	var rec *httptest.ResponseRecorder
	testhelper.Eventually(t, func() bool {
		rec = httptest.NewRecorder()
		h.HandleAgentEvents(rec, agentEventsReq(testAgentEventsKey, wfAgent, "", ""))
		return rec.Code == http.StatusOK
	}, 5*time.Second, "the restored workflow agent was never served")
	if ents := decodeEntries(t, rec); len(ents) != 2 || ents[0].Summary != "Reply with just the number 2+2" {
		t.Errorf("entries %+v", ents)
	}
	if sess := router.SessionFor(testAgentEventsKey); sess == nil || sess.SubagentLinker() != nil {
		t.Errorf("session %v; want one with no linker", sess)
	}
	rec = httptest.NewRecorder()
	h.HandleAgentEvents(rec, agentEventsReq(testAgentEventsKey, "a0ba344a06862740f", "", ""))
	if rec.Code != http.StatusAccepted {
		t.Errorf("a listed agent whose transcript is not on disk: %d, want 202", rec.Code)
	}
}
