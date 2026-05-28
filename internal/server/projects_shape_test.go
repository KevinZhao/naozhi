package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// TestDashboardJSON_Projects_ShapeContract pins the wire shape of
// GET /api/projects so dashboard.js (renderProjectsList in
// internal/server/static/dashboard.js) and curl/IM tooling keep working.
//
// WHY: naozhi has no API versioning and no OpenAPI spec; regex-based shape
// tests are the sole guard against a silent refactor dropping a key
// dashboard.js relies on (RNEW-ARCH-401 in docs/TODO.md). The handler
// returns a raw JSON array of per-project objects carrying name/path/
// config/favorite/git_remote_url/github + planner_state/planner_model.
//
// INTERPRETING A FAILURE:
//   - Mistake (fix the handler): a field in `required` was removed or
//     renamed; restore it or update dashboard.js + this test together.
//   - Intentional addition (update this test): a NEW field was added
//     (e.g. display_name once Round 208 lands). Add the field name here
//     and note it in RNEW-ARCH-401.
//
// Scope: top-level is an array (not an object), so only per-project keys
// are enforced. dashproject.RedactGitRemoteURL is separately tested — we only pin
// presence of git_remote_url, not its value.
func TestDashboardJSON_Projects_ShapeContract(t *testing.T) {
	// Materialize a temp workspace with one demo project so handleList walks
	// a non-empty projectMgr.All() and emits a populated row. Mirrors the
	// setup used by newProjectHandlersForTest (project_files_test.go).
	root := t.TempDir()
	projDir := filepath.Join(root, "demo")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "CLAUDE.md"), []byte("# demo"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr, err := project.NewManager(root, project.PlannerDefaults{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Scan(); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	router := session.NewRouter(session.RouterConfig{})
	platforms := map[string]platform.Platform{"test": &mockPlatform{}}
	srv := NewWithOptions(ServerOptions{
		Addr:           ":0",
		Router:         router,
		Platforms:      platforms,
		Backend:        "claude",
		ProjectManager: mgr,
	})
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	w := httptest.NewRecorder()
	srv.projectH.HandleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}

	var resp []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if len(resp) == 0 {
		t.Fatal("projects array empty — Manager.Scan should have found the demo fixture")
	}
	first := resp[0]
	// Required per-project keys. Conservative set: keys dashboard.js
	// dereferences unconditionally on every render. "config" is the
	// nested ProjectConfig object (schedule, planner prompt, favorite
	// mirror); its internal shape is pinned by project package tests, so
	// here we only assert its presence at this layer.
	required := []string{
		"name", "path", "planner_state", "planner_model",
		"config", "favorite", "git_remote_url", "github",
	}
	for _, k := range required {
		if _, ok := first[k]; !ok {
			t.Errorf("projects[0].%s missing; body=%s", k, w.Body.String())
		}
	}
}
