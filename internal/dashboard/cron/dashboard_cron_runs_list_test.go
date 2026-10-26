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

// TestHandleRunsList_SummarySessionID_Clean is the happy path for
// HandleRunsList: a persisted run is listed and a normal UUID session_id
// passes through unchanged.
func TestHandleRunsList_SummarySessionID_Clean(t *testing.T) {
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
		ID:       strings.Repeat("1", 16),
		Schedule: "@every 1h",
		Prompt:   "clean summary session test",
		WorkDir:  workDir,
	}
	if err := sched.AddJob(&job); err != nil {
		t.Fatalf("add job: %v", err)
	}

	jobID := job.ID
	runID := strings.Repeat("2", 16)
	cleanSessionID := "cccccccc-1234-1234-1234-000000000002"

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
	if err := os.Chtimes(runPath, now.Add(-1*time.Minute), now.Add(-1*time.Minute)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	h := &Handlers{deps: Deps{Scheduler: sched}}
	req := httptest.NewRequest(http.MethodGet,
		"/api/cron/runs?job_id="+jobID, nil)
	w := httptest.NewRecorder()
	h.HandleRunsList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Runs []cronRunSummaryView `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if len(resp.Runs) == 0 {
		t.Fatal("HandleRunsList returned no runs; expected at least one")
	}

	// A clean UUID must pass through unchanged.
	if resp.Runs[0].SessionID != cleanSessionID {
		t.Errorf("runs[0].session_id = %q, want %q — SanitizeForLog must not mangle clean UUIDs",
			resp.Runs[0].SessionID, cleanSessionID)
	}
}
