package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/platform"
	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// TestProjectScanTick_BumpsRouterVersionOnChange: the dashboard's
// /api/sessions poll is gated on stats.version — when the project list
// changes but the router version does not move, dashboard.js short-circuits
// the render and the sidebar never shows the new (or drops the removed)
// project. The scan tick must therefore BumpVersion, not just broadcast.
func TestProjectScanTick_BumpsRouterVersionOnChange(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "alpha"), 0o755); err != nil {
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
		Addr:           ":0",
		Router:         router,
		Platforms:      map[string]platform.Platform{"test": &mockPlatform{}},
		Backend:        "claude",
		ProjectManager: mgr,
	})
	srv.registerDashboard()

	// No change → no bump.
	before := router.Version()
	srv.projectScanTick()
	if got := router.Version(); got != before {
		t.Fatalf("unchanged project set bumped version %d → %d", before, got)
	}

	// Added directory → bump.
	if err := os.MkdirAll(filepath.Join(root, "beta"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv.projectScanTick()
	afterAdd := router.Version()
	if afterAdd == before {
		t.Fatalf("project added but router version stayed %d", before)
	}

	// Removed directory → bump.
	if err := os.RemoveAll(filepath.Join(root, "alpha")); err != nil {
		t.Fatal(err)
	}
	srv.projectScanTick()
	if got := router.Version(); got == afterAdd {
		t.Fatalf("project removed but router version stayed %d", afterAdd)
	}
}
