package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/naozhi/naozhi/internal/project"
	"github.com/naozhi/naozhi/internal/session"
)

// TestBuildSessionOpts_PlannerKeyColonName covers R20260531-QUAL-4: a
// project whose directory name contains ':' (e.g. "my:proj") must still
// resolve its planner workspace. The old SplitN(key,":",4)[1] extraction
// truncated the name at the first colon ("my"), so projectMgr.Get failed
// and the planner session spawned with no Workspace override. The fix
// strips the fixed "project:" prefix and ":planner" suffix instead — the
// exact inverse of PlannerKeyFor — so the full name survives.
func TestBuildSessionOpts_PlannerKeyColonName(t *testing.T) {
	root := t.TempDir()
	const colonName = "my:proj"
	projDir := filepath.Join(root, colonName)
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", projDir, err)
	}

	mgr, err := project.NewManager(root, project.PlannerDefaults{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Scan(); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if mgr.Get(colonName) == nil {
		t.Fatalf("precondition: project %q not registered after Scan", colonName)
	}

	key := project.PlannerKeyFor(colonName) // "project:my:proj:planner"
	if !project.IsPlannerKey(key) {
		t.Fatalf("precondition: %q is not a planner key", key)
	}

	agents := map[string]session.AgentOpts{"general": {}}
	// resolver=nil exercises the legacy inline planner branch directly.
	opts := buildSessionOpts(key, nil, agents, mgr)

	if !opts.Exempt {
		t.Errorf("planner opts.Exempt = false, want true")
	}
	wantWS := filepath.Join(root, colonName)
	if opts.Workspace != wantWS {
		t.Errorf("planner Workspace = %q, want %q (colon name was truncated?)", opts.Workspace, wantWS)
	}
}

// TestBuildSessionOpts_PlannerKeySimpleName guards the common no-colon case
// so the Trim-based extraction stays behaviourally identical to the old
// parts[1] path for ordinary names.
func TestBuildSessionOpts_PlannerKeySimpleName(t *testing.T) {
	root := t.TempDir()
	const name = "webapp"
	if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mgr, err := project.NewManager(root, project.PlannerDefaults{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.Scan(); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	opts := buildSessionOpts(project.PlannerKeyFor(name), nil, map[string]session.AgentOpts{"general": {}}, mgr)
	if !opts.Exempt {
		t.Errorf("planner opts.Exempt = false, want true")
	}
	if want := filepath.Join(root, name); opts.Workspace != want {
		t.Errorf("planner Workspace = %q, want %q", opts.Workspace, want)
	}
}
