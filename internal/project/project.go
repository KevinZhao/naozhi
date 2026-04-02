package project

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Project represents a workspace folder discovered under projects_root.
type Project struct {
	Name       string        // directory name (unique ID)
	Path       string        // absolute filesystem path
	PathPrefix string        // Path + "/" — precomputed for ResolveWorkspaces prefix matching
	Config     ProjectConfig // loaded from .naozhi/project.yaml
}

// ProjectConfig is persisted to .naozhi/project.yaml inside each project directory.
type ProjectConfig struct {
	GitSync    bool   `yaml:"git_sync,omitempty" json:"git_sync"`
	GitRemote  string `yaml:"git_remote,omitempty" json:"git_remote,omitempty"`
	MemoryFile string `yaml:"memory_file,omitempty" json:"memory_file,omitempty"`

	PlannerModel  string `yaml:"planner_model,omitempty" json:"planner_model,omitempty"`
	PlannerPrompt string `yaml:"planner_prompt,omitempty" json:"planner_prompt,omitempty"`

	ChatBindings []ChatBinding `yaml:"chat_bindings,omitempty" json:"chat_bindings,omitempty"`
}

// ChatBinding links an IM chat to this project's planner.
type ChatBinding struct {
	Platform string `yaml:"platform" json:"platform"`
	ChatID   string `yaml:"chat_id" json:"chat_id"`
	ChatType string `yaml:"chat_type,omitempty" json:"chat_type,omitempty"`
}

// PlannerDefaults holds global defaults for planner sessions, overridable per-project.
type PlannerDefaults struct {
	Model  string `yaml:"model,omitempty"`
	Prompt string `yaml:"prompt,omitempty"`
}

// PlannerSessionKey returns the session key for this project's planner.
func (p *Project) PlannerSessionKey() string {
	return PlannerKeyFor(p.Name)
}

// PlannerKeyFor returns the planner session key for the given project name.
func PlannerKeyFor(name string) string {
	return "project:" + name + ":planner"
}

// snapshot returns a deep copy of the project for safe use outside the manager lock.
func (p *Project) snapshot() *Project {
	cp := *p
	if len(p.Config.ChatBindings) > 0 {
		cp.Config.ChatBindings = make([]ChatBinding, len(p.Config.ChatBindings))
		copy(cp.Config.ChatBindings, p.Config.ChatBindings)
	}
	return &cp
}

// snapshotLight returns a shallow copy without deep-copying ChatBindings.
// Use when the caller only reads Name/Path/PlannerModel/PlannerPrompt.
func (p *Project) snapshotLight() *Project {
	cp := *p
	cp.Config.ChatBindings = nil
	return &cp
}

// IsPlannerKey returns true if the session key is a project planner key.
func IsPlannerKey(key string) bool {
	// Format: "project:{name}:planner"
	if len(key) < len("project::planner") {
		return false
	}
	return key[:8] == "project:" && key[len(key)-8:] == ":planner"
}

const configDir = ".naozhi"
const configFile = "project.yaml"

// configPath returns the path to .naozhi/project.yaml for this project.
func (p *Project) configPath() string {
	return filepath.Join(p.Path, configDir, configFile)
}

// loadConfig reads .naozhi/project.yaml. Returns zero-value config if file doesn't exist.
func loadConfig(projectPath string) (ProjectConfig, error) {
	var cfg ProjectConfig
	path := filepath.Join(projectPath, configDir, configFile)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read project config: %w", err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse project config %s: %w", path, err)
	}
	return cfg, nil
}

// SaveConfig writes the project config to .naozhi/project.yaml.
func (p *Project) SaveConfig() error {
	return saveConfigToPath(p.configPath(), p.Config)
}

// saveConfigToPath atomically writes a ProjectConfig to the given path.
func saveConfigToPath(path string, cfg ProjectConfig) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write project config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) // M4: clean up on rename failure
		return fmt.Errorf("rename project config: %w", err)
	}
	return nil
}

// snapshotConfig returns a deep copy of a project's config (slice headers included).
func snapshotConfig(p *Project) ProjectConfig {
	cfg := p.Config
	if len(cfg.ChatBindings) > 0 {
		cfg.ChatBindings = make([]ChatBinding, len(p.Config.ChatBindings))
		copy(cfg.ChatBindings, p.Config.ChatBindings)
	}
	return cfg
}
