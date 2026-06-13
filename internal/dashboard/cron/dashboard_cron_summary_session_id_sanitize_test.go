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

// TestHandleRunsList_SummarySessionID_Sanitized pins R20260613-SEC-11: the
// SessionID field projected by cronSummaryToView (used by HandleList's
// recent_runs preview and HandleRunsList's paginated history) must be passed
// through osutil.SanitizeForLog before being serialised into the JSON output.
//
// Prior to this fix, cronRunDetailView (runs.go) sanitised SessionID but
// cronRunSummaryView (handlers.go) did not. A CronRun persisted before the
// validator was tightened (or hand-edited on disk) could carry bidi override
// or other control characters that reach the browser via HandleRunsList.
func TestHandleRunsList_SummarySessionID_Sanitized(t *testing.T) {
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
		ID:       strings.Repeat("a", 16),
		Schedule: "@every 1h",
		Prompt:   "summary sanitize test",
		WorkDir:  workDir,
	}
	if err := sched.AddJob(&job); err != nil {
		t.Fatalf("add job: %v", err)
	}

	jobID := job.ID
	runID := strings.Repeat("b", 16)

	// Craft a SessionID with a bidi override character (U+202E RIGHT-TO-LEFT
	// OVERRIDE) that SanitizeForLog must strip.
	taintedSessionID := "bbbbbbbb-1234-1234-1234-000000000001‮"

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
	// Set mtime so the runs store picks up the file.
	runPath := filepath.Join(runsDir, runID+".json")
	if err := os.WriteFile(runPath, runJSON, 0o600); err != nil {
		t.Fatalf("write run json: %v", err)
	}
	if err := os.Chtimes(runPath, now.Add(-1*time.Minute), now.Add(-1*time.Minute)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	h := &Handlers{scheduler: sched}
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

	// The bidi override must not appear in any summary session_id output.
	for i, r := range resp.Runs {
		if strings.ContainsRune(r.SessionID, '‮') {
			t.Errorf("runs[%d].session_id contains bidi override (U+202E); SanitizeForLog not applied: %q", i, r.SessionID)
		}
	}
	// The sanitised output must not be empty (the non-control prefix is preserved).
	if resp.Runs[0].SessionID == "" {
		t.Errorf("runs[0].session_id is empty after sanitise; expected UUID prefix to be retained")
	}
}

// TestHandleRunsList_SummarySessionID_Clean passes through a normal UUID unchanged.
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

	h := &Handlers{scheduler: sched}
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

// TestCronSummaryToView_SessionID_Sanitized directly exercises the pure
// projection function without HTTP plumbing. This is a fast unit-level pin
// for the R20260613-SEC-11 fix — the bidi override injected into the input
// struct must not survive into the returned view.
func TestCronSummaryToView_SessionID_Sanitized(t *testing.T) {
	t.Parallel()

	taintedSessionID := "dddddddd-1234-1234-1234-000000000003‮"
	summary := cronpkg.CronRunSummary{
		RunID:     strings.Repeat("e", 16),
		JobID:     strings.Repeat("f", 16),
		State:     cronpkg.RunStateSucceeded,
		Trigger:   cronpkg.TriggerScheduled,
		StartedAt: time.Now().UTC(),
		SessionID: taintedSessionID,
	}

	view := cronSummaryToView(summary)

	if strings.ContainsRune(view.SessionID, '‮') {
		t.Errorf("cronSummaryToView SessionID contains bidi override (U+202E); SanitizeForLog not applied: %q", view.SessionID)
	}
	if view.SessionID == "" {
		t.Errorf("cronSummaryToView SessionID is empty; expected UUID prefix to be retained")
	}
}
