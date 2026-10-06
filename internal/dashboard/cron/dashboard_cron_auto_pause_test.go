package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestHandleList_AutoPauseFields: the list carries why a job is paused, the
// failure count behind that reason and, for a backend-outage pause, how long
// the outage had gone on; the drawer and row labels read them.
func TestHandleList_AutoPauseFields(t *testing.T) {
	t.Parallel()
	lastRun := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	outage := 7*time.Hour + 30*time.Minute
	cases := []struct {
		name                   string
		reason                 string
		consecutive, transient int
		since                  time.Time
		wantCount, wantOutage  any
	}{
		{"streak", cronpkg.PausedReasonAutoFailures, 5, 0, time.Time{}, 5.0, nil},
		{"transient", cronpkg.PausedReasonAutoTransient, 2, 72, lastRun.Add(-outage), 72.0, float64(outage.Milliseconds())},
		{"streak after a shorter transient run", cronpkg.PausedReasonAutoFailures, 5, 6, lastRun.Add(-5 * time.Hour), 5.0, nil},
		{"transient after an edit", cronpkg.PausedReasonAutoTransient, 0, 0, time.Time{}, nil, nil},
		{"manual", "", 3, 4, lastRun.Add(-time.Hour), nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{}, cronpkg.SchedulerDeps{})
			if err := sched.AddJob(&cronpkg.Job{
				ID: "cc00000000000001", Schedule: "*/5 * * * *", Prompt: "p",
				Paused: true, PausedReason: tc.reason, LastRunAt: lastRun,
				ConsecutiveFailures: tc.consecutive, TransientFailures: tc.transient, TransientFailingSince: tc.since,
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
			wantReason := any(tc.reason)
			if tc.reason == "" {
				wantReason = nil
			}
			if got["paused_reason"] != wantReason || got["consecutive_failures"] != tc.wantCount || got["transient_outage_ms"] != tc.wantOutage {
				t.Errorf("paused_reason=%v consecutive_failures=%v transient_outage_ms=%v, want %v / %v / %v",
					got["paused_reason"], got["consecutive_failures"], got["transient_outage_ms"], wantReason, tc.wantCount, tc.wantOutage)
			}
		})
	}
}
