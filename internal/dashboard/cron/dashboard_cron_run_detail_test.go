package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestHandleRunDetailAndList_SessionIDClean is the happy path of the run
// detail and runs list endpoints: a persisted run is served and a normal UUID
// session_id passes through SanitizeForLog unchanged.
func TestHandleRunDetailAndList_SessionIDClean(t *testing.T) {
	t.Parallel()
	const sessionID = "12345678-1234-1234-1234-123456789abc" // fixtureRunWithJSONL's
	h, jobID, runID, _ := fixtureRunWithJSONL(t, []string{`{"type":"user","message":{"role":"user","content":"x"}}`})

	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunDetail(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", w.Code, w.Body.String())
	}
	var detail cronRunDetailView
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if detail.SessionID != sessionID {
		t.Errorf("detail session_id = %q, want %q", detail.SessionID, sessionID)
	}

	w = httptest.NewRecorder()
	h.HandleRunsList(w, httptest.NewRequest(http.MethodGet, "/api/cron/runs?job_id="+jobID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	var list struct {
		Runs []cronRunSummaryView `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if len(list.Runs) != 1 || list.Runs[0].SessionID != sessionID {
		t.Errorf("runs = %+v, want one run with session_id %q", list.Runs, sessionID)
	}
}

// TestHandleRunDetail_CrossOwnership404 pins [R202606-SEC-2]: a run record
// whose persisted JobID differs from the URL job_id must 404, mirroring
// HandleRunTranscript. runstore.Store.Get keys on the disk path today, but a future
// refactor that loosens the key must not silently expose another job's run.
// We stage a record physically under the URL job's runs dir but carrying a
// foreign JobID to exercise the in-handler guard directly.
func TestHandleRunDetail_CrossOwnership404(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	workDir := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}

	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      storePath,
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})

	urlJobID := strings.Repeat("a", 16)     // job_id in the request URL
	foreignJobID := strings.Repeat("b", 16) // the record's actual owner
	runID := strings.Repeat("c", 16)

	runsDir := filepath.Join(tmp, "runs", urlJobID)
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("mkdir runs: %v", err)
	}
	now := time.Now().UTC()
	runRec := cronpkg.CronRun{
		RunID:     runID,
		JobID:     foreignJobID, // mismatch vs the dir it lives under
		State:     cronpkg.RunStateSucceeded,
		Trigger:   cronpkg.TriggerScheduled,
		StartedAt: now.Add(-1 * time.Minute),
		EndedAt:   now,
		WorkDir:   workDir,
	}
	runJSON, err := json.Marshal(runRec)
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runsDir, runID+".json"), runJSON, 0o600); err != nil {
		t.Fatalf("write run json: %v", err)
	}

	h := &Handlers{deps: Deps{Scheduler: sched}}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"?job_id="+urlJobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunDetail(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-ownership status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}
