package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestHandleList_AutoPauseFields: the list carries why a job is paused and
// its failure count, which the drawer and row labels read. A transient pause
// leaves the streak alone, so the count is the larger of the two.
func TestHandleList_AutoPauseFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                   string
		consecutive, transient int
		want                   float64
	}{
		{"streak", 5, 0, 5},
		{"transient", 2, 72, 72},
		{"streak above transient", 5, 3, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{}, cronpkg.SchedulerDeps{})
			if err := sched.AddJob(&cronpkg.Job{
				ID: "cc00000000000001", Schedule: "*/5 * * * *", Prompt: "p",
				Paused: true, PausedReason: cronpkg.PausedReasonAutoFailures,
				ConsecutiveFailures: tc.consecutive, TransientFailures: tc.transient,
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
			if got["paused_reason"] != "auto_failures" || got["consecutive_failures"] != tc.want {
				t.Errorf("paused_reason=%v consecutive_failures=%v, want auto_failures / %v", got["paused_reason"], got["consecutive_failures"], tc.want)
			}
		})
	}
}
