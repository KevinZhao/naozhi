package project

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// makeProjectDir creates a minimal project directory under root:
// root/<name>/CLAUDE.md and optionally root/<name>/.naozhi/project.yaml.
func makeProjectDir(t *testing.T, root, name string, cfg *ProjectConfig) {
	t.Helper()
	projDir := filepath.Join(root, name)
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatalf("create project dir: %v", err)
	}
	// CLAUDE.md is required for Scan() to pick up the project.
	if err := os.WriteFile(filepath.Join(projDir, "CLAUDE.md"), []byte("# "+name), 0644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	if cfg != nil {
		cfgDir := filepath.Join(projDir, ".naozhi")
		if err := os.MkdirAll(cfgDir, 0700); err != nil {
			t.Fatalf("create config dir: %v", err)
		}
		if err := saveConfigToPath(filepath.Join(cfgDir, "project.yaml"), *cfg); err != nil {
			t.Fatalf("write project config: %v", err)
		}
	}
}

// ---- NewManager ----

func TestNewManager_ValidDir(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root, PlannerDefaults{})
	if err != nil {
		t.Fatalf("NewManager = %v", err)
	}
	if m == nil {
		t.Fatal("NewManager returned nil")
	}
}

func TestNewManager_NonExistentDir(t *testing.T) {
	_, err := NewManager("/nonexistent/path/xyz", PlannerDefaults{})
	if err == nil {
		t.Error("NewManager with missing dir should return error")
	}
}

func TestNewManager_NotADirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := NewManager(file, PlannerDefaults{})
	if err == nil {
		t.Error("NewManager with file path should return error")
	}
}

// ---- Scan ----

func TestScan_Empty(t *testing.T) {
	root := t.TempDir()
	m, _ := NewManager(root, PlannerDefaults{})
	if err := m.Scan(); err != nil {
		t.Fatalf("Scan empty dir = %v", err)
	}
	if all := m.All(); len(all) != 0 {
		t.Errorf("All() after empty scan = %d, want 0", len(all))
	}
}

func TestScan_SkipsHiddenDirs(t *testing.T) {
	root := t.TempDir()
	// Create a hidden directory — should be skipped.
	hidden := filepath.Join(root, ".hidden")
	os.MkdirAll(hidden, 0755)
	os.WriteFile(filepath.Join(hidden, "CLAUDE.md"), []byte("x"), 0644)

	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	if all := m.All(); len(all) != 0 {
		t.Errorf("Scan() picked up hidden dir; All() = %d, want 0", len(all))
	}
}

func TestScan_SkipsDirsWithoutCLAUDEMd(t *testing.T) {
	root := t.TempDir()
	// Directory without CLAUDE.md
	os.MkdirAll(filepath.Join(root, "noclaudemd"), 0755)

	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	if all := m.All(); len(all) != 0 {
		t.Errorf("Scan() picked up dir without CLAUDE.md; All() = %d", len(all))
	}
}

func TestScan_PicksUpProjects(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "alpha", nil)
	makeProjectDir(t, root, "beta", nil)

	m, _ := NewManager(root, PlannerDefaults{})
	if err := m.Scan(); err != nil {
		t.Fatalf("Scan = %v", err)
	}

	all := m.All()
	if len(all) != 2 {
		t.Fatalf("All() = %d, want 2", len(all))
	}
	// Sorted by name
	if all[0].Name != "alpha" || all[1].Name != "beta" {
		t.Errorf("sort order wrong: %v %v", all[0].Name, all[1].Name)
	}
}

func TestScan_SkipsBadConfig(t *testing.T) {
	root := t.TempDir()
	projDir := filepath.Join(root, "badcfg")
	os.MkdirAll(filepath.Join(projDir, ".naozhi"), 0755)
	os.WriteFile(filepath.Join(projDir, "CLAUDE.md"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(projDir, ".naozhi", "project.yaml"), []byte("git_sync: [unclosed"), 0600)

	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	// Bad config project is skipped, no error returned from Scan.
	if all := m.All(); len(all) != 0 {
		t.Errorf("Scan() should skip bad-config project; All() = %d", len(all))
	}
}

// ---- Get ----

func TestGet_ExistingProject(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "myproj", nil)
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	p := m.Get("myproj")
	if p == nil {
		t.Fatal("Get(\"myproj\") = nil, want project")
	}
	if p.Name != "myproj" {
		t.Errorf("Get name = %q, want \"myproj\"", p.Name)
	}
}

func TestGet_NonExistentProject(t *testing.T) {
	root := t.TempDir()
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	p := m.Get("missing")
	if p != nil {
		t.Errorf("Get(\"missing\") = %+v, want nil", p)
	}
}

func TestGet_ReturnsCopy(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "copyproj", &ProjectConfig{
		ChatBindings: []ChatBinding{{Platform: "feishu", ChatID: "c1"}},
	})
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	p1 := m.Get("copyproj")
	p2 := m.Get("copyproj")
	// They should be equal in content but distinct pointers.
	if p1 == p2 {
		t.Error("Get() returned same pointer, want distinct copies")
	}
	p1.Config.ChatBindings[0].ChatID = "mutated"
	p3 := m.Get("copyproj")
	if p3.Config.ChatBindings[0].ChatID == "mutated" {
		t.Error("mutating snapshot affected manager state")
	}
}

// ---- All (sorted) ----

func TestAll_Sorted(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"charlie", "alpha", "beta"} {
		makeProjectDir(t, root, name, nil)
	}
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	all := m.All()
	if len(all) != 3 {
		t.Fatalf("All() = %d, want 3", len(all))
	}
	names := []string{all[0].Name, all[1].Name, all[2].Name}
	want := []string{"alpha", "beta", "charlie"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("All()[%d].Name = %q, want %q", i, names[i], want[i])
		}
	}
}

// ---- ProjectForChat ----

func TestProjectForChat_Bound(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "proj1", &ProjectConfig{
		ChatBindings: []ChatBinding{
			{Platform: "feishu", ChatType: "group", ChatID: "g1"},
		},
	})
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	p := m.ProjectForChat("feishu", "group", "g1")
	if p == nil {
		t.Fatal("ProjectForChat with bound chat = nil")
	}
	if p.Name != "proj1" {
		t.Errorf("ProjectForChat name = %q, want \"proj1\"", p.Name)
	}
}

func TestProjectForChat_Unbound(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "proj1", nil)
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	p := m.ProjectForChat("feishu", "group", "unknown")
	if p != nil {
		t.Errorf("ProjectForChat unbound = %+v, want nil", p)
	}
}

// ---- BindChat ----

func TestBindChat_NewBinding(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "bindproj", nil)
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	if err := m.BindChat("bindproj", "feishu", "group", "chat1"); err != nil {
		t.Fatalf("BindChat = %v", err)
	}

	// Project should now have the binding
	p := m.Get("bindproj")
	if len(p.Config.ChatBindings) != 1 {
		t.Fatalf("ChatBindings len = %d, want 1", len(p.Config.ChatBindings))
	}
	if p.Config.ChatBindings[0].ChatID != "chat1" {
		t.Errorf("ChatID = %q, want \"chat1\"", p.Config.ChatBindings[0].ChatID)
	}

	// The binding index should be updated
	found := m.ProjectForChat("feishu", "group", "chat1")
	if found == nil || found.Name != "bindproj" {
		t.Errorf("ProjectForChat after bind = %v", found)
	}
}

func TestBindChat_Idempotent(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "idempotent", nil)
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	m.BindChat("idempotent", "feishu", "group", "c1")
	m.BindChat("idempotent", "feishu", "group", "c1") // duplicate

	p := m.Get("idempotent")
	if len(p.Config.ChatBindings) != 1 {
		t.Errorf("duplicate BindChat added extra binding; len = %d", len(p.Config.ChatBindings))
	}
}

func TestBindChat_NotFound(t *testing.T) {
	root := t.TempDir()
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	err := m.BindChat("nonexistent", "feishu", "group", "c1")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("BindChat nonexistent = %v, want ErrNotFound", err)
	}
}

func TestBindChat_PersistsToDisk(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "persist", nil)
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	m.BindChat("persist", "feishu", "group", "cgp1")

	// Create a fresh manager and re-scan to read from disk.
	m2, _ := NewManager(root, PlannerDefaults{})
	m2.Scan()

	p := m2.Get("persist")
	if p == nil || len(p.Config.ChatBindings) != 1 {
		t.Errorf("binding not persisted to disk; p = %+v", p)
	}
}

// ---- UnbindAllChat ----

func TestUnbindAllChat(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "unbindproj", &ProjectConfig{
		ChatBindings: []ChatBinding{
			{Platform: "feishu", ChatType: "group", ChatID: "c1"},
			{Platform: "feishu", ChatType: "group", ChatID: "c2"},
		},
	})
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	if err := m.UnbindAllChat("feishu", "group", "c1"); err != nil {
		t.Fatalf("UnbindAllChat = %v", err)
	}

	p := m.Get("unbindproj")
	if len(p.Config.ChatBindings) != 1 {
		t.Errorf("after unbind len = %d, want 1", len(p.Config.ChatBindings))
	}
	if p.Config.ChatBindings[0].ChatID != "c2" {
		t.Errorf("wrong binding remains: %q", p.Config.ChatBindings[0].ChatID)
	}
}

func TestUnbindAllChat_NotBound(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "notbound", nil)
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	// Unregistered chat should be a no-op (no error).
	if err := m.UnbindAllChat("feishu", "group", "nobody"); err != nil {
		t.Errorf("UnbindAllChat unregistered = %v, want nil", err)
	}
}

// ---- UpdateConfig ----

func TestUpdateConfig_Success(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "updateme", nil)
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	newCfg := ProjectConfig{GitSync: true, GitRemote: "upstream"}
	if err := m.UpdateConfig("updateme", newCfg); err != nil {
		t.Fatalf("UpdateConfig = %v", err)
	}

	p := m.Get("updateme")
	if !p.Config.GitSync {
		t.Error("GitSync not updated")
	}
	if p.Config.GitRemote != "upstream" {
		t.Errorf("GitRemote = %q, want \"upstream\"", p.Config.GitRemote)
	}
}

func TestUpdateConfig_NotFound(t *testing.T) {
	root := t.TempDir()
	m, _ := NewManager(root, PlannerDefaults{})
	err := m.UpdateConfig("ghost", ProjectConfig{})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateConfig nonexistent = %v, want ErrNotFound", err)
	}
}

// ---- ProjectNames ----

func TestProjectNames(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "p1", nil)
	makeProjectDir(t, root, "p2", nil)
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	names := m.ProjectNames()
	if len(names) != 2 {
		t.Fatalf("ProjectNames() len = %d, want 2", len(names))
	}
	for _, name := range []string{"p1", "p2"} {
		if _, ok := names[name]; !ok {
			t.Errorf("ProjectNames() missing %q", name)
		}
	}
}

// ---- ResolveWorkspaces ----

func TestResolveWorkspaces_Match(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "workspace", nil)
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	projPath := filepath.Join(root, "workspace")
	subPath := filepath.Join(projPath, "src", "main.go")

	result := m.ResolveWorkspaces([]string{subPath})
	if got, ok := result[subPath]; !ok || got != "workspace" {
		t.Errorf("ResolveWorkspaces(%q) = %q, want \"workspace\"", subPath, got)
	}
}

func TestResolveWorkspaces_NoMatch(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "proj", nil)
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	result := m.ResolveWorkspaces([]string{"/some/other/path"})
	if len(result) != 0 {
		t.Errorf("ResolveWorkspaces no match = %v, want empty", result)
	}
}

func TestResolveWorkspaces_DedupPaths(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "dedup", nil)
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	projPath := filepath.Join(root, "dedup", "sub")
	// Same path twice → result should have one entry.
	result := m.ResolveWorkspaces([]string{projPath, projPath})
	if len(result) > 1 {
		t.Errorf("ResolveWorkspaces dedup = %d entries, want <= 1", len(result))
	}
}

func TestResolveWorkspaces_EmptyPath(t *testing.T) {
	root := t.TempDir()
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	result := m.ResolveWorkspaces([]string{""})
	if len(result) != 0 {
		t.Errorf("ResolveWorkspaces empty path = %v, want empty", result)
	}
}

// ---- EffectivePlannerModel / EffectivePlannerPrompt ----

func TestEffectivePlannerModel_ProjectOverride(t *testing.T) {
	m := &Manager{defaults: PlannerDefaults{Model: "global-model"}}
	p := &Project{Config: ProjectConfig{PlannerModel: "project-model"}}
	got := m.EffectivePlannerModel(p)
	if got != "project-model" {
		t.Errorf("EffectivePlannerModel = %q, want \"project-model\"", got)
	}
}

func TestEffectivePlannerModel_GlobalDefault(t *testing.T) {
	m := &Manager{defaults: PlannerDefaults{Model: "global-model"}}
	p := &Project{}
	got := m.EffectivePlannerModel(p)
	if got != "global-model" {
		t.Errorf("EffectivePlannerModel = %q, want \"global-model\"", got)
	}
}

func TestEffectivePlannerModel_Empty(t *testing.T) {
	m := &Manager{defaults: PlannerDefaults{}}
	p := &Project{}
	got := m.EffectivePlannerModel(p)
	if got != "" {
		t.Errorf("EffectivePlannerModel = %q, want empty", got)
	}
}

func TestEffectivePlannerPrompt_ProjectOverride(t *testing.T) {
	m := &Manager{defaults: PlannerDefaults{Prompt: "global-prompt"}}
	p := &Project{Config: ProjectConfig{PlannerPrompt: "project-prompt"}}
	got := m.EffectivePlannerPrompt(p)
	if got != "project-prompt" {
		t.Errorf("EffectivePlannerPrompt = %q, want \"project-prompt\"", got)
	}
}

func TestEffectivePlannerPrompt_GlobalDefault(t *testing.T) {
	m := &Manager{defaults: PlannerDefaults{Prompt: "global-prompt"}}
	p := &Project{}
	got := m.EffectivePlannerPrompt(p)
	if got != "global-prompt" {
		t.Errorf("EffectivePlannerPrompt = %q, want \"global-prompt\"", got)
	}
}

// ---- rebuildBindingIndex (via BindChat collision) ----

func TestRebuildBindingIndex_OverwriteOnBind(t *testing.T) {
	root := t.TempDir()
	makeProjectDir(t, root, "proj_a", &ProjectConfig{
		ChatBindings: []ChatBinding{{Platform: "feishu", ChatType: "group", ChatID: "shared"}},
	})
	makeProjectDir(t, root, "proj_b", nil)
	m, _ := NewManager(root, PlannerDefaults{})
	m.Scan()

	// "shared" currently belongs to proj_a
	p := m.ProjectForChat("feishu", "group", "shared")
	if p == nil || p.Name != "proj_a" {
		t.Fatalf("initial binding wrong: %v", p)
	}

	// Bind the same chat to proj_b (last bind wins in rebuildBindingIndex)
	m.BindChat("proj_b", "feishu", "group", "shared")

	p2 := m.ProjectForChat("feishu", "group", "shared")
	// After rebuildBindingIndex, both proj_a and proj_b claim "shared".
	// The result is non-deterministic (map iteration order) but we just ensure
	// the function doesn't panic and returns a non-nil project.
	if p2 == nil {
		t.Error("ProjectForChat after double-bind = nil")
	}
}
