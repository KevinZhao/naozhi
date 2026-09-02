package cron

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestHandleUpdate_NotifyTrueUsesExistingPerJobTarget: the dashboard PATCHes
// only the fields that changed, so ticking "notify" on a job that already has
// a per-job target arrives as `{"notify":true}`. The coherency gate must merge
// the request with the job's current notify_platform/notify_chat_id instead of
// rejecting with 400 when no cron.notify_default is configured.
func TestHandleUpdate_NotifyTrueUsesExistingPerJobTarget(t *testing.T) {
	t.Parallel()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{}, cronpkg.SchedulerDeps{})
	if sched.NotifyDefault().IsSet() {
		t.Fatal("precondition: notify_default must be unset")
	}
	job := &cronpkg.Job{
		Schedule:       "*/10 * * * *",
		Prompt:         "hi",
		Platform:       "dashboard",
		ChatID:         "web",
		NotifyPlatform: "feishu",
		NotifyChatID:   "oc_existing",
	}
	if err := sched.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	h := &Handlers{scheduler: sched}

	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/cron?id="+job.ID, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.HandleUpdate(w, req)
		return w
	}

	if w := patch(`{"notify":true}`); w.Code != http.StatusOK {
		t.Fatalf("PATCH notify=true with existing per-job target: status %d body=%s", w.Code, w.Body.String())
	}
	jobs := sched.ListJobs("dashboard", "web")
	if len(jobs) != 1 || jobs[0].Notify == nil || !*jobs[0].Notify {
		t.Fatalf("Notify not persisted: %+v", jobs)
	}

	// A job WITHOUT any target must still be rejected — the merge does not
	// weaken the guard.
	bare := &cronpkg.Job{Schedule: "*/10 * * * *", Prompt: "hi", Platform: "dashboard", ChatID: "web2"}
	if err := sched.AddJob(bare); err != nil {
		t.Fatalf("AddJob bare: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/cron?id="+bare.ID, strings.NewReader(`{"notify":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.HandleUpdate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PATCH notify=true without any target: status %d, want 400", w.Code)
	}
}
