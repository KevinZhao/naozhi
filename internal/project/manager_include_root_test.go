package project

import (
	"os"
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
	if !rootProj.IsRoot {
		t.Error("root project IsRoot = false, want true")
	}
	// Subdirectory projects must NOT be marked IsRoot.
	if sub := m.Get("alpha"); sub != nil && sub.IsRoot {
		t.Error("subdirectory project 'alpha' wrongly marked IsRoot")
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

// The root project must NOT cause Scan to write a .naozhi/project.yaml into the
// user's top-level workspace directory. The CreatedAt migration persists config
// for every other zero-CreatedAt project, but the synthetic root project is
// skipped (its CreatedAt is in-memory-only).
func TestScan_IncludeRoot_DoesNotWriteRootConfig(t *testing.T) {
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

	rootCfg := filepath.Join(root, ".naozhi", "project.yaml")
	if _, statErr := os.Stat(rootCfg); statErr == nil {
		t.Errorf("Scan wrote %s into the workspace root; include_root must not persist root config", rootCfg)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error: %v", statErr)
	}

	// The subdirectory project's config IS expected to be written by the
	// migration (it had no project.yaml → CreatedAt==0 → stamped+persisted).
	subCfg := filepath.Join(root, "alpha", ".naozhi", "project.yaml")
	if _, statErr := os.Stat(subCfg); statErr != nil {
		t.Errorf("expected migration to persist alpha config at %s: %v", subCfg, statErr)
	}

	// Root must still carry an in-memory CreatedAt so sidebar sorting is stable.
	rootProj := m.Get("workspace")
	if rootProj == nil {
		t.Fatal("root project missing")
	}
	if rootProj.Config.CreatedAt == 0 {
		t.Error("root project CreatedAt not synthesised (would sort unstably)")
	}
}

// The root project's synthetic CreatedAt must sort it strictly LAST so it lands
// at the bottom of the sidebar regardless of when the other projects were made.
func TestScan_IncludeRoot_RootSortsLast(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	// Give subdirs explicit high CreatedAt values to ensure root still beats them.
	makeProjectDir(t, root, "alpha", &ProjectConfig{CreatedAt: 1_000_000})
	makeProjectDir(t, root, "beta", &ProjectConfig{CreatedAt: 2_000_000})

	m, err := NewManager(root, PlannerDefaults{}, WithIncludeRoot(true))
	if err != nil {
		t.Fatalf("NewManager = %v", err)
	}
	if err := m.Scan(); err != nil {
		t.Fatalf("Scan = %v", err)
	}

	all := m.All() // sorted by CreatedAt ascending
	if len(all) != 3 {
		t.Fatalf("All() = %d, want 3", len(all))
	}
	last := all[len(all)-1]
	if !last.IsRoot {
		t.Errorf("last project = %q (IsRoot=%v), want the root project sorted last", last.Name, last.IsRoot)
	}
}
