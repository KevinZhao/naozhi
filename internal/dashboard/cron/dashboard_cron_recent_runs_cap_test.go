package cron

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	cronpkg "github.com/naozhi/naozhi/internal/cron"
)

// TestHandleList_RecentRunsCapEchoed pins the wire contract the dashboard
// timeline relies on: GET /api/cron carries recent_runs_cap == recentRunsPerJob
// so cron_view.js can decide whether a job's embedded recent_runs is the
// complete history (len < cap) or merely the first page (len == cap) without
// duplicating the server constant. Before this field existed the front end
// hard-coded `< 10` against a 5-entry cap and 加载更多 was unreachable.
func TestHandleList_RecentRunsCapEchoed(t *testing.T) {
	t.Parallel()

	sched := cronpkg.NewScheduler(cronpkg.SchedulerConfig{}, cronpkg.SchedulerDeps{})
	if err := sched.AddJob(&cronpkg.Job{
		ID:       "aa00000000000002",
		Schedule: "*/10 * * * *",
		Prompt:   "ping",
		Platform: "feishu",
		ChatID:   "oc_test",
	}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	h := &Handlers{scheduler: sched}

	for _, query := range []string{"", "?compact=1"} {
		req := httptest.NewRequest(http.MethodGet, "/api/cron"+query, nil)
		w := httptest.NewRecorder()
		h.HandleList(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("query %q: status %d, body=%s", query, w.Code, w.Body.String())
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatalf("query %q: decode: %v", query, err)
		}
		capRaw, ok := raw["recent_runs_cap"]
		if !ok {
			t.Fatalf("query %q: response must carry recent_runs_cap (no omitempty — the front end needs it even when the cap is small)", query)
		}
		var got int
		if err := json.Unmarshal(capRaw, &got); err != nil {
			t.Fatalf("query %q: recent_runs_cap decode: %v", query, err)
		}
		if got != recentRunsPerJob {
			t.Fatalf("query %q: recent_runs_cap = %d, want recentRunsPerJob (%d)", query, got, recentRunsPerJob)
		}
	}
}
