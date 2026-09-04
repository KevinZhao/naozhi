package planner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/session"
)

func newStatsHandlers() *Handlers {
	return New(Deps{Router: session.NewRouter(session.RouterConfig{})})
}

// TestHandleStats_ShapeContract pins the wire shape the dashboard JS expects
// from GET /api/planner/stats: a per-planner-process RSS follow-up may add
// rows, but the top-level keys must keep the same shape so the dashboard
// renders during a rolling deploy.
func TestHandleStats_ShapeContract(t *testing.T) {
	t.Parallel()
	h := newStatsHandlers()

	r := httptest.NewRequest(http.MethodGet, "/api/planner/stats", nil)
	w := httptest.NewRecorder()
	h.HandleStats(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
	}

	// Decode into the typed response shape: any field rename or removal
	// surfaces here as a zero-value missing field rather than silent drift.
	var resp statsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%q", err, w.Body.String())
	}

	// runtime.MemStats values are non-deterministic but a healthy process
	// always has non-zero Sys + HeapAlloc and at least one goroutine.
	if resp.NaozhiRSSBytes == 0 {
		t.Errorf("naozhi_rss_bytes = 0, want non-zero (runtime.MemStats.Sys cannot be zero in a live process)")
	}
	if resp.NaozhiHeapAllocBytes == 0 {
		t.Errorf("naozhi_heap_alloc_bytes = 0, want non-zero")
	}
	if resp.Goroutines <= 0 {
		t.Errorf("goroutines = %d, want >= 1", resp.Goroutines)
	}

	// PlannerKeys must always be a non-nil slice (json `[]`, never `null`)
	// so dashboard JS can call .map() unconditionally.
	if resp.PlannerKeys == nil {
		t.Error("planner_keys is null, want empty array []")
	}
	// No planner sessions in a fresh router → count must be 0.
	if resp.PlannerSessionsCount != 0 {
		t.Errorf("planner_sessions_count = %d, want 0 (no planner keys in fresh router)", resp.PlannerSessionsCount)
	}
	if len(resp.PlannerKeys) != 0 {
		t.Errorf("planner_keys = %v, want empty", resp.PlannerKeys)
	}
}

// TestHandleStats_EmitsArrayNotNull asserts the on-the-wire shape uses `[]`
// for an empty planner_keys list; JSON `null` would force a null guard into
// the dashboard JS before iterating.
func TestHandleStats_EmitsArrayNotNull(t *testing.T) {
	t.Parallel()
	h := newStatsHandlers()

	r := httptest.NewRequest(http.MethodGet, "/api/planner/stats", nil)
	w := httptest.NewRecorder()
	h.HandleStats(w, r)

	body := w.Body.String()
	// The empty-keys form must include the literal `"planner_keys":[]`
	// (no spaces, json.Encoder default).
	if !strings.Contains(body, `"planner_keys":[]`) {
		t.Errorf("body should emit planner_keys as `[]`, got body=%q", body)
	}
}
