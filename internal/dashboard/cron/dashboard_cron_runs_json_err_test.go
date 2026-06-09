package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestHandleRunsList_ValidationErrors_JSONEnvelope asserts that every 400
// validation path in HandleRunsList returns application/json with a body
// containing {"error": "..."}, matching the writeCronErr contract used by
// all other cron endpoints. Previously these paths used http.Error which
// returned text/plain, making the error invisible to dashboard JS that
// parses body.error.
func TestHandleRunsList_ValidationErrors_JSONEnvelope(t *testing.T) {
	t.Parallel()

	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      t.TempDir() + "/cron_jobs.json",
		AllowNilRouter: true,
	})
	h := &Handlers{scheduler: sched}

	cases := []struct {
		name   string
		query  string
		status int
	}{
		{"missing job_id", "", http.StatusBadRequest},
		{"job_id too long", "job_id=" + strings.Repeat("a", 200), http.StatusBadRequest},
		{"job_id invalid hex", "job_id=ZZZZZZZZZZZZZZZZ", http.StatusBadRequest},
		{"limit too long", "job_id=" + strings.Repeat("a", 16) + "&limit=99999", http.StatusBadRequest},
		{"limit non-integer", "job_id=" + strings.Repeat("a", 16) + "&limit=abc", http.StatusBadRequest},
		{"before too long", "job_id=" + strings.Repeat("a", 16) + "&before=" + strings.Repeat("1", 20), http.StatusBadRequest},
		{"before non-integer", "job_id=" + strings.Repeat("a", 16) + "&before=notanumber", http.StatusBadRequest},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/api/cron/runs?"+tc.query, nil)
			w := httptest.NewRecorder()
			h.HandleRunsList(w, req)

			assertRunsJSONErrEnvelope(t, w, tc.status)
		})
	}
}

// TestHandleRunDetail_ValidationErrors_JSONEnvelope asserts that every 400
// validation path in HandleRunDetail returns application/json with a body
// containing {"error": "..."}.
func TestHandleRunDetail_ValidationErrors_JSONEnvelope(t *testing.T) {
	t.Parallel()

	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      t.TempDir() + "/cron_jobs.json",
		AllowNilRouter: true,
	})
	h := &Handlers{scheduler: sched}

	validJobID := strings.Repeat("a", 16)
	validRunID := strings.Repeat("b", 16)

	cases := []struct {
		name   string
		runID  string
		query  string
		status int
	}{
		{"missing run_id", "", "job_id=" + validJobID, http.StatusBadRequest},
		{"run_id too long", strings.Repeat("a", 200), "job_id=" + validJobID, http.StatusBadRequest},
		{"run_id invalid hex", "ZZZZZZZZZZZZZZZZ", "job_id=" + validJobID, http.StatusBadRequest},
		{"missing job_id", validRunID, "", http.StatusBadRequest},
		{"job_id too long", validRunID, "job_id=" + strings.Repeat("a", 200), http.StatusBadRequest},
		{"job_id invalid hex", validRunID, "job_id=ZZZZZZZZZZZZZZZZ", http.StatusBadRequest},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			url := "/api/cron/runs/" + tc.runID
			if tc.query != "" {
				url += "?" + tc.query
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			if tc.runID != "" {
				req.SetPathValue("run_id", tc.runID)
			}
			w := httptest.NewRecorder()
			h.HandleRunDetail(w, req)

			assertRunsJSONErrEnvelope(t, w, tc.status)
		})
	}
}

// TestHandleRunDetail_NotFound_JSONEnvelope asserts that 404 (run not found)
// also uses the JSON envelope instead of text/plain.
func TestHandleRunDetail_NotFound_JSONEnvelope(t *testing.T) {
	t.Parallel()

	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      t.TempDir() + "/cron_jobs.json",
		AllowNilRouter: true,
	})
	h := &Handlers{scheduler: sched}

	runID := strings.Repeat("c", 16)
	jobID := strings.Repeat("d", 16)

	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunDetail(w, req)

	assertRunsJSONErrEnvelope(t, w, http.StatusNotFound)
}

// assertRunsJSONErrEnvelope checks that w has the expected status code,
// Content-Type: application/json, and a JSON body with a non-empty "error" key.
func assertRunsJSONErrEnvelope(t *testing.T, w *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if w.Code != wantStatus {
		t.Errorf("status = %d, want %d (body: %s)", w.Code, wantStatus, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json (body: %s)", ct, w.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body not valid JSON: %v (body: %s)", err, w.Body.String())
	}
	errVal, ok := env["error"]
	if !ok {
		t.Errorf("JSON body missing \"error\" key: %s", w.Body.String())
		return
	}
	if s, _ := errVal.(string); s == "" {
		t.Errorf("JSON \"error\" field is empty")
	}
}
