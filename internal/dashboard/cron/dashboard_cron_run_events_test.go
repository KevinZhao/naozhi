package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestHandleRunEvents_ServesPersistedLog pins the §7.3 events endpoint: the
// persisted sandboxevents NDJSON is returned verbatim as a JSON array.
func TestHandleRunEvents_ServesPersistedLog(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      storePath,
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})

	jobID := strings.Repeat("a", 16)
	runID := strings.Repeat("b", 16)
	evDir := filepath.Join(tmp, "sandboxevents", jobID)
	if err := os.MkdirAll(evDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lines := `{"kind":"boot","msg":"materialized"}` + "\n" +
		`{"kind":"cli","line":{"type":"result","is_error":false}}` + "\n" +
		`{"kind":"exit","code":0}` + "\n"
	if err := os.WriteFile(filepath.Join(evDir, runID+".ndjson"), []byte(lines), 0o600); err != nil {
		t.Fatalf("write events: %v", err)
	}

	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"/events?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Events    []json.RawMessage `json:"events"`
		Truncated bool              `json:"truncated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(resp.Events))
	}
	if resp.Truncated {
		t.Fatal("must not be truncated")
	}
	if !strings.Contains(string(resp.Events[0]), "materialized") {
		t.Fatalf("first event unexpected: %s", resp.Events[0])
	}
}

// TestHandleRunEvents_MissingLogEmptyArray: a run with no event log (local
// run / events-disabled) returns 200 + empty array, not 404.
func TestHandleRunEvents_MissingLogEmptyArray(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      filepath.Join(tmp, "cron_jobs.json"),
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})

	jobID := strings.Repeat("a", 16)
	runID := strings.Repeat("b", 16)
	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"/events?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 0 {
		t.Fatalf("events = %d, want 0", len(resp.Events))
	}
}

// TestHandleRunEvents_RejectsBadIDs guards the path-traversal surface.
func TestHandleRunEvents_RejectsBadIDs(t *testing.T) {
	t.Parallel()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      filepath.Join(t.TempDir(), "cron_jobs.json"),
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})
	h := &Handlers{scheduler: sched}

	// non-hex run_id
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/x/events?job_id="+strings.Repeat("a", 16), nil)
	req.SetPathValue("run_id", "../etc/passwd")
	w := httptest.NewRecorder()
	h.HandleRunEvents(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad run_id status = %d, want 400", w.Code)
	}

	// missing job_id
	req = httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+strings.Repeat("b", 16)+"/events", nil)
	req.SetPathValue("run_id", strings.Repeat("b", 16))
	w = httptest.NewRecorder()
	h.HandleRunEvents(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing job_id status = %d, want 400", w.Code)
	}
}

// TestHandleRunEvents_RedactsSecrets verifies R20260613-SEC-1: secrets in the
// sandbox event NDJSON are replaced with [REDACTED] before reaching the wire.
// An sk-ant- token and an AKIA token embedded in tool-call lines must not
// appear in the JSON response.
func TestHandleRunEvents_RedactsSecrets(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      filepath.Join(tmp, "cron_jobs.json"),
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})

	jobID := strings.Repeat("c", 16)
	runID := strings.Repeat("d", 16)
	evDir := filepath.Join(tmp, "sandboxevents", jobID)
	if err := os.MkdirAll(evDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	antSecret := "sk-ant-api03-" + strings.Repeat("X", 40)
	akiaSecret := "AKIA" + strings.Repeat("Y", 20)
	lines := `{"kind":"cli","line":{"type":"tool_use","name":"Bash","input":{"command":"echo ` + antSecret + `"}}}` + "\n" +
		`{"kind":"cli","line":{"type":"tool_result","content":"key=` + akiaSecret + ` done"}}` + "\n" +
		`{"kind":"exit","code":0}` + "\n"
	if err := os.WriteFile(filepath.Join(evDir, runID+".ndjson"), []byte(lines), 0o600); err != nil {
		t.Fatalf("write events: %v", err)
	}

	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"/events?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if strings.Contains(body, antSecret) {
		t.Errorf("R20260613-SEC-1: sk-ant- secret leaked in events response: %q", antSecret)
	}
	if strings.Contains(body, akiaSecret) {
		t.Errorf("R20260613-SEC-1: AKIA secret leaked in events response: %q", akiaSecret)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("R20260613-SEC-1: expected [REDACTED] marker in response, got: %s", body)
	}

	// Response must still be valid JSON with 3 events.
	var resp struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("R20260613-SEC-1: redacted response is not valid JSON: %v", err)
	}
	if len(resp.Events) != 3 {
		t.Fatalf("R20260613-SEC-1: events = %d, want 3", len(resp.Events))
	}
	// Each event must still be valid JSON after redaction.
	for i, ev := range resp.Events {
		var v map[string]any
		if err := json.Unmarshal(ev, &v); err != nil {
			t.Errorf("R20260613-SEC-1: event[%d] is not valid JSON after redaction: %v (%s)", i, err, ev)
		}
	}
}

// TestHandleRunEvents_ErrorPathsUseJSONStatus verifies R20260613-SEC-10: the
// error paths in HandleRunEvents use WriteJSONStatus (JSON + nosniff) rather
// than bare http.Error (text/plain).
func TestHandleRunEvents_ErrorPathsUseJSONStatus(t *testing.T) {
	t.Parallel()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      filepath.Join(t.TempDir(), "cron_jobs.json"),
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})

	tests := []struct {
		name       string
		runID      string
		jobID      string
		wantStatus int
	}{
		{
			name:       "invalid run_id",
			runID:      "../etc/passwd",
			jobID:      strings.Repeat("a", 16),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing job_id",
			runID:      strings.Repeat("b", 16),
			jobID:      "",
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &Handlers{scheduler: sched}
			url := "/api/cron/runs/" + tc.runID + "/events"
			if tc.jobID != "" {
				url += "?job_id=" + tc.jobID
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			req.SetPathValue("run_id", tc.runID)
			w := httptest.NewRecorder()
			h.HandleRunEvents(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			ct := w.Header().Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			nosniff := w.Header().Get("X-Content-Type-Options")
			if nosniff != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", nosniff)
			}
			var v map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
				t.Errorf("response is not valid JSON: %v (%s)", err, w.Body.String())
			}
			if v["error"] == "" {
				t.Errorf("response JSON missing 'error' key: %s", w.Body.String())
			}
		})
	}
}

// TestHandleRunDetail_SurfacesSandboxMeta pins the §7.3 meta bar data source:
// a run record with SandboxMeta surfaces a `sandbox` object in the detail view.
func TestHandleRunDetail_SurfacesSandboxMeta(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storePath := filepath.Join(tmp, "cron_jobs.json")
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      storePath,
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})

	jobID := strings.Repeat("a", 16)
	runID := strings.Repeat("b", 16)
	runsDir := filepath.Join(tmp, "runs", jobID)
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rec := cronpkg.CronRun{
		RunID: runID, JobID: jobID, State: cronpkg.RunStateSucceeded,
		SandboxMeta: &cronpkg.SandboxRunMeta{
			RuntimeARN: "arn:x", ImageVersion: "phase2",
			CostUSD: 0.0044, DurationMS: 1888, MemoryPeakBytes: 268435456,
		},
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(runsDir, runID+".json"), b, 0o600); err != nil {
		t.Fatalf("write run: %v", err)
	}

	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunDetail(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`"sandbox"`, `"image_version":"phase2"`, `"cost_usd":0.0044`, `"memory_peak_bytes":268435456`} {
		if !strings.Contains(body, want) {
			t.Errorf("detail view missing %q: %s", want, body)
		}
	}
}
