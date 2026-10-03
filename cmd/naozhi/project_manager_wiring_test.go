package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/config"
	"github.com/naozhi/naozhi/internal/datadir"
)

// newProjectManager is main's only route from cfg.Projects to the Manager, so
// each option is asserted by its effect on Scan rather than by its presence.
func TestNewProjectManager_WiresProjectsConfig(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"alpha", "tmp-1"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{}
	cfg.Projects.Root = root
	cfg.Projects.IncludeRoot = true
	cfg.Projects.Exclude = []string{"tmp-*"}
	cfg.Projects.PlannerDefaults.Model = "opus"
	cfg.Projects.PlannerDefaults.Prompt = "plan carefully"
	layout := datadir.ForStore(filepath.Join(t.TempDir(), "sessions.json"))

	mgr, err := newProjectManager(cfg, layout)
	if err != nil {
		t.Fatalf("newProjectManager = %v", err)
	}
	if err := mgr.Scan(); err != nil {
		t.Fatalf("Scan = %v", err)
	}
	if mgr.Get("tmp-1") != nil {
		t.Error("projects.exclude did not reach the Manager: tmp-1 was discovered")
	}
	alpha := mgr.Get("alpha")
	if alpha == nil {
		t.Fatal("alpha was not discovered")
	}
	if got := mgr.EffectivePlannerModel(alpha); got != "opus" {
		t.Errorf("planner_defaults.model = %q at the Manager, want opus", got)
	}
	if got := mgr.EffectivePlannerPrompt(alpha); got != "plan carefully" {
		t.Errorf("planner_defaults.prompt = %q at the Manager, want %q", got, "plan carefully")
	}
	if mgr.Get(filepath.Base(root)) == nil {
		t.Error("projects.include_root did not reach the Manager: root project missing")
	}
	if _, err := os.Stat(layout.ProjectsIndexPath()); err != nil {
		t.Errorf("projects index not written at the layout path: %v", err)
	}
}
