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

// TestSessionsStatsProjects_CarriesPaletteFields: dashboard.js populates
// projectsData ONLY from /api/sessions stats.projects, and reads
// p.stableKey (continue-session), p.dir_mtime (picker order) and
// p.config.emoji / display_name (labels). Those fields only existed on
// /api/projects, so the palette silently degraded. Pin them on the
// /api/sessions path as well.
func TestSessionsStatsProjects_CarriesPaletteFields(t *testing.T) {
	root := t.TempDir()
	projDir := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(projDir, ".naozhi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, ".naozhi", "project.yaml"),
		[]byte("display_name: Demo\nemoji: \"🚀\"\n"), 0o644); err != nil {
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
		ProjectStableKeyEnabled: true,
	})
	srv.registerDashboard()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Stats struct {
			Projects []map[string]any `json:"projects"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Stats.Projects) != 1 {
		t.Fatalf("projects = %d, want 1 (%s)", len(resp.Stats.Projects), w.Body.String())
	}
	p := resp.Stats.Projects[0]
	if sk, _ := p["stableKey"].(string); sk != session.ProjectStableKey(projDir, "general") {
		t.Errorf("stableKey = %v, want %s", p["stableKey"], session.ProjectStableKey(projDir, "general"))
	}
	if mt, _ := p["dir_mtime"].(float64); mt <= 0 {
		t.Errorf("dir_mtime = %v, want > 0", p["dir_mtime"])
	}
	cfg, _ := p["config"].(map[string]any)
	if cfg == nil {
		t.Fatalf("config missing: %v", p)
	}
	if cfg["display_name"] != "Demo" || cfg["emoji"] != "🚀" {
		t.Errorf("config = %v, want display_name=Demo emoji=🚀", cfg)
	}
	// Parity guard: every key /api/projects emits that the palette reads
	// must also be present here.
	for _, k := range []string{"name", "path", "node", "created_at", "stableKey", "dir_mtime", "config"} {
		if _, ok := p[k]; !ok {
			t.Errorf("stats.projects[0] missing %q: %v", k, p)
		}
	}
}

// TestSessionsStatsProjects_EmptyListIsArrayNotOmitted: after the last
// project is removed the dashboard must receive `projects: []` so it clears
// the stale list; omitempty dropped the key and the old entries lingered.
func TestSessionsStatsProjects_EmptyListIsArrayNotOmitted(t *testing.T) {
	root := t.TempDir()
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

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	srv.sessionH.HandleList(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Stats map[string]json.RawMessage `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, ok := resp.Stats["projects"]
	if !ok {
		t.Fatalf("stats.projects omitted; body=%s", w.Body.String())
	}
	if string(raw) != "[]" {
		t.Fatalf("stats.projects = %s, want []", raw)
	}
}
