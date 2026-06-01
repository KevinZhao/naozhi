package cron

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestHandleUpdate_NotifyClear_R103901_GO_1 pins R103901-GO-1: the scheduler
// has supported resetting Job.Notify back to legacy-default via
// JobUpdate.NotifyClear (R249-CR-15 #958), but the dashboard PATCH /api/cron
// handler never exposed a notify_clear field, so the reset path was
// unreachable over HTTP (dead line). After wiring it, a PATCH with
// {"notify_clear":true} must reset a previously-set Notify back to nil.
func TestHandleUpdate_NotifyClear_R103901_GO_1(t *testing.T) {
	t.Parallel()

	const (
		platform = "feishu"
		chatID   = "oc_test"
	)

	// newHandlers returns handlers plus the scheduler-assigned job ID
	// (AddJob always overwrites Job.ID with a derived hex id).
	newHandlers := func(t *testing.T) (*Handlers, string) {
		t.Helper()
		sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{})
		job := &cronpkg.Job{
			Schedule: "*/10 * * * *",
			Prompt:   "hi",
			Platform: platform,
			ChatID:   chatID,
		}
		if err := sched.AddJob(job); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		return &Handlers{scheduler: sched}, job.ID
	}

	patch := func(t *testing.T, h *Handlers, jobID, body string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, "/api/cron?id="+jobID, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.HandleUpdate(w, req)
		return w.Code
	}

	jobNotify := func(t *testing.T, h *Handlers) *bool {
		t.Helper()
		jobs := h.scheduler.ListJobs(platform, chatID)
		if len(jobs) != 1 {
			t.Fatalf("ListJobs: want 1 job, got %d", len(jobs))
		}
		return jobs[0].Notify
	}

	t.Run("notify_clear=true resets a set Notify to nil", func(t *testing.T) {
		t.Parallel()
		h, jobID := newHandlers(t)

		// First set Notify=true via the HTTP path. A per-job target is
		// supplied so the notify=true coherency gate passes.
		if code := patch(t, h, jobID, `{"notify":true,"notify_platform":"feishu","notify_chat_id":"oc_x"}`); code != http.StatusOK {
			t.Fatalf("PATCH notify=true: status %d", code)
		}
		if n := jobNotify(t, h); n == nil || *n != true {
			t.Fatalf("after set: Notify = %v, want pointer-to-true", n)
		}

		// Now clear it over HTTP — this is the previously-unreachable path.
		if code := patch(t, h, jobID, `{"notify_clear":true}`); code != http.StatusOK {
			t.Fatalf("PATCH notify_clear=true: status %d", code)
		}
		if n := jobNotify(t, h); n != nil {
			t.Fatalf("after notify_clear=true: Notify = %v, want nil", *n)
		}
	})

	t.Run("notify_clear alone satisfies at-least-one-field gate", func(t *testing.T) {
		t.Parallel()
		h, jobID := newHandlers(t)
		// A body with ONLY notify_clear must not trip the
		// "at least one field must be provided" 400.
		if code := patch(t, h, jobID, `{"notify_clear":true}`); code != http.StatusOK {
			t.Fatalf("PATCH notify_clear-only: status %d, want 200", code)
		}
	})
}
