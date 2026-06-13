package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

func attentionTestScheduler(t *testing.T, storePath string) *cronpkg.Scheduler {
	t.Helper()
	return cronpkg.NewScheduler(cronpkg.SchedulerConfig{
		StorePath:      storePath,
		AllowNilRouter: true,
	}, cronpkg.SchedulerDeps{})
}

// TestHandleAttentionList_ReturnsQueue: staged queue records are returned as
// the §7.4 items array with sanitised labels.
func TestHandleAttentionList_ReturnsQueue(t *testing.T) {
	t.Parallel()
	storePath := filepath.Join(t.TempDir(), "cron_jobs.json")
	sched := attentionTestScheduler(t, storePath)
	sched.WriteSandboxAttentionForTest(strings.Repeat("a", 16), strings.Repeat("b", 16), "transport", "nightly PR job")

	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/attention", nil)
	w := httptest.NewRecorder()
	h.HandleAttentionList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Items []struct {
			JobID    string `json:"job_id"`
			RunID    string `json:"run_id"`
			Reason   string `json:"reason"`
			JobLabel string `json:"job_label"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(resp.Items))
	}
	if resp.Items[0].Reason != "transport" || resp.Items[0].JobLabel != "nightly PR job" {
		t.Fatalf("item wrong: %+v", resp.Items[0])
	}
}

// TestHandleAttentionList_EmptyArray: an empty queue returns items:[] (not
// null / 404) so the drawer renders deterministically.
func TestHandleAttentionList_EmptyArray(t *testing.T) {
	t.Parallel()
	sched := attentionTestScheduler(t, filepath.Join(t.TempDir(), "cron_jobs.json"))
	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/attention", nil)
	w := httptest.NewRecorder()
	h.HandleAttentionList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatalf("want items:[], got %s", w.Body.String())
	}
}

// TestHandleRunConfirm_Resolves: POST /confirm removes the queue record.
func TestHandleRunConfirm_Resolves(t *testing.T) {
	t.Parallel()
	sched := attentionTestScheduler(t, filepath.Join(t.TempDir(), "cron_jobs.json"))
	runID := strings.Repeat("b", 16)
	sched.WriteSandboxAttentionForTest(strings.Repeat("a", 16), runID, "transport", "job")

	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodPost, "/api/cron/runs/"+runID+"/confirm", nil)
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunConfirm(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if sched.SandboxAttentionCount() != 0 {
		t.Fatalf("confirm must clear the queue; count = %d", sched.SandboxAttentionCount())
	}
}

// TestHandleRunConfirm_RejectsBadID guards the path-traversal surface.
func TestHandleRunConfirm_RejectsBadID(t *testing.T) {
	t.Parallel()
	sched := attentionTestScheduler(t, filepath.Join(t.TempDir(), "cron_jobs.json"))
	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodPost, "/api/cron/runs/x/confirm", nil)
	req.SetPathValue("run_id", "../../etc")
	w := httptest.NewRecorder()
	h.HandleRunConfirm(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestHandleRunReplay_RequiresJobID: replay without job_id is a 400.
func TestHandleRunReplay_RequiresJobID(t *testing.T) {
	t.Parallel()
	sched := attentionTestScheduler(t, filepath.Join(t.TempDir(), "cron_jobs.json"))
	h := &Handlers{scheduler: sched}
	runID := strings.Repeat("b", 16)
	req := httptest.NewRequest(http.MethodPost, "/api/cron/runs/"+runID+"/replay", strings.NewReader(`{}`))
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunReplay(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleRunReplay_JobNotFound: replaying a run for a missing job → 404.
func TestHandleRunReplay_JobNotFound(t *testing.T) {
	t.Parallel()
	sched := attentionTestScheduler(t, filepath.Join(t.TempDir(), "cron_jobs.json"))
	h := &Handlers{scheduler: sched}
	runID, jobID := strings.Repeat("b", 16), strings.Repeat("a", 16)
	req := httptest.NewRequest(http.MethodPost, "/api/cron/runs/"+runID+"/replay",
		strings.NewReader(`{"job_id":"`+jobID+`"}`))
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunReplay(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleAttentionList_SanitizesReason pins R20260613-SEC-3: the Reason
// field must be sanitized via osutil.SanitizeForLog for parity with JobLabel.
// Reason comes from operator-writable sandboxattention/*.json; control
// characters and HTML must be neutralised before reaching API consumers.
func TestHandleAttentionList_SanitizesReason(t *testing.T) {
	t.Parallel()
	storePath := filepath.Join(t.TempDir(), "cron_jobs.json")
	sched := attentionTestScheduler(t, storePath)
	// Reason contains a newline (log-injection) and an HTML tag (XSS-adjacent).
	dirtyReason := "transport\n<script>alert(1)</script>"
	sched.WriteSandboxAttentionForTest(strings.Repeat("a", 16), strings.Repeat("b", 16), dirtyReason, "job")

	h := &Handlers{scheduler: sched}
	req := httptest.NewRequest(http.MethodGet, "/api/cron/attention", nil)
	w := httptest.NewRecorder()
	h.HandleAttentionList(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Items []struct {
			Reason string `json:"reason"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(resp.Items))
	}
	got := resp.Items[0].Reason
	// The newline must have been replaced (control char sanitised).
	if strings.Contains(got, "\n") {
		t.Errorf("Reason still contains newline after sanitize: %q", got)
	}
	// The sanitised value must still convey the reason prefix — record is not dropped.
	if !strings.HasPrefix(got, "transport") {
		t.Errorf("Reason prefix lost — attention record must not be dropped: %q", got)
	}
}

// TestHandleRunReplay_NilScheduler: with no scheduler wired, 501.
func TestHandleRunReplay_NilScheduler(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	runID := strings.Repeat("b", 16)
	req := httptest.NewRequest(http.MethodPost, "/api/cron/runs/"+runID+"/replay", strings.NewReader(`{}`))
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	h.HandleRunReplay(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", w.Code)
	}
}
