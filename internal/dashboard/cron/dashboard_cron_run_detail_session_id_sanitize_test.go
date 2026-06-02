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

// TestHandleRunDetail_SessionID_Sanitized pins R112714-SEC-5: the SessionID
// field in the HandleRunDetail response must be passed through
// osutil.SanitizeForLog before being serialised into the JSON output, just
// as Prompt and WorkDir already are.
//
// A CronRun persisted before the validator was tightened (or hand-edited on
// disk) can carry control/bidi characters. Without sanitisation a bidi
// override in SessionID could redirect how a terminal or web renderer reads
// the surrounding fields, or inject forged key/value pairs into log lines
// that include the response body.
func TestHandleRunDetail_SessionID_Sanitized(t *testing.T) {
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
	})

	job := cronpkg.Job{
		ID:       strings.Repeat("c", 16),
		Schedule: "@every 1h",
		Prompt:   "sanitize test",
		WorkDir:  workDir,
	}
	if err := sched.AddJob(&job); err != nil {
		t.Fatalf("add job: %v", err)
	}

	jobID := job.ID
	runID := strings.Repeat("d", 16)

	// Craft a SessionID with a bidi override character (U+202E RIGHT-TO-LEFT
	// OVERRIDE) that SanitizeForLog must strip.
	taintedSessionID := "aaaaaaaa-1234-1234-1234-000000000001‮"

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
		SessionID: taintedSessionID,
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

	h := &Handlers{scheduler: sched}
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

	// The bidi override must not appear in the session_id output.
	if strings.ContainsRune(resp.SessionID, '‮') {
		t.Errorf("session_id contains bidi override (U+202E); SanitizeForLog not applied: %q", resp.SessionID)
	}
	// The sanitised output must not be empty (the non-control prefix is preserved).
	if resp.SessionID == "" {
		t.Errorf("session_id is empty after sanitise; expected UUID prefix to be retained")
	}
}

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
	})

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

	h := &Handlers{scheduler: sched}
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
