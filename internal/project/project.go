package project

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/naozhi/naozhi/internal/osutil"
	"github.com/naozhi/naozhi/internal/sessionkey"
	"gopkg.in/yaml.v3"
)

// Project represents a workspace folder discovered under projects_root.
type Project struct {
	Name       string        `json:"name"`                  // directory name (unique ID)
	Path       string        `json:"path"`                  // absolute filesystem path
	PathPrefix string        `json:"path_prefix,omitempty"` // Path + "/" — precomputed for ResolveWorkspaces prefix matching
	Config     ProjectConfig `json:"config"`                // loaded from .naozhi/project.yaml

	// Runtime-derived fields (not persisted, refreshed on Scan).
	GitRemoteURL string `json:"git_remote_url,omitempty"`
	IsGitHub     bool   `json:"is_github,omitempty"`

	// DirModTime is the directory's mtime at Scan time (unix ms); the "new
	// session" folder picker orders its fallback tier by it descending.
	// Not persisted; 0 when the directory could not be stat'd.
	DirModTime int64 `json:"dir_mtime,omitempty"`

	// IsRoot marks the synthetic project for the projects root itself
	// (ProjectsConfig.IncludeRoot). Its Path is the whole workspace tree, so
	// the file endpoints apply the __public_tmp__ pseudo-project gates and
	// Scan never persists a CreatedAt for it.
	IsRoot bool `json:"is_root,omitempty"`
}

// ProjectConfig is persisted to .naozhi/project.yaml inside each project directory.
type ProjectConfig struct {
	Favorite   bool   `yaml:"favorite,omitempty" json:"favorite,omitempty"`
	GitSync    bool   `yaml:"git_sync,omitempty" json:"git_sync"`
	GitRemote  string `yaml:"git_remote,omitempty" json:"git_remote,omitempty"`
	MemoryFile string `yaml:"memory_file,omitempty" json:"memory_file,omitempty"`

	// CreatedAt (unix ms) anchors sidebar order: ascending, so new folders land
	// at the bottom of their tier. Scan synthesises and persists it when missing.
	CreatedAt int64 `yaml:"created_at,omitempty" json:"created_at,omitempty"`

	PlannerModel  string `yaml:"planner_model,omitempty" json:"planner_model,omitempty"`
	PlannerPrompt string `yaml:"planner_prompt,omitempty" json:"planner_prompt,omitempty"`

	// Backend pins the default CLI backend for this project's sessions; empty =
	// router default. Referential validity is checked at the config layer;
	// ValidateConfig only enforces byte-hygiene.
	Backend string `yaml:"backend,omitempty" json:"backend,omitempty"`
	// AccessProfile names the default access profile for this project's
	// sessions. Only the NAME: env values live in the trusted config.yaml, so a
	// project.yaml synced from git can never inject env. Empty = global default.
	AccessProfile string `yaml:"access_profile,omitempty" json:"access_profile,omitempty"`

	// DisplayName overrides the directory name in dashboard rendering; empty
	// means "use directory name".
	DisplayName string `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	// Emoji is a single Unicode emoji (or short label prefix up to 8 runes)
	// rendered alongside DisplayName. Empty means "no prefix".
	Emoji string `yaml:"emoji,omitempty" json:"emoji,omitempty"`

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

// snapshotLight returns a shallow copy without ChatBindings, for read-only callers.
func (p *Project) snapshotLight() *Project {
	cp := *p
	cp.Config.ChatBindings = nil
	return &cp
}

// IsPlannerKey returns true if the session key is a project planner key
// ("project:{name}:planner"). Delegates to internal/sessionkey, the single
// source of truth (#1412).
func IsPlannerKey(key string) bool {
	return sessionkey.IsPlannerKey(key)
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
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read project config: %w", err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse project config %s: %w", path, err)
	}
	return cfg, nil
}

// saveConfigToPath atomically writes cfg to path (write-tmp → fsync → rename).
func saveConfigToPath(path string, cfg ProjectConfig) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}

	if err := osutil.WriteFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("save project config: %w", err)
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
