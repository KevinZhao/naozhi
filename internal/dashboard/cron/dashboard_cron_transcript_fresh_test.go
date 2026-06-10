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
	"github.com/naozhi/naozhi/internal/discovery"
)

// fixtureRunWithJSONLFresh mirrors fixtureRunWithJSONL but exposes the
// CronRun.Fresh flag so dual-mode regression tests can pin both
// branches of the timestamp-less filter (R240-SEC-15 / #1046).
//
// Forking rather than expanding fixtureRunWithJSONL's signature keeps
// existing call sites unchanged — every TestTranscript_* helper still
// reads as "default = fresh=false (the leaky case we filter against)".
func fixtureRunWithJSONLFresh(t *testing.T, fresh bool, jsonlLines []string) (h *Handlers, jobID, runID, claudeDir string) {
	t.Helper()

	tmp := t.TempDir()
	claudeDir = filepath.Join(tmp, ".claude")
	storePath := filepath.Join(tmp, "cron_jobs.json")
	workDir := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}

	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{StorePath: storePath}, cronpkg.SchedulerDeps{})
	job := cronpkg.Job{
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
	runRec := cronpkg.CronRun{
		RunID:      runID,
		JobID:      jobID,
		State:      cronpkg.RunStateSucceeded,
		Trigger:    cronpkg.TriggerScheduled,
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

	h = &Handlers{
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
// `else if !run.Fresh { continue }` gate in HandleRunTranscript.
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
	h.HandleRunTranscript(w, req)

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

// TestTranscript_FreshFalse_BoundaryEndExclusive pins R242-SEC-12 (#642):
// when fresh=false the run filter uses a half-open interval
// [startedMS, endedMS) so an event whose timestamp lands exactly on
// run.EndedAt is rejected from this run's response. The boundary event
// must still be retrievable — the LATER adjacent run (whose StartedAt
// equals this run's EndedAt) claims it. Without this fix the event was
// included in BOTH runs because the time-window check used `ts > endedMS`
// AND `ts < startedMS` (both half-open in the wrong direction), letting
// the boundary timestamp pass both gates.
//
// Fresh=true is unaffected — it owns the JSONL exclusively so the
// inclusive boundary is preserved (TestTranscript_FreshTrue_*
// already locks the inclusive case).
func TestTranscript_FreshFalse_BoundaryEndExclusive(t *testing.T) {
	t.Parallel()
	// Construct a JSONL where one assistant event has timestamp ==
	// run.EndedAt (the boundary instant). Under the half-open rule
	// this event must be DROPPED for fresh=false.
	tmp := t.TempDir()
	claudeDir := filepath.Join(tmp, ".claude")
	storePath := filepath.Join(tmp, "cron_jobs.json")
	workDir := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{StorePath: storePath}, cronpkg.SchedulerDeps{})
	job := cronpkg.Job{
		ID:       strings.Repeat("c", 16),
		Schedule: "@every 1h",
		Prompt:   "boundary fixture",
		WorkDir:  workDir,
	}
	if err := sched.AddJob(&job); err != nil {
		t.Fatalf("add job: %v", err)
	}

	sessionID := "12345678-1234-1234-1234-123456789def"
	jobID := job.ID
	runID := strings.Repeat("d", 16)

	// Pin started/ended at deterministic ms-resolution so the boundary
	// timestamp can exactly match endedAt on the wire.
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	endedAt := time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC)
	runRec := cronpkg.CronRun{
		RunID:      runID,
		JobID:      jobID,
		State:      cronpkg.RunStateSucceeded,
		Trigger:    cronpkg.TriggerScheduled,
		StartedAt:  startedAt,
		EndedAt:    endedAt,
		DurationMS: endedAt.Sub(startedAt).Milliseconds(),
		SessionID:  sessionID,
		WorkDir:    workDir,
		Fresh:      false,
	}
	runsDir := filepath.Join(tmp, "runs", jobID)
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("mkdir runs: %v", err)
	}
	runJSON, err := json.Marshal(runRec)
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runsDir, runID+".json"), runJSON, 0o600); err != nil {
		t.Fatalf("write run: %v", err)
	}

	// Three events: in-window, boundary (== endedAt), past-end.
	inWindow := startedAt.Add(time.Minute).Format(time.RFC3339Nano)
	boundary := endedAt.Format(time.RFC3339Nano)
	pastEnd := endedAt.Add(time.Second).Format(time.RFC3339Nano)
	lines := []string{
		`{"type":"assistant","timestamp":"` + inWindow + `","message":{"role":"assistant","content":[{"type":"text","text":"IN_WINDOW"}]}}`,
		`{"type":"assistant","timestamp":"` + boundary + `","message":{"role":"assistant","content":[{"type":"text","text":"BOUNDARY_EVENT"}]}}`,
		`{"type":"assistant","timestamp":"` + pastEnd + `","message":{"role":"assistant","content":[{"type":"text","text":"PAST_END"}]}}`,
	}
	projDir := filepath.Join(claudeDir, "projects", discovery.ClaudeProjectSlug(workDir))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	h := &Handlers{scheduler: sched, claudeDir: claudeDir}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"/transcript?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunTranscript(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp transcriptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	var inFound, boundaryFound, pastFound bool
	for _, tr := range resp.Turns {
		switch {
		case strings.Contains(tr.Text, "IN_WINDOW"):
			inFound = true
		case strings.Contains(tr.Text, "BOUNDARY_EVENT"):
			boundaryFound = true
		case strings.Contains(tr.Text, "PAST_END"):
			pastFound = true
		}
	}
	if !inFound {
		t.Errorf("in-window event missing — base filter regression; turns=%+v", resp.Turns)
	}
	if boundaryFound {
		t.Errorf("fresh=false boundary event (ts == endedMS) leaked; half-open [startedMS, endedMS) must reject it. turns=%+v", resp.Turns)
	}
	if pastFound {
		t.Errorf("past-end event leaked — base filter regression; turns=%+v", resp.Turns)
	}
}

// TestTranscript_FreshTrue_BoundaryEndInclusive locks the corresponding
// fresh=true contract: the inclusive upper bound is preserved so a
// single-owner JSONL still surfaces a boundary event in this run's
// response. R242-SEC-12 (#642) is scoped to fresh=false; touching the
// fresh=true filter would silently drop legitimate run-end events.
func TestTranscript_FreshTrue_BoundaryEndInclusive(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	claudeDir := filepath.Join(tmp, ".claude")
	storePath := filepath.Join(tmp, "cron_jobs.json")
	workDir := filepath.Join(tmp, "workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{StorePath: storePath}, cronpkg.SchedulerDeps{})
	job := cronpkg.Job{
		ID:       strings.Repeat("e", 16),
		Schedule: "@every 1h",
		Prompt:   "boundary fixture fresh",
		WorkDir:  workDir,
	}
	if err := sched.AddJob(&job); err != nil {
		t.Fatalf("add job: %v", err)
	}

	sessionID := "12345678-1234-1234-1234-1234567890ab"
	jobID := job.ID
	runID := strings.Repeat("f", 16)
	startedAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	endedAt := time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC)
	runRec := cronpkg.CronRun{
		RunID:      runID,
		JobID:      jobID,
		State:      cronpkg.RunStateSucceeded,
		Trigger:    cronpkg.TriggerScheduled,
		StartedAt:  startedAt,
		EndedAt:    endedAt,
		DurationMS: endedAt.Sub(startedAt).Milliseconds(),
		SessionID:  sessionID,
		WorkDir:    workDir,
		Fresh:      true,
	}
	runsDir := filepath.Join(tmp, "runs", jobID)
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("mkdir runs: %v", err)
	}
	runJSON, _ := json.Marshal(runRec)
	if err := os.WriteFile(filepath.Join(runsDir, runID+".json"), runJSON, 0o600); err != nil {
		t.Fatalf("write run: %v", err)
	}

	boundary := endedAt.Format(time.RFC3339Nano)
	lines := []string{
		`{"type":"assistant","timestamp":"` + boundary + `","message":{"role":"assistant","content":[{"type":"text","text":"BOUNDARY_FRESH_KEEPS"}]}}`,
	}
	projDir := filepath.Join(claudeDir, "projects", discovery.ClaudeProjectSlug(workDir))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}

	h := &Handlers{scheduler: sched, claudeDir: claudeDir}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"/transcript?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunTranscript(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp transcriptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	found := false
	for _, tr := range resp.Turns {
		if strings.Contains(tr.Text, "BOUNDARY_FRESH_KEEPS") {
			found = true
		}
	}
	if !found {
		t.Errorf("Fresh=true must keep ts == endedMS event (inclusive upper bound preserved); turns=%+v", resp.Turns)
	}
}
