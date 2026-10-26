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

// TestHandleRunDetail_SessionID_Clean passes through a normal UUID unchanged.
func TestHandleRunDetail_SessionID_Clean(t *testing.T) {
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

	job := cronpkg.Job{
		ID:       strings.Repeat("e", 16),
		Schedule: "@every 1h",
		Prompt:   "clean session test",
		WorkDir:  workDir,
	}
	if err := sched.AddJob(&job); err != nil {
		t.Fatalf("add job: %v", err)
	}

	jobID := job.ID
	runID := strings.Repeat("f", 16)
	cleanSessionID := "aaaaaaaa-1234-1234-1234-000000000002"

	runsDir := filepath.Join(tmp, "runs", jobID)
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("mkdir runs: %v", err)
	}
	now := time.Now().UTC()
	runRec := cronpkg.CronRun{
		RunID:     runID,
		JobID:     jobID,
		State:     cronpkg.RunStateSucceeded,
		Trigger:   cronpkg.TriggerScheduled,
		StartedAt: now.Add(-1 * time.Minute),
		EndedAt:   now,
		SessionID: cleanSessionID,
		WorkDir:   workDir,
	}
	runJSON, err := json.Marshal(runRec)
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	runPath := filepath.Join(runsDir, runID+".json")
	if err := os.WriteFile(runPath, runJSON, 0o600); err != nil {
		t.Fatalf("write run json: %v", err)
	}

	h := &Handlers{deps: Deps{Scheduler: sched}}
	req := httptest.NewRequest(http.MethodGet,
		"/api/cron/runs/"+runID+"?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunDetail(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var resp cronRunDetailView
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}

	// A clean UUID must pass through unchanged.
	if resp.SessionID != cleanSessionID {
		t.Errorf("session_id = %q, want %q — SanitizeForLog must not mangle clean UUIDs",
			resp.SessionID, cleanSessionID)
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
