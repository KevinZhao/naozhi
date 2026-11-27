package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIntegrationDashboardStack drives a whole request through the real
// *Server.mux — auth middleware + route table + handler together (#864).
// Without a dashboard token requireAuth passes through, so the request
// reaches the handler; SysessionManager is nil, whose contract is an empty
// JSON array.
func TestIntegrationDashboardStack(t *testing.T) {
	srv := newTestServer(&mockPlatform{})

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/system/daemons", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
	var daemons []json.RawMessage
	body := strings.TrimSpace(w.Body.String())
	if err := json.Unmarshal([]byte(body), &daemons); err != nil {
		t.Fatalf("response is not a JSON array: %v; body=%q", err, body)
	}
	if len(daemons) != 0 {
		t.Errorf("daemons = %d entries, want 0 (sysession disabled)", len(daemons))
	}
}
