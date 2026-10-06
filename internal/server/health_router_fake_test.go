package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naozhi/naozhi/internal/dashboard/auth"
	"github.com/naozhi/naozhi/internal/session"
)

// fakeHealthRouter is a healthRouter with canned answers; History and Runs
// return nil facets, which read as disabled.
type fakeHealthRouter struct {
	active, total int
	blocks        []session.StoreBlock
}

func (f fakeHealthRouter) Stats() (int, int)                      { return f.active, f.total }
func (fakeHealthRouter) Backends() *session.BackendRegistry       { return &session.BackendRegistry{} }
func (fakeHealthRouter) History() *session.HistoryIO              { return nil }
func (fakeHealthRouter) Runs() *session.RunLedger                 { return nil }
func (f fakeHealthRouter) StoreWriteBlocks() []session.StoreBlock { return f.blocks }

// HealthHandler reads the router only through healthRouter, so a fake drives
// the authenticated sessions / session_store sections and /readyz without a
// real session.Router.
func TestHealthHandler_FakeRouterDrivesSections(t *testing.T) {
	t.Parallel()
	fake := fakeHealthRouter{active: 3, total: 7, blocks: []session.StoreBlock{{
		Path: "/data/sessions.json", Label: "session store", Reason: "disk full", Since: time.Unix(0, 0).UTC(),
	}}}
	h := &HealthHandler{
		router:            fake,
		auth:              &auth.Handlers{}, // no token: every request is authenticated
		watchdogNoOut:     new(atomic.Int64),
		watchdogTotal:     new(atomic.Int64),
		nodeAccess:        newNodeRegistry(nil),
		dispatcherMetrics: func() (int64, int64, int64, time.Time) { return 0, 0, 0, time.Time{} },
	}

	w := httptest.NewRecorder()
	h.handleHealth(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	var got struct {
		Sessions struct {
			Active int `json:"active"`
			Total  int `json:"total"`
		} `json:"sessions"`
		SessionStore *struct {
			Blocked []struct {
				Path string `json:"path"`
			} `json:"blocked"`
		} `json:"session_store"`
		EventLog *json.RawMessage `json:"eventlog"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /health: %v\n%s", err, w.Body.String())
	}
	if got.Sessions.Active != 3 || got.Sessions.Total != 7 {
		t.Errorf("sessions = %+v, want the fake's 3/7", got.Sessions)
	}
	if got.SessionStore == nil || len(got.SessionStore.Blocked) != 1 || got.SessionStore.Blocked[0].Path != "/data/sessions.json" {
		t.Errorf("session_store = %+v, want the fake's one blocked file", got.SessionStore)
	}
	if got.EventLog != nil {
		t.Errorf("eventlog = %s with a nil History facet, want omitted", *got.EventLog)
	}

	w = httptest.NewRecorder()
	h.handleReadyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "ready" {
		t.Errorf("/readyz = %d %q, want 200 ready", w.Code, w.Body.String())
	}
}

// routerViews is buildServerWithHandlers' one typed-nil unwrap. A nil
// *session.Router boxed straight into the views would read non-nil: /readyz
// would answer "ready" with no router and projectScanTick's BumpVersion guard
// would call through nil.
func TestRouterViews_NilRouterGivesNilInterfaces(t *testing.T) {
	t.Parallel()
	srvRouter, healthR := routerViews(nil)
	if srvRouter != nil || healthR != nil {
		t.Fatalf("routerViews(nil) = %#v, %#v; want nil interfaces", srvRouter, healthR)
	}
	w := httptest.NewRecorder()
	(&HealthHandler{router: healthR}).handleReadyz(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz with no router = %d %q, want 503", w.Code, w.Body.String())
	}
}
