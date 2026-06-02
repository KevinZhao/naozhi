package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// newProjectsListForStableKeyTest materialises a one-project workspace and
// returns the decoded /api/projects rows for the given enabled flag.
func newProjectsListForStableKeyTest(t *testing.T, enabled bool) (rows []map[string]any, projPath string) {
	t.Helper()
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
	srv := NewWithOptions(ServerOptions{
		Addr:                    ":0",
		Router:                  router,
		Platforms:               map[string]platform.Platform{"test": &mockPlatform{}},
		Backend:                 "claude",
		ProjectManager:          mgr,
		ProjectStableKeyEnabled: enabled,
	})
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	w := httptest.NewRecorder()
	srv.projectH.HandleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	if len(rows) == 0 {
		t.Fatal("projects array empty")
	}
	return rows, projDir
}

// TestProjectsList_StableKeyEmittedWhenEnabled: with the feature on, each
// project row carries a well-formed dashboard:pj:<hash>:general stable key
// matching session.ProjectStableKey for that path.
func TestProjectsList_StableKeyEmittedWhenEnabled(t *testing.T) {
	rows, projPath := newProjectsListForStableKeyTest(t, true)
	got, _ := rows[0]["stableKey"].(string)
	if got == "" {
		t.Fatal("stableKey missing when feature enabled")
	}
	want := session.ProjectStableKey(projPath, "general")
	if got != want {
		t.Errorf("stableKey = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "dashboard:pj:") || !strings.HasSuffix(got, ":general") {
		t.Errorf("stableKey shape unexpected: %q", got)
	}
}

// TestProjectsList_StableKeyOmittedWhenDisabled: with the feature off, the
// omitempty field is absent so the frontend falls back to timestamp keys.
func TestProjectsList_StableKeyOmittedWhenDisabled(t *testing.T) {
	rows, _ := newProjectsListForStableKeyTest(t, false)
	if v, ok := rows[0]["stableKey"]; ok && v != "" {
		t.Errorf("stableKey should be omitted when disabled, got %v", v)
	}
}
