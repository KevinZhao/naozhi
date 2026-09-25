package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/cron"
	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// TestRunStoresHealthProbe_OmitsWhenNothingPersists: /health's wire contract
// is that a disabled subsystem contributes nothing (omitempty), so monitoring
// that keys on a section's presence does not see a zeroed store it cannot
// tell from a healthy one.
func TestRunStoresHealthProbe_OmitsWhenNothingPersists(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func() cron.RunStoreHealth
	}{
		{"no scheduler", nil},
		{"scheduler without persistence", func() cron.RunStoreHealth { return cron.RunStoreHealth{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var auth healthAuthSection
			runStoresHealthProbe(tc.fn, nil)(&auth)
			if auth.RunStores != nil {
				t.Errorf("run_stores = %+v, want omitted", auth.RunStores)
			}
		})
	}
}

// TestHandleHealth_RunStoresCarryTheCounters: the counters reach an
// authenticated /health under stable key names. Before #2792 the cron run
// store's comments called these "the only operator-visible signal" of lost
// history while nothing outside tests read them.
func TestHandleHealth_RunStoresCarryTheCounters(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")
	srv.healthH.cronRunStore = func() cron.RunStoreHealth {
		return cron.RunStoreHealth{Enabled: true, WriteFailedDiskFull: 1, WriteFailedOther: 2, HistoryDropped: 3, CacheStaleEvictions: 4}
	}
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	srv.healthH.handleHealth(w, req)

	var body struct {
		RunStores map[string]map[string]float64 `json:"run_stores"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, w.Body.String())
	}
	c := body.RunStores["cron"]
	for key, want := range map[string]float64{
		"write_failed_disk_full_total": 1,
		"write_failed_other_total":     2,
		"history_dropped_total":        3,
		"cache_stale_evictions_total":  4,
	} {
		if got, ok := c[key]; !ok || got != want {
			t.Errorf("run_stores.cron.%s = %v (present=%v), want %v; body=%s", key, got, ok, want, w.Body.String())
		}
	}
}

// TestHandleHealth_RunStoresHiddenFromAnonymousProbes: the section is part of
// the authenticated view only, like every other store detail.
func TestHandleHealth_RunStoresHiddenFromAnonymousProbes(t *testing.T) {
	srv := newTestServerWithToken(&mockPlatform{}, "secret")
	srv.healthH.cronRunStore = func() cron.RunStoreHealth {
		return cron.RunStoreHealth{Enabled: true, WriteFailedOther: 9}
	}
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.healthH.handleHealth(w, req)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["run_stores"]; ok {
		t.Errorf("anonymous /health exposed run_stores: %s", w.Body.String())
	}
}

// TestNewWithOptions_WiresTheCronRunStoreIntoHealth: the probe is only useful
// if server construction actually hands it the scheduler. Built through the
// real constructor with a real persisting scheduler, so dropping the wiring —
// which the probe-level tests above cannot see — makes the section disappear.
func TestNewWithOptions_WiresTheCronRunStoreIntoHealth(t *testing.T) {
	sched := cron.NewScheduler(
		cron.SchedulerConfig{MaxJobs: 5, StorePath: filepath.Join(t.TempDir(), "cron_jobs.json"), AllowNilRouter: true},
		cron.SchedulerDeps{},
	)
	srv := NewWithOptions(ServerOptions{
		Addr:           ":0",
		Router:         session.NewRouter(session.RouterConfig{}),
		Platforms:      map[string]platform.Platform{"test": &mockPlatform{}},
		Backend:        "claude",
		DashboardToken: "secret",
		Scheduler:      sched,
	})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	srv.healthH.handleHealth(w, req)

	var body struct {
		RunStores map[string]map[string]float64 `json:"run_stores"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body.RunStores["cron"]; !ok {
		t.Errorf("run_stores.cron missing although a persisting scheduler was configured; body=%s", w.Body.String())
	}
}
