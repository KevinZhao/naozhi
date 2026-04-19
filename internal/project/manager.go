package project

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// ErrNotFound is returned when a project name does not exist in the manager.
var ErrNotFound = errors.New("project not found")

// Manager discovers and manages projects under a projects_root directory.
type Manager struct {
	root     string
	defaults PlannerDefaults

	mu       sync.RWMutex
	projects map[string]*Project // name -> project

	// bindingIndex: "platform:chatType:chatID" -> project name (built from all ChatBindings)
	bindingIndex map[string]string
}

// NewManager creates a project manager for the given root directory.
func NewManager(root string, defaults PlannerDefaults) (*Manager, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve projects root: %w", err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("projects root not found: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("projects root is not a directory: %s", absRoot)
	}
	return &Manager{
		root:         absRoot,
		defaults:     defaults,
		projects:     make(map[string]*Project),
		bindingIndex: make(map[string]string),
	}, nil
}

// Scan discovers all subdirectories under root and loads their project configs.
func (m *Manager) Scan() error {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return fmt.Errorf("scan projects root: %w", err)
	}

	projects := make(map[string]*Project, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Skip hidden directories
		if strings.HasPrefix(name, ".") {
			continue
		}

		absPath := filepath.Join(m.root, name)

		// Only include directories that contain CLAUDE.md
		if _, err := os.Stat(filepath.Join(absPath, "CLAUDE.md")); err != nil {
			continue
		}

		cfg, err := loadConfig(absPath)
		if err != nil {
			slog.Warn("skip project with bad config", "name", name, "err", err)
			continue
		}

		projects[name] = &Project{
			Name:       name,
			Path:       absPath,
			PathPrefix: absPath + "/",
			Config:     cfg,
		}
	}

	m.mu.Lock()
	m.projects = projects
	m.rebuildBindingIndex()
	m.mu.Unlock()

	slog.Info("scanned projects", "root", m.root, "count", len(projects))
	return nil
}

// Get returns a snapshot of the project by name, or nil if not found.
// The returned *Project is a copy; mutations do not affect the manager's state.
func (m *Manager) Get(name string) *Project {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p := m.projects[name]
	if p == nil {
		return nil
	}
	return p.snapshot()
}

// All returns snapshots of all projects sorted by name.
func (m *Manager) All() []*Project {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*Project, 0, len(m.projects))
	for _, p := range m.projects {
		result = append(result, p.snapshot())
	}
	slices.SortFunc(result, func(a, b *Project) int {
		return cmp.Compare(a.Name, b.Name)
	})
	return result
}

// ProjectForChat returns a snapshot of the project bound to the given chat, or nil.
func (m *Manager) ProjectForChat(platform, chatType, chatID string) *Project {
	m.mu.RLock()
	defer m.mu.RUnlock()

	key := platform + ":" + chatType + ":" + chatID
	if name, ok := m.bindingIndex[key]; ok {
		if p := m.projects[name]; p != nil {
			return p.snapshotLight()
		}
	}
	return nil
}

// BindChat binds a chat to a project and persists the binding to project.yaml.
func (m *Manager) BindChat(projectName, platform, chatType, chatID string) error {
	m.mu.Lock()
	p, ok := m.projects[projectName]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, projectName)
	}

	binding := ChatBinding{Platform: platform, ChatID: chatID, ChatType: chatType}

	// Check if already bound
	for _, b := range p.Config.ChatBindings {
		if b.Platform == platform && b.ChatID == chatID && b.ChatType == chatType {
			m.mu.Unlock()
			return nil // already bound
		}
	}

	p.Config.ChatBindings = append(p.Config.ChatBindings, binding)
	m.rebuildBindingIndex()
	cfgSnap := snapshotConfig(p)
	path := p.configPath()
	m.mu.Unlock()

	return saveConfigToPath(path, cfgSnap)
}

// UnbindAllChat removes all bindings for a given chat across all projects.
func (m *Manager) UnbindAllChat(platform, chatType, chatID string) error {
	m.mu.Lock()
	key := platform + ":" + chatType + ":" + chatID
	name, ok := m.bindingIndex[key]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	p := m.projects[name]
	if p == nil {
		m.mu.Unlock()
		return nil
	}

	filtered := p.Config.ChatBindings[:0]
	for _, b := range p.Config.ChatBindings {
		if b.Platform != platform || b.ChatID != chatID || b.ChatType != chatType {
			filtered = append(filtered, b)
		}
	}
	p.Config.ChatBindings = filtered
	m.rebuildBindingIndex()
	cfgSnap := snapshotConfig(p)
	path := p.configPath()
	m.mu.Unlock()

	return saveConfigToPath(path, cfgSnap)
}

// UpdateConfig updates a project's config and persists it.
func (m *Manager) UpdateConfig(name string, cfg ProjectConfig) error {
	m.mu.Lock()
	p, ok := m.projects[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	p.Config = cfg
	m.rebuildBindingIndex()
	cfgSnap := snapshotConfig(p)
	path := p.configPath()
	m.mu.Unlock()

	return saveConfigToPath(path, cfgSnap)
}

// ProjectNames returns the set of current project names.
func (m *Manager) ProjectNames() map[string]struct{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make(map[string]struct{}, len(m.projects))
	for name := range m.projects {
		names[name] = struct{}{}
	}
	return names
}

// ResolveWorkspaces maps workspace paths to project names in a single lock acquisition.
// Returns a map from workspace path to project name. Paths that don't match any project are omitted.
func (m *Manager) ResolveWorkspaces(paths []string) map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]string, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, ws := range paths {
		if ws == "" {
			continue
		}
		if _, ok := seen[ws]; ok {
			continue
		}
		seen[ws] = struct{}{}
		normalized := ws
		if normalized[len(normalized)-1] != '/' {
			normalized += "/"
		}
		var bestName string
		var bestLen int
		for _, p := range m.projects {
			if strings.HasPrefix(normalized, p.PathPrefix) {
				if len(p.Path) > bestLen {
					bestName = p.Name
					bestLen = len(p.Path)
				}
			}
		}
		if bestName != "" {
			result[ws] = bestName
		}
	}
	return result
}

// EffectivePlannerModel returns the model for the planner (project override > global default > "sonnet").
func (m *Manager) EffectivePlannerModel(p *Project) string {
	if p.Config.PlannerModel != "" {
		return p.Config.PlannerModel
	}
	if m.defaults.Model != "" {
		return m.defaults.Model
	}
	return ""
}

// EffectivePlannerPrompt returns the prompt for the planner (project override > global default > "").
func (m *Manager) EffectivePlannerPrompt(p *Project) string {
	if p.Config.PlannerPrompt != "" {
		return p.Config.PlannerPrompt
	}
	return m.defaults.Prompt
}

// rebuildBindingIndex rebuilds the chat -> project index from all project configs.
// Must be called under m.mu write lock.
func (m *Manager) rebuildBindingIndex() {
	m.bindingIndex = make(map[string]string)
	for _, p := range m.projects {
		for _, b := range p.Config.ChatBindings {
			key := b.Platform + ":" + b.ChatType + ":" + b.ChatID
			m.bindingIndex[key] = p.Name
		}
	}
}
