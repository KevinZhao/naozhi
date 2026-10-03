package project

import (
	"path/filepath"
	"testing"
)

// projects.exclude hides a matching directory as completely as a leading dot:
// no project, no binding, no index entry, and the legacy-stub sweep leaves its
// files alone. Matching is a case-sensitive basename glob.
func TestScan_ExcludeSkipsMatchingDirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	makeProjectDir(t, root, "alpha", nil)
	makeProjectDir(t, root, "beta", nil)
	makeProjectDir(t, root, "Logs", nil)
	makeProjectDir(t, root, "archive", nil)
	makeProjectDir(t, root, "tmp-1", &ProjectConfig{
		ChatBindings: []ChatBinding{{Platform: "feishu", ChatType: "group", ChatID: "oc_1"}},
	})
	stub := writeStub(t, root, "tmp-x", 100)
	indexPath := filepath.Join(t.TempDir(), "projects-index.json")

	m, err := NewManager(root, PlannerDefaults{},
		WithExclude([]string{"tmp-*", "archive", "logs"}), WithIndexPath(indexPath))
	if err != nil {
		t.Fatalf("NewManager = %v", err)
	}
	if err := m.Scan(); err != nil {
		t.Fatalf("Scan = %v", err)
	}

	if got := projectNames(m.All()); got != "Logs,alpha,beta" {
		t.Errorf("projects = %s, want Logs,alpha,beta", got)
	}
	if p := m.ProjectForChat("feishu", "group", "oc_1"); p != nil {
		t.Errorf("binding of excluded tmp-1 resolved to %q", p.Name)
	}
	f := readIndexFile(t, indexPath)
	for _, name := range []string{"archive", "tmp-1", "tmp-x"} {
		if ms, ok := f.CreatedAt[filepath.Join(root, name)]; ok {
			t.Errorf("index has excluded %s (created_at %d)", name, ms)
		}
	}
	if len(f.StubCleanupDone) != 1 {
		t.Fatalf("stub_cleanup_done = %q, want the sweep to have run", f.StubCleanupDone)
	}
	assertExists(t, stub, true)
}

// An excluded subdirectory no longer claims the root's basename, so
// include_root can register the root project under it.
func TestScan_ExcludedSubdirFreesIncludeRootName(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "workspace")
	makeProjectDir(t, root, "workspace", nil)
	makeProjectDir(t, root, "alpha", nil)

	m, err := NewManager(root, PlannerDefaults{},
		WithIncludeRoot(true), WithExclude([]string{"work*"}))
	if err != nil {
		t.Fatalf("NewManager = %v", err)
	}
	if err := m.Scan(); err != nil {
		t.Fatalf("Scan = %v", err)
	}
	p := m.Get("workspace")
	if p == nil || !p.IsRoot || p.Path != root {
		t.Fatalf("Get(workspace) = %+v, want the root project at %s", p, root)
	}
	if got := projectNames(m.All()); got != "alpha,workspace" {
		t.Errorf("projects = %s, want alpha,workspace", got)
	}
}

// Library callers bypass config validation, so NewManager refuses a pattern
// filepath.Match would reject instead of silently excluding nothing.
func TestNewManager_RejectsInvalidExclude(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, pat := range []string{"[", "a/b", ""} {
		if _, err := NewManager(root, PlannerDefaults{}, WithExclude([]string{"ok", pat})); err == nil {
			t.Errorf("NewManager(exclude %q) = nil error", pat)
		}
	}
}
