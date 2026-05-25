package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/session"
)

// TestDashboardJSON_Sessions_ShapeContract pins the top-level wire shape of
// GET /api/sessions and the per-session object shape consumed by dashboard.js
// + external IM callers (feishu reply builder in internal/platform/feishu
// inspects sessions[].key / .state / .last_active to compose "who is idle"
// cards).
//
// WHY THIS CONTRACT EXISTS:
//   - naozhi has no dashboard API versioning and no OpenAPI spec; the only
//     defense against a silent refactor dropping a key dashboard.js relies on
//     is regex-based shape tests like this one (RNEW-ARCH-401 in docs/TODO.md).
//   - dashboard.js (internal/server/static/dashboard.js) treats "sessions"
//     as an array and per-row accesses .key, .state, .last_active, .workspace.
//     Renaming or removing any of those yields a silent empty-sidebar bug.
//
// INTERPRETING A FAILURE:
//   - Mistake (fix the handler): a field listed in the required set was
//     renamed, removed, or changed type. Restore the field or bump the
//     dashboard.js consumer in the same PR.
//   - Intentional addition (update this test): a NEW field was added. That's
//     backward-compatible on the wire; extend `allowed` below and note it in
//     RNEW-ARCH-401 so the versioning discussion captures the delta.
//
// Scope: this test only pins keys that have a non-theoretical consumer. It
// does NOT enforce optional-field presence (workspace/project/summary are
// omitempty and absent for bare sessions).
func TestDashboardJSON_Sessions_ShapeContract(t *testing.T) {
	router := session.NewRouter(session.RouterConfig{})
	// Inject one bare session so per-session assertions exercise a real row
	// instead of skipping. RegisterForResume is the lightest path that
	// produces a ManagedSession without spawning a real CLI subprocess.
	router.RegisterForResume("dashboard:direct:shape-contract:general", "sess-shape-1", "", "")
	platforms := map[string]platform.Platform{"test": &mockPlatform{}}
	srv := NewWithOptions(ServerOptions{
		Addr:      ":0",
		Router:    router,
		Platforms: platforms,
		Backend:   "claude",
	})
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.handleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}

	// Top-level required keys. stats sub-object shape is covered by
	// stats_shape_test.go; here we only assert the outer envelope.
	for _, k := range []string{"sessions", "stats"} {
		if _, ok := resp[k]; !ok {
			t.Fatalf("/api/sessions missing top-level %q; body=%s", k, w.Body.String())
		}
	}
	sessions, ok := resp["sessions"].([]any)
	if !ok {
		t.Fatalf("sessions wrong type: %T", resp["sessions"])
	}
	if len(sessions) == 0 {
		t.Fatal("sessions array empty — RegisterForResume did not register a row")
	}
	first, ok := sessions[0].(map[string]any)
	if !ok {
		t.Fatalf("sessions[0] wrong type: %T", sessions[0])
	}
	// Required per-session keys. Conservative set: keys dashboard.js and the
	// reverse-RPC forwarder both dereference unconditionally. Optional
	// omitempty fields (workspace, summary, project, death_reason, …) are
	// NOT enforced so a bare resume-only row still passes.
	for _, k := range []string{"key", "state", "last_active", "session_id"} {
		if _, ok := first[k]; !ok {
			t.Errorf("sessions[0].%s missing; body=%s", k, w.Body.String())
		}
	}
}
