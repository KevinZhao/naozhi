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
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
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

		cfg, err := loadConfig(absPath)
		if err != nil {
			slog.Warn("skip project with bad config", "name", name, "err", err)
			continue
		}
		// Defense-in-depth: the write-path (HTTP PUT / reverse-RPC
		// update_config) already runs ValidateConfig, but a tampered
		// project.yaml committed via git pull or a direct filesystem
		// edit could land invalid values that flow into CLI argv
		// (PlannerPrompt/PlannerModel) or bindingIndex (ChatBindings
		// with ':' / NUL) if load accepted them silently. Skip the
		// project same as a parse failure.
		if err := ValidateConfig(cfg); err != nil {
			slog.Warn("skip project with invalid config", "name", name, "err", err)
			continue
		}

		remote, isGH := DetectGitHubRemote(absPath)
		projects[name] = &Project{
			Name:         name,
			Path:         absPath,
			PathPrefix:   absPath + "/",
			Config:       cfg,
			GitRemoteURL: remote,
			IsGitHub:     isGH,
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
	// R185-SEC-M1: IM /project <name> → BindChat is another trust boundary
	// into bindingIndex. Enforce the same field invariants as ValidateConfig
	// (R184-SEC-M1b) so the index's "platform:chatType:chatID" key invariant
	// is upheld regardless of ingress. Empty required fields are also rejected
	// (R185-SEC-M2).
	if platform == "" || chatType == "" || chatID == "" {
		return fmt.Errorf("%w: BindChat requires non-empty platform/chatType/chatID", ErrInvalidConfig)
	}
	if err := validateBindingField(platform, chatType, chatID); err != nil {
		return fmt.Errorf("%w: BindChat: %s", ErrInvalidConfig, err.Error())
	}
	m.mu.Lock()
	p, ok := m.projects[projectName]
	if !ok {
		m.mu.Unlock()
		// R183-SEC-L1: use %q to mirror UpdateConfig / SetFavorite (lines 211,
		// 237). An exported function is one future caller away from being
		// reached without a trust-boundary ValidateProjectName; defense-in-
		// depth quoting means bidi/C1/newline bytes in projectName cannot
		// forge structured log entries via err.Error().
		return fmt.Errorf("%w: %q", ErrNotFound, projectName)
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

// SetFavorite toggles a project's Favorite flag and persists it atomically.
// Only touches Favorite — other config fields are preserved.
func (m *Manager) SetFavorite(name string, favorite bool) error {
	m.mu.Lock()
	p, ok := m.projects[name]
	if !ok {
		m.mu.Unlock()
		// R182-SEC-L1: %q mirrors UpdateConfig (line 234). set_favorite now
		// validates at the RPC boundary (R182-SEC-M1), but function is
		// defense-in-depth for any future caller that forgets to validate.
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if p.Config.Favorite == favorite {
		m.mu.Unlock()
		return nil
	}
	p.Config.Favorite = favorite
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
		// R181-SEC-P2-2: name comes from reverse-RPC frames (update_config)
		// and dashboard query strings. %q escapes bidi/C1/newline so the
		// wrapped error cannot forge structured log entries when the caller
		// logs err.Error(). BindChat / UpdateBinding / SetFavorite now all
		// use %q too (R182/R183/R185) so every ErrNotFound path is uniform.
		return fmt.Errorf("%w: %q", ErrNotFound, name)
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
//
// Prompt 最终会被拼成 argv 里的 `--append-system-prompt <prompt>` 传给 CLI 子进程，
// prompt 字符串来自磁盘 CLAUDE.md（Claude tool 可写），必须在源头拦截 NUL / C0
// 控制字节 + 非法 UTF-8，防止 argv 截断或 shim stream-json 编码受污染。
// 非法 prompt 返回空串（等价于没有配置 planner prompt），而不是返回部分字符，
// 避免"静默截断"产生难以追踪的 planner 行为漂移。
func (m *Manager) EffectivePlannerPrompt(p *Project) string {
	raw := p.Config.PlannerPrompt
	if raw == "" {
		raw = m.defaults.Prompt
	}
	if raw == "" {
		return ""
	}
	if !utf8.ValidString(raw) {
		slog.Warn("planner prompt contains invalid UTF-8; dropping", "project", p.Name)
		return ""
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		// 0x09 tab / 0x0A LF / 0x0D CR 是 markdown/CLAUDE.md 合法内容，放行；
		// 其余 C0 控制字节 + NUL + DEL 会破坏 argv 或 stream-json，整串丢弃。
		// 与 project.ValidateConfig / config.validatePlannerPrompt 对齐 DEL 处理。
		if c == 0 || (c < 0x20 && c != 0x09 && c != 0x0a && c != 0x0d) || c == 0x7f {
			slog.Warn("planner prompt contains control byte; dropping",
				"project", p.Name, "byte", c)
			return ""
		}
	}
	// R225-SEC-6: 字节循环只覆盖 NUL/C0/DEL，跳不到 C1/bidi override/LS-PS（多字节
	// UTF-8，首字节 >= 0xC2）。ValidateConfig 已在写路径上跑同一份 IsLogInjectionRune
	// 扫描，但被篡改的 project.yaml 或绕开 ValidateConfig 的全局默认仍可能落到此处 —
	// 运行时再扫一遍是 defense-in-depth；rune 用 U+XXXX 格式化避免 bidi 字面值翻转
	// journalctl 渲染。
	for _, r := range raw {
		if osutil.IsLogInjectionRune(r) {
			slog.Warn("planner prompt contains injection rune (C1/bidi/LS-PS); dropping",
				"project", p.Name, "rune", fmt.Sprintf("U+%04X", r))
			return ""
		}
	}
	return raw
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
