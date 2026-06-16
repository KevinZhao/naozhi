package project

import (
	"path/filepath"
	"testing"
)

// ---- IncludeRoot ----

// When IncludeRoot is enabled, the root directory itself is registered as a
// project (named after its basename) in addition to its subdirectories.
func TestScan_IncludeRoot_RegistersRootProject(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	makeProjectDir(t, root, "alpha", nil)
	makeProjectDir(t, root, "beta", nil)

	m, err := NewManager(root, PlannerDefaults{}, WithIncludeRoot(true))
	if err != nil {
		t.Fatalf("NewManager = %v", err)
	}
	if err := m.Scan(); err != nil {
		t.Fatalf("Scan = %v", err)
	}

	rootProj := m.Get("workspace")
	if rootProj == nil {
		t.Fatal("root project 'workspace' not registered")
	}
	if rootProj.Path != root {
		t.Errorf("root project Path = %q, want %q", rootProj.Path, root)
	}
	if rootProj.PathPrefix != root+string(filepath.Separator) {
		t.Errorf("root project PathPrefix = %q, want %q", rootProj.PathPrefix, root+string(filepath.Separator))
	}
	// Subdirectory projects still present alongside the root project.
	if m.Get("alpha") == nil || m.Get("beta") == nil {
		t.Error("subdirectory projects missing after include-root scan")
	}
	if all := m.All(); len(all) != 3 {
		t.Errorf("All() = %d projects, want 3 (alpha, beta, workspace)", len(all))
	}
}

// Default (no option) must not register the root project — back-compat.
func TestScan_IncludeRootDisabled_NoRootProject(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	makeProjectDir(t, root, "alpha", nil)

	m, err := NewManager(root, PlannerDefaults{})
	if err != nil {
		t.Fatalf("NewManager = %v", err)
	}
	if err := m.Scan(); err != nil {
		t.Fatalf("Scan = %v", err)
	}
	if m.Get("workspace") != nil {
		t.Error("root project registered without WithIncludeRoot(true)")
	}
	if all := m.All(); len(all) != 1 {
		t.Errorf("All() = %d, want 1 (alpha only)", len(all))
	}
}

// A real subdirectory whose name equals the root basename must win; the root
// project is skipped rather than shadowing the subdirectory.
func TestScan_IncludeRoot_SubdirNameClash_SubdirWins(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	makeProjectDir(t, root, "workspace", nil) // subdir literally named "workspace"

	m, err := NewManager(root, PlannerDefaults{}, WithIncludeRoot(true))
	if err != nil {
		t.Fatalf("NewManager = %v", err)
	}
	if err := m.Scan(); err != nil {
		t.Fatalf("Scan = %v", err)
	}

	p := m.Get("workspace")
	if p == nil {
		t.Fatal("project 'workspace' missing")
	}
	wantSubdir := filepath.Join(root, "workspace")
	if p.Path != wantSubdir {
		t.Errorf("clash resolved to %q, want subdirectory %q (root must not shadow it)", p.Path, wantSubdir)
	}
	if all := m.All(); len(all) != 1 {
		t.Errorf("All() = %d, want 1 (subdir only, root skipped)", len(all))
	}
}

// ResolveWorkspaces must keep longest-prefix semantics: a file under a deeper
// subdirectory project resolves there, while a file directly under root (not
// inside any subdirectory) resolves to the root project.
func TestResolveWorkspaces_IncludeRoot_LongestPrefixWins(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	makeProjectDir(t, root, "alpha", nil)

	m, err := NewManager(root, PlannerDefaults{}, WithIncludeRoot(true))
	if err != nil {
		t.Fatalf("NewManager = %v", err)
	}
	if err := m.Scan(); err != nil {
		t.Fatalf("Scan = %v", err)
	}

	insideAlpha := filepath.Join(root, "alpha", "src", "main.go")
	directlyUnderRoot := filepath.Join(root, "notes.md")
	got := m.ResolveWorkspaces([]string{insideAlpha, directlyUnderRoot})

	if got[insideAlpha] != "alpha" {
		t.Errorf("file inside alpha resolved to %q, want \"alpha\"", got[insideAlpha])
	}
	if got[directlyUnderRoot] != "workspace" {
		t.Errorf("file directly under root resolved to %q, want \"workspace\"", got[directlyUnderRoot])
	}
}
