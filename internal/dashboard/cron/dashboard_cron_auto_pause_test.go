package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestHandleList_AutoPauseFields: the list carries why a job is paused and
// its failure streak, which the drawer and row labels read.
func TestHandleList_AutoPauseFields(t *testing.T) {
	t.Parallel()
	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{}, cronpkg.SchedulerDeps{})
	if err := sched.AddJob(&cronpkg.Job{
		ID: "cc00000000000001", Schedule: "*/5 * * * *", Prompt: "p",
		Paused: true, PausedReason: cronpkg.PausedReasonAutoFailures, ConsecutiveFailures: 5,
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	h := &Handlers{deps: Deps{Scheduler: sched}}
	w := httptest.NewRecorder()
	h.HandleList(w, httptest.NewRequest(http.MethodGet, "/api/cron", nil))
	var resp struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Jobs) != 1 {
		t.Fatalf("decode: %v; body=%s", err, w.Body.String())
	}
	got := resp.Jobs[0]
	if got["paused_reason"] != "auto_failures" || got["consecutive_failures"] != float64(5) {
		t.Errorf("paused_reason=%v consecutive_failures=%v, want auto_failures / 5", got["paused_reason"], got["consecutive_failures"])
	}
}
