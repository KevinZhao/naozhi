package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/discovery"
)

// fixtureRunWithJSONLFresh mirrors fixtureRunWithJSONL but exposes the
// CronRun.Fresh flag so dual-mode regression tests can pin both
// branches of the timestamp-less filter (R240-SEC-15 / #1046).
//
// Forking rather than expanding fixtureRunWithJSONL's signature keeps
// existing call sites unchanged — every TestTranscript_* helper still
// reads as "default = fresh=false (the leaky case we filter against)".
func fixtureRunWithJSONLFresh(t *testing.T, fresh bool, jsonlLines []string) (h *CronHandlers, jobID, runID, claudeDir string) {
	t.Helper()

	tmp := t.TempDir()
	claudeDir = filepath.Join(tmp, ".claude")
	storePath := filepath.Join(tmp, "cron_jobs.json")
	workDir := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}

	sched := cron.NewScheduler(cron.SchedulerConfig{StorePath: storePath})
	job := cron.Job{
		ID:       strings.Repeat("a", 16),
		Schedule: "@every 1h",
		Prompt:   "transcript fixture",
		WorkDir:  workDir,
	}
	if err := sched.AddJob(&job); err != nil {
		t.Fatalf("add job: %v", err)
	}

	sessionID := "12345678-1234-1234-1234-123456789abc"
	jobID = job.ID
	runID = strings.Repeat("b", 16)

	runsDir := filepath.Join(tmp, "runs", jobID)
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("mkdir runs: %v", err)
	}
	now := time.Now().UTC()
	startedAt := now.Add(-2 * time.Minute)
	endedAt := now
	runRec := cron.CronRun{
		RunID:      runID,
		JobID:      jobID,
		State:      cron.RunStateSucceeded,
		Trigger:    cron.TriggerScheduled,
		StartedAt:  startedAt,
		EndedAt:    endedAt,
		DurationMS: endedAt.Sub(startedAt).Milliseconds(),
		SessionID:  sessionID,
		WorkDir:    workDir,
		Fresh:      fresh,
	}
	runJSON, err := json.Marshal(runRec)
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	runPath := filepath.Join(runsDir, runID+".json")
	if err := os.WriteFile(runPath, runJSON, 0o600); err != nil {
		t.Fatalf("write run json: %v", err)
	}

	projDir := filepath.Join(claudeDir, "projects", discovery.ClaudeProjectSlug(workDir))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	jsonlPath := filepath.Join(projDir, sessionID+".jsonl")
	if err := os.WriteFile(jsonlPath, []byte(strings.Join(jsonlLines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	h = &CronHandlers{
		scheduler: sched,
		claudeDir: claudeDir,
	}
	return h, jobID, runID, claudeDir
}

// TestTranscript_FreshTrue_KeepsTimestampLessEvents pins the positive
// half of R240-SEC-15 / #1046: when the run owns its JSONL exclusively
// (Fresh=true), the time-window filter MUST NOT drop events that lack
// timestamps. Cross-run leakage is impossible because there is no
// "other run" sharing the file, so the safest behaviour is to surface
// the event as a turn (matching the pre-#1046 contract for the fresh
// path). A future refactor that over-corrects by dropping all
// timestamp-less events would silently lose legitimate metadata-only
// events from fresh-context runs; this test catches that drift.
//
// Pairs with TestTranscript_FreshFalse_DropsTimestampLessEvents which
// pins the negative half. Together they lock both branches of the
// `else if !run.Fresh { continue }` gate in handleRunTranscript.
func TestTranscript_FreshTrue_KeepsTimestampLessEvents(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339Nano)

	// Mix one in-window dated event + one un-dated event. Under
	// Fresh=true the un-dated event must NOT be dropped — there's no
	// adjacent-run risk so it's safe to attribute to this run.
	lines := []string{
		`{"type":"user","timestamp":"` + now + `","message":{"role":"user","content":"in-window prompt"}}`,
		// Un-dated assistant event with a recognisable marker. With
		// Fresh=true this must surface in the response.
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"FRESH_KEEPS_THIS"}]}}`,
	}

	w := httptest.NewRecorder()
	h, jobID, runID, _ := fixtureRunWithJSONLFresh(t, true, lines)
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"/transcript?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	h.handleRunTranscript(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp transcriptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	foundUndated := false
	for _, tr := range resp.Turns {
		if strings.Contains(tr.Text, "FRESH_KEEPS_THIS") {
			foundUndated = true
		}
	}
	if !foundUndated {
		t.Errorf("Fresh=true dropped timestamp-less event — over-correction regression of #1046; turns=%+v", resp.Turns)
	}
}
