package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ---- PlannerKeyFor / PlannerSessionKey ----

func TestPlannerKeyFor(t *testing.T) {
	t.Parallel()
	got := PlannerKeyFor("myapp")
	want := "project:myapp:planner"
	if got != want {
		t.Errorf("PlannerKeyFor(\"myapp\") = %q, want %q", got, want)
	}
}

func TestPlannerSessionKey(t *testing.T) {
	t.Parallel()
	p := &Project{Name: "webapp"}
	got := p.PlannerSessionKey()
	want := "project:webapp:planner"
	if got != want {
		t.Errorf("PlannerSessionKey() = %q, want %q", got, want)
	}
}

// ---- snapshot / snapshotLight ----

func TestSnapshot_DeepCopy(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// A project.yaml may come in with a git pull, so it can name an access
// profile but not declare a credential source: one that tries is refused
// rather than read with the key silently dropped.
func TestLoadConfig_RejectsCredentialSources(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"aws_profile":     "access_profile: iso\naws_profile: admin\n",
		"access_profiles": "access_profiles:\n  iso:\n    aws_profile: admin\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			cfgDir := filepath.Join(dir, ".naozhi")
			if err := os.MkdirAll(cfgDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfgDir, "project.yaml"), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadConfig(dir); err == nil || !strings.Contains(err.Error(), "credential source") {
				t.Errorf("loadConfig = %v, want a credential-source refusal", err)
			}
		})
	}
	// Naming a profile stays fine.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".naozhi"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".naozhi", "project.yaml"), []byte("access_profile: iso\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := loadConfig(dir); err != nil || cfg.AccessProfile != "iso" {
		t.Errorf("loadConfig(access_profile only) = %+v, %v", cfg, err)
	}
}

// ---- saveConfigToPath ----

func TestSaveConfigToPath_RoundTrip(t *testing.T) {
	t.Parallel()
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

func TestSaveConfigToPath_CreatesDir(t *testing.T) {
	t.Parallel()
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

// ---- snapshotConfig ----

func TestSnapshotConfig_DeepCopy(t *testing.T) {
	t.Parallel()
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

// ---- DisplayName / Emoji (R110-P2 foundation) ----

func TestProjectConfig_LegacyYamlWithoutDisplayName(t *testing.T) {
	t.Parallel()
	legacy := []byte("git_sync: true\ngit_remote: origin\n")
	var cfg ProjectConfig
	if err := yaml.Unmarshal(legacy, &cfg); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if cfg.DisplayName != "" || cfg.Emoji != "" {
		t.Errorf("legacy yaml should leave DisplayName/Emoji empty, got %q/%q", cfg.DisplayName, cfg.Emoji)
	}
	if !cfg.GitSync || cfg.GitRemote != "origin" {
		t.Errorf("legacy fields corrupted: %+v", cfg)
	}
}

func TestProjectConfig_DisplayNameTooLong(t *testing.T) {
	t.Parallel()
	cfg := ProjectConfig{DisplayName: strings.Repeat("x", 129)}
	if err := ValidateConfig(cfg); err == nil {
		t.Error("expected 129-rune DisplayName to fail ValidateConfig")
	}
}

func TestProjectConfig_EmojiTooLong(t *testing.T) {
	t.Parallel()
	cfg := ProjectConfig{Emoji: strings.Repeat("a", 9)}
	if err := ValidateConfig(cfg); err == nil {
		t.Error("expected 9-rune Emoji to fail ValidateConfig")
	}
}

// ---- configPath ----

func TestConfigPath(t *testing.T) {
	t.Parallel()
	p := &Project{Path: "/home/user/projects/myapp"}
	got := p.configPath()
	want := "/home/user/projects/myapp/.naozhi/project.yaml"
	if got != want {
		t.Errorf("configPath() = %q, want %q", got, want)
	}
}
