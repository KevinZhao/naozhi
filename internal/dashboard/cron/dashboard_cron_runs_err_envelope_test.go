package cron

// Tests for R20260613-SEC-2: HandleRunsList and HandleRunDetail must return
// application/json error bodies ({"error":...}) for all validation and
// business errors, not text/plain via http.Error.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// assertJSONErr verifies that the response has the given HTTP status, a
// Content-Type of application/json, and a body that decodes to
// {"error": <non-empty>}.
func assertJSONErr(t *testing.T, label string, w *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if w.Code != wantStatus {
		t.Errorf("%s: status = %d, want %d; body=%s", label, w.Code, wantStatus, w.Body.String())
		return
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("%s: Content-Type = %q, want application/json", label, ct)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Errorf("%s: body is not JSON: %v; body=%s", label, err, w.Body.String())
		return
	}
	if body["error"] == "" {
		t.Errorf("%s: body[\"error\"] is empty; body=%s", label, w.Body.String())
	}
}

// --- HandleRunsList validation errors ---

func TestHandleRunsList_ErrEnvelope_MissingJobID(t *testing.T) {
	t.Parallel()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      t.TempDir() + "/cron_jobs.json",
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})
	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs", nil)
	w := httptest.NewRecorder()
	h.HandleRunsList(w, req)
	assertJSONErr(t, "missing job_id", w, http.StatusBadRequest)
}

func TestHandleRunsList_ErrEnvelope_JobIDTooLong(t *testing.T) {
	t.Parallel()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      t.TempDir() + "/cron_jobs.json",
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})
	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs?job_id="+strings.Repeat("a", 200), nil)
	w := httptest.NewRecorder()
	h.HandleRunsList(w, req)
	assertJSONErr(t, "job_id too long", w, http.StatusBadRequest)
}

func TestHandleRunsList_ErrEnvelope_JobIDNotHex(t *testing.T) {
	t.Parallel()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      t.TempDir() + "/cron_jobs.json",
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})
	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs?job_id=UPPERCASE_BAD!!", nil)
	w := httptest.NewRecorder()
	h.HandleRunsList(w, req)
	assertJSONErr(t, "job_id not hex", w, http.StatusBadRequest)
}

func TestHandleRunsList_ErrEnvelope_InvalidLimit(t *testing.T) {
	t.Parallel()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      t.TempDir() + "/cron_jobs.json",
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})
	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet,
		"/api/cron/runs?job_id="+strings.Repeat("a", 16)+"&limit=bad", nil)
	w := httptest.NewRecorder()
	h.HandleRunsList(w, req)
	assertJSONErr(t, "invalid limit", w, http.StatusBadRequest)
}

func TestHandleRunsList_ErrEnvelope_InvalidBefore(t *testing.T) {
	t.Parallel()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      t.TempDir() + "/cron_jobs.json",
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})
	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet,
		"/api/cron/runs?job_id="+strings.Repeat("a", 16)+"&before=notanumber", nil)
	w := httptest.NewRecorder()
	h.HandleRunsList(w, req)
	assertJSONErr(t, "invalid before", w, http.StatusBadRequest)
}

// --- HandleRunDetail validation errors ---

func TestHandleRunDetail_ErrEnvelope_NilScheduler(t *testing.T) {
	t.Parallel()
	h := &Handlers{scheduler: nil}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+strings.Repeat("a", 16)+"?job_id="+strings.Repeat("b", 16), nil)
	req.SetPathValue("run_id", strings.Repeat("a", 16))
	w := httptest.NewRecorder()
	h.HandleRunDetail(w, req)
	assertJSONErr(t, "nil scheduler", w, http.StatusNotImplemented)
}

func TestHandleRunDetail_ErrEnvelope_MissingRunID(t *testing.T) {
	t.Parallel()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      t.TempDir() + "/cron_jobs.json",
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})
	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/?job_id="+strings.Repeat("b", 16), nil)
	req.SetPathValue("run_id", "")
	w := httptest.NewRecorder()
	h.HandleRunDetail(w, req)
	assertJSONErr(t, "missing run_id", w, http.StatusBadRequest)
}

func TestHandleRunDetail_ErrEnvelope_RunIDNotHex(t *testing.T) {
	t.Parallel()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      t.TempDir() + "/cron_jobs.json",
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})
	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/BADID?job_id="+strings.Repeat("b", 16), nil)
	req.SetPathValue("run_id", "BADID")
	w := httptest.NewRecorder()
	h.HandleRunDetail(w, req)
	assertJSONErr(t, "run_id not hex", w, http.StatusBadRequest)
}

func TestHandleRunDetail_ErrEnvelope_MissingJobID(t *testing.T) {
	t.Parallel()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      t.TempDir() + "/cron_jobs.json",
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})
	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+strings.Repeat("a", 16), nil)
	req.SetPathValue("run_id", strings.Repeat("a", 16))
	w := httptest.NewRecorder()
	h.HandleRunDetail(w, req)
	assertJSONErr(t, "missing job_id", w, http.StatusBadRequest)
}

func TestHandleRunDetail_ErrEnvelope_RunNotFound(t *testing.T) {
	t.Parallel()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      t.TempDir() + "/cron_jobs.json",
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})
	h := &Handlers{scheduler: sched}
	runID := strings.Repeat("a", 16)
	jobID := strings.Repeat("b", 16)
	req := httptest.NewRequest(http.MethodGet, "/api/cron/runs/"+runID+"?job_id="+jobID, nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunDetail(w, req)
	assertJSONErr(t, "run not found", w, http.StatusNotFound)
}
