package cron

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// A create the scheduler refuses for a full job table is a 409 that says so,
// and a too-frequent schedule (on create or edit) names the interval floor;
// only the remaining rejections keep the generic 400.
func TestHandleCreateUpdate_RejectionStatusByClass(t *testing.T) {
	t.Parallel()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{MaxJobs: 1, AllowNilRouter: true}, cronpkg.SchedulerDeps{})
	h := &Handlers{deps: Deps{Scheduler: sched}}
	send := func(method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		if method == http.MethodPost {
			h.HandleCreate(w, req)
		} else {
			h.HandleUpdate(w, req)
		}
		return w
	}

	// Rejected before the table is consulted, so these run with room left.
	for _, c := range []struct {
		body, wantErr string
	}{
		{`{"schedule":"@every 1m","prompt":"hi"}`, "schedule interval below the 5m minimum"},
		{`{"schedule":"not a cron","prompt":"hi"}`, "invalid schedule or job fields"},
	} {
		w := send(http.MethodPost, "/api/cron", c.body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.wantErr) {
			t.Errorf("create %s: got %d %q, want 400 containing %q", c.body, w.Code, w.Body.String(), c.wantErr)
		}
	}

	job := &cronpkg.Job{Schedule: "@every 1h", Prompt: "hi", Platform: "dashboard", ChatID: "global"}
	if err := sched.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	w := send(http.MethodPost, "/api/cron", `{"schedule":"@every 1h","prompt":"hi"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "cron job quota reached") {
		t.Errorf("create over quota: got %d %q, want 409 quota", w.Code, w.Body.String())
	}

	w = send(http.MethodPatch, "/api/cron?id="+job.ID, `{"schedule":"@every 1m"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "schedule interval below the 5m minimum") {
		t.Errorf("update to 1m: got %d %q, want 400 interval", w.Code, w.Body.String())
	}
	w = send(http.MethodPatch, "/api/cron?id="+job.ID, `{"schedule":"not a cron"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid update payload") {
		t.Errorf("update to garbage: got %d %q, want 400 generic", w.Code, w.Body.String())
	}
}
