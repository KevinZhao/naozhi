package project

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// ---- IsPlannerKey ----

func TestIsPlannerKey_Valid(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		{"project:myapp:planner", true},
		{"project:x:planner", true},
		{"project:some-long-project-name:planner", true},
	}
	for _, tt := range tests {
		if got := IsPlannerKey(tt.key); got != tt.want {
			t.Errorf("IsPlannerKey(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
}

func TestIsPlannerKey_Invalid(t *testing.T) {
	tests := []string{
		"project::planner",           // name too short (exact sentinel)
		"project:planner",            // missing name segment
		"feishu:foo:planner",         // wrong prefix
		"project:foo:general",        // wrong suffix
		"",                           // empty
		"planner",                    // no prefix
		"project:foo:planner:extra",  // extra segment still ends with ":extra" not ":planner"
	}
	for _, key := range tests {
		if IsPlannerKey(key) {
			t.Errorf("IsPlannerKey(%q) = true, want false", key)
		}
	}
}

func TestIsPlannerKey_ExactBoundary(t *testing.T) {
	// "project::planner" has len == len("project::planner") which equals the sentinel
	boundary := "project::planner"
	if IsPlannerKey(boundary) {
		t.Errorf("IsPlannerKey(%q) = true, want false (empty name is invalid)", boundary)
	}
}

// ---- PlannerKeyFor / PlannerSessionKey ----

func TestPlannerKeyFor(t *testing.T) {
	got := PlannerKeyFor("myapp")
	want := "project:myapp:planner"
	if got != want {
		t.Errorf("PlannerKeyFor(\"myapp\") = %q, want %q", got, want)
	}
}

func TestPlannerSessionKey(t *testing.T) {
	p := &Project{Name: "webapp"}
	got := p.PlannerSessionKey()
	want := "project:webapp:planner"
	if got != want {
		t.Errorf("PlannerSessionKey() = %q, want %q", got, want)
	}
}

// ---- snapshot / snapshotLight ----

func TestSnapshot_DeepCopy(t *testing.T) {
	orig := &Project{
		Name: "proj",
		Path: "/tmp/proj",
		Config: ProjectConfig{
			ChatBindings: []ChatBinding{
				{Platform: "feishu", ChatID: "c1", ChatType: "group"},
			},
		},
	}
	snap := orig.snapshot()
	// Mutate snap's bindings — original must not change.
	snap.Config.ChatBindings[0].ChatID = "mutated"
	if orig.Config.ChatBindings[0].ChatID == "mutated" {
		t.Error("snapshot() is not a deep copy: mutation propagated to original")
	}
}

func TestSnapshotLight_NilBindings(t *testing.T) {
	orig := &Project{
		Name: "proj",
		Config: ProjectConfig{
			ChatBindings: []ChatBinding{
				{Platform: "feishu", ChatID: "c1"},
			},
		},
	}
	light := orig.snapshotLight()
	if light.Config.ChatBindings != nil {
		t.Error("snapshotLight() should nil ChatBindings")
	}
	// Ensure original is untouched
	if len(orig.Config.ChatBindings) != 1 {
		t.Error("snapshotLight() modified original ChatBindings")
	}
}

// ---- loadConfig ----

func TestLoadConfig_NotExist(t *testing.T) {
	dir := t.TempDir()
	cfg, err := loadConfig(dir) // no .naozhi/project.yaml
	if err != nil {
		t.Fatalf("loadConfig with missing file = %v, want nil", err)
	}
	// Should return zero-value config
	if cfg.GitSync || cfg.GitRemote != "" || len(cfg.ChatBindings) != 0 {
		t.Errorf("loadConfig missing file returned non-zero config: %+v", cfg)
	}
}

func TestLoadConfig_Valid(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".naozhi")
	if err := os.MkdirAll(cfgDir, 0700); err != nil {
		t.Fatal(err)
	}
	content := `
git_sync: true
git_remote: "origin"
chat_bindings:
  - platform: feishu
    chat_id: "c1"
    chat_type: group
`
	if err := os.WriteFile(filepath.Join(cfgDir, "project.yaml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(dir)
	if err != nil {
		t.Fatalf("loadConfig = %v", err)
	}
	if !cfg.GitSync {
		t.Error("GitSync should be true")
	}
	if cfg.GitRemote != "origin" {
		t.Errorf("GitRemote = %q, want \"origin\"", cfg.GitRemote)
	}
	if len(cfg.ChatBindings) != 1 || cfg.ChatBindings[0].ChatID != "c1" {
		t.Errorf("ChatBindings = %+v", cfg.ChatBindings)
	}
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".naozhi")
	if err := os.MkdirAll(cfgDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "project.yaml"), []byte("git_sync: [unclosed"), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := loadConfig(dir)
	if err == nil {
		t.Error("loadConfig with invalid YAML should return error")
	}
}

// ---- saveConfigToPath ----

func TestSaveConfigToPath_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".naozhi", "project.yaml")

	cfg := ProjectConfig{
		GitSync:   true,
		GitRemote: "upstream",
		ChatBindings: []ChatBinding{
			{Platform: "feishu", ChatID: "chat1", ChatType: "group"},
			{Platform: "weixin", ChatID: "chat2", ChatType: "direct"},
		},
	}

	if err := saveConfigToPath(path, cfg); err != nil {
		t.Fatalf("saveConfigToPath = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after save: %v", err)
	}

	var got ProjectConfig
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal saved YAML: %v", err)
	}

	if !got.GitSync {
		t.Error("GitSync should be true after round-trip")
	}
	if got.GitRemote != "upstream" {
		t.Errorf("GitRemote = %q, want \"upstream\"", got.GitRemote)
	}
	if len(got.ChatBindings) != 2 {
		t.Errorf("ChatBindings len = %d, want 2", len(got.ChatBindings))
	}
}

func TestSaveConfigToPath_AtomicWrite(t *testing.T) {
	// Verify the tmp file does not survive after a successful save.
	dir := t.TempDir()
	path := filepath.Join(dir, ".naozhi", "project.yaml")

	cfg := ProjectConfig{GitSync: false}
	if err := saveConfigToPath(path, cfg); err != nil {
		t.Fatalf("saveConfigToPath = %v", err)
	}

	tmpPath := path + ".tmp"
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("tmp file %q should not exist after successful save", tmpPath)
	}

	// The final path must exist.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("saved file %q does not exist: %v", path, err)
	}
}

func TestSaveConfigToPath_CreatesDir(t *testing.T) {
	dir := t.TempDir()
	// Deeply nested path that doesn't exist yet
	path := filepath.Join(dir, "a", "b", "c", "project.yaml")
	if err := saveConfigToPath(path, ProjectConfig{}); err != nil {
		t.Fatalf("saveConfigToPath nested dir = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created at %q: %v", path, err)
	}
}

func TestSaveConfigToPath_OverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project.yaml")

	// Write first version
	cfg1 := ProjectConfig{GitRemote: "first"}
	if err := saveConfigToPath(path, cfg1); err != nil {
		t.Fatalf("first save: %v", err)
	}

	// Overwrite
	cfg2 := ProjectConfig{GitRemote: "second"}
	if err := saveConfigToPath(path, cfg2); err != nil {
		t.Fatalf("second save: %v", err)
	}

	data, _ := os.ReadFile(path)
	var got ProjectConfig
	yaml.Unmarshal(data, &got) //nolint:errcheck
	if got.GitRemote != "second" {
		t.Errorf("after overwrite GitRemote = %q, want \"second\"", got.GitRemote)
	}
}

// ---- snapshotConfig ----

func TestSnapshotConfig_DeepCopy(t *testing.T) {
	p := &Project{
		Config: ProjectConfig{
			ChatBindings: []ChatBinding{
				{Platform: "feishu", ChatID: "c1"},
			},
		},
	}
	snap := snapshotConfig(p)
	snap.ChatBindings[0].ChatID = "mutated"
	if p.Config.ChatBindings[0].ChatID == "mutated" {
		t.Error("snapshotConfig is not a deep copy")
	}
}

func TestSnapshotConfig_EmptyBindings(t *testing.T) {
	p := &Project{Config: ProjectConfig{GitSync: true}}
	snap := snapshotConfig(p)
	if snap.ChatBindings != nil {
		t.Error("snapshotConfig should preserve nil ChatBindings")
	}
}

// ---- configPath ----

func TestConfigPath(t *testing.T) {
	p := &Project{Path: "/home/user/projects/myapp"}
	got := p.configPath()
	want := "/home/user/projects/myapp/.naozhi/project.yaml"
	if got != want {
		t.Errorf("configPath() = %q, want %q", got, want)
	}
}
