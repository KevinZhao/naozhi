package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandlePlannerStats_ShapeContract pins the wire shape the dashboard
// JS expects from GET /api/planner/stats. Field names are stable across
// part-1 → part-2; the part-2 follow-up adds per-planner-process RSS
// rows but the top-level keys here must keep the same shape so the
// dashboard renders during a rolling deploy.
func TestHandlePlannerStats_ShapeContract(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})

	r := httptest.NewRequest(http.MethodGet, "/api/planner/stats", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
	}

	// Decode into the typed response shape: any field rename or removal
	// will surface here as a zero-value missing field rather than a
	// silent body drift.
	var resp plannerStatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%q", err, w.Body.String())
	}

	// runtime.MemStats values are non-deterministic but a healthy server
	// will always have non-zero Sys + HeapAlloc + HeapInuse and at
	// least one goroutine (the test goroutine itself).
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
	// so dashboard JS can call .map() unconditionally. The bare
	// `*plannerStatsResponse` zero value would have a nil slice; the
	// handler explicitly initialises it, and we assert the contract here.
	if resp.PlannerKeys == nil {
		t.Error("planner_keys is null, want empty array []")
	}
	// No planner sessions configured in newTestServer → count must be 0.
	if resp.PlannerSessionsCount != 0 {
		t.Errorf("planner_sessions_count = %d, want 0 (no planner keys in test server)", resp.PlannerSessionsCount)
	}
	if len(resp.PlannerKeys) != 0 {
		t.Errorf("planner_keys = %v, want empty", resp.PlannerKeys)
	}
}

// TestHandlePlannerStats_EmitsArrayNotNull asserts the on-the-wire shape
// uses `[]` for an empty planner_keys list. JSON `null` would force the
// dashboard JS to add a null guard before iterating, breaking the
// CLIENT-SIDE CONTRACT documented above writeJSON in dashboard.go.
func TestHandlePlannerStats_EmitsArrayNotNull(t *testing.T) {
	t.Parallel()
	srv := newTestServer(&mockPlatform{})

	r := httptest.NewRequest(http.MethodGet, "/api/planner/stats", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	body := w.Body.String()
	// Cheap substring check: the empty-keys form must include the
	// literal `"planner_keys":[]` (no spaces, json.Encoder default).
	if !strings.Contains(body, `"planner_keys":[]`) {
		t.Errorf("body should emit planner_keys as `[]`, got body=%q", body)
	}
}
