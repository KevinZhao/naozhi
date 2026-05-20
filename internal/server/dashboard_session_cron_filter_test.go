package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/cli"
	"github.com/naozhi/naozhi/internal/session"
)

// TestSessionsList_CronFilteredOut pins the cron-panel-consolidation RFC's
// post-condition: cron stub sessions registered for the cron scheduler's
// session-routing reuse must NEVER surface in the GET /api/sessions response.
//
// Before this filter the frontend kept a `cronVisibleKeys` whitelist to hide
// cron rows from the sidebar; now the canonical exclusion is server-side so
// the dashboard, the reverse-RPC forwarder, and any external IM caller all
// see a uniform "no cron" view.
//
// The test seeds the router with one normal IM session plus two cron stubs
// (one ready, one running), then asserts:
//
//   - the sessions[] payload contains the IM session and zero rows whose
//     key starts with "cron:";
//   - stats.running and stats.ready remain inclusive of cron CLI processes
//     so operators still see maxProcs pressure during cron execution.
func TestSessionsList_CronFilteredOut(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	// One real IM session — must appear in the response.
	imKey := "feishu:direct:alice:general"
	imProc := session.NewTestProcess()
	// NewTestProcess defaults to StateReady so the IM session is "ready".
	srv.router.InjectSession(imKey, imProc)

	// Two cron stubs — must be filtered out of sessions[] but counted in stats.
	cronReadyKey := session.CronKey("job-ready")
	cronReadyProc := session.NewTestProcess()
	srv.router.InjectSession(cronReadyKey, cronReadyProc)

	cronRunningKey := session.CronKey("job-running")
	cronRunningProc := session.NewTestProcess()
	cronRunningProc.StateVal = cli.StateRunning
	srv.router.InjectSession(cronRunningKey, cronRunningProc)

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}

	// Cheap string check first — guarantees no cron prefix anywhere in the
	// JSON, including nested fields like death reasons that may quote keys.
	if strings.Contains(w.Body.String(), "\"cron:") {
		t.Errorf("response contains cron: prefix — sidebar must hide cron stubs.\nbody=%s", w.Body.String())
	}

	var resp struct {
		Sessions []map[string]any `json:"sessions"`
		Stats    struct {
			Running int `json:"running"`
			Ready   int `json:"ready"`
			Total   int `json:"total"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}

	// Exactly one row, the IM session.
	if len(resp.Sessions) != 1 {
		t.Fatalf("sessions[] len = %d, want 1; rows=%v", len(resp.Sessions), resp.Sessions)
	}
	if got, _ := resp.Sessions[0]["key"].(string); got != imKey {
		t.Errorf("sessions[0].key = %q, want %q", got, imKey)
	}

	// Stats must still account for cron CLI process pressure: 1 ready IM +
	// 1 ready cron = 2 ready; 1 running cron = 1 running.
	if resp.Stats.Ready != 2 {
		t.Errorf("stats.ready = %d, want 2 (1 IM + 1 cron stub ready)", resp.Stats.Ready)
	}
	if resp.Stats.Running != 1 {
		t.Errorf("stats.running = %d, want 1 (cron stub running)", resp.Stats.Running)
	}
}

// TestSessionsList_CronFilterRespectsCronKeyPrefix guards against prefix
// drift: any future addition to session.CronKeyPrefix must remain consistent
// with the filter at handleList. The test injects a session whose key is
// exactly "{CronKeyPrefix}edge" and asserts it is filtered, so a refactor
// that hardcodes "cron:" elsewhere would not silently bypass this filter.
func TestSessionsList_CronFilterRespectsCronKeyPrefix(t *testing.T) {
	srv := newTestServer(&mockPlatform{})
	key := session.CronKeyPrefix + "edge"
	proc := session.NewTestProcess()
	srv.router.InjectSession(key, proc)

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), key) {
		t.Errorf("response leaks cron-prefixed key %q; body=%s", key, w.Body.String())
	}
}
