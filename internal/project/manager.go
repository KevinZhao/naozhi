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
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/naozhi/naozhi/internal/osutil"
)

// ErrNotFound is returned when a project name does not exist in the manager.
var ErrNotFound = errors.New("project not found")

// Manager discovers and manages projects under a projects_root directory.
type Manager struct {
	root     string
	defaults PlannerDefaults

	// includeRoot also registers the root directory itself as a project (see
	// ProjectsConfig.IncludeRoot) so files directly under root resolve to an owner.
	includeRoot bool

	// exclude holds projects.exclude: basename globs Scan skips like hidden
	// directories. Fixed at construction.
	exclude []string

	// scanMu serialises Scan; it guards index and is never taken under mu.
	scanMu sync.Mutex

	// indexPath is where index persists ("" = in memory only), fixed at
	// construction. index holds the sidebar CreatedAt for projects whose
	// project.yaml carries none; guarded by scanMu.
	indexPath string
	index     *projectIndex

	// scanDiskHook, when set (tests only), runs at the end of every scanDisk.
	scanDiskHook func()

	mu       sync.RWMutex
	projects map[string]*Project // name -> project

	// mutGen is bumped under mu.Lock by every writer that changes a project's
	// config, so a Scan whose lock-free disk read overlapped one can tell.
	mutGen uint64

	// bindingIndex: "platform:chatType:chatID" -> project name (built from all ChatBindings)
	bindingIndex map[string]string

	// resolveCache memoises ResolveWorkspaces' inode-walk fallback (ws → project
	// name, "" = no match), consulted only after the byte-prefix fast path
	// misses; without it a case-insensitive FS re-Stats every ancestor per
	// dashboard poll. Valid only for the current m.projects: Scan clears it
	// under m.mu.Lock (the other writers never change Path).
	resolveCache sync.Map // ws string → string (project name; "" = no match)

	// resolveGen is bumped under m.mu.Lock whenever Scan swaps m.projects and
	// clears resolveCache; resolveWorkspaceByInode runs lock-free against a
	// snapshot and discards its Store when the generation moved (#2228).
	resolveGen atomic.Uint64
}

// projRef is the (name, Path) snapshot resolveWorkspaceByInode walks after
// m.mu is released (#2228).
type projRef struct {
	name string
	path string
}

// Option customises a Manager at construction time.
type Option func(*Manager)

// WithIncludeRoot registers the projects root directory itself as a project
// (in addition to its subdirectories) when enabled.
func WithIncludeRoot(enabled bool) Option {
	return func(m *Manager) { m.includeRoot = enabled }
}

// WithExclude makes Scan skip subdirectories whose basename matches any of
// patterns (filepath.Match globs, see ValidateExcludePattern). The include_root
// project is governed by WithIncludeRoot alone.
func WithExclude(patterns []string) Option {
	return func(m *Manager) { m.exclude = slices.Clone(patterns) }
}

// WithIndexPath persists naozhi's per-project bookkeeping (sidebar order) to
// path, normally datadir.Layout.ProjectsIndexPath. Without it the order is
// stable for the Manager's lifetime only.
func WithIndexPath(path string) Option {
	return func(m *Manager) { m.indexPath = path }
}

// NewManager creates a project manager for the given root directory.
func NewManager(root string, defaults PlannerDefaults, opts ...Option) (*Manager, error) {
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
	m := &Manager{
		root:         absRoot,
		defaults:     defaults,
		projects:     make(map[string]*Project),
		bindingIndex: make(map[string]string),
	}
	for _, opt := range opts {
		opt(m)
	}
	for _, pat := range m.exclude {
		if err := ValidateExcludePattern(pat); err != nil {
			return nil, fmt.Errorf("projects exclude: %w", err)
		}
	}
	m.index = loadProjectIndex(m.indexPath)
	return m, nil
}

// dirModTimeMillis returns the directory mtime in unix ms, preferring
// entry.Info() and falling back to os.Stat (entry is nil for the synthetic
// root). Returns 0 on error so the picker falls back to backend order.
func dirModTimeMillis(entry os.DirEntry, path string) int64 {
	if entry != nil {
		if info, err := entry.Info(); err == nil {
			return info.ModTime().UnixMilli()
		}
	}
	if info, err := os.Stat(path); err == nil {
		return info.ModTime().UnixMilli()
	}
	return 0
}

// Scan discovers all subdirectories under root and loads their project configs.
// It never writes into a project directory; only the projects index is saved
// (plus a one-shot removal of legacy stubs, see sweepLegacyStubs). The disk
// reads run without m.mu, so ProjectForChat and ResolveWorkspaces never wait
// on them; only the map swap takes the write lock. A writer that persisted
// meanwhile (mutGen moved) may postdate those reads, so Scan then re-reads
// under the lock: a swap never drops a binding, favorite or config edit.
func (m *Manager) Scan() error {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()

	m.mu.RLock()
	gen := m.mutGen
	m.mu.RUnlock()

	res, err := m.scanDisk()
	if err != nil {
		return err
	}

	m.mu.Lock()
	if m.mutGen != gen {
		if res, err = m.scanDisk(); err != nil {
			m.mu.Unlock()
			return err
		}
	}
	m.projects = res.projects
	m.rebuildBindingIndex()
	// Drop the inode-walk memo (keyed to the replaced project set) and bump the
	// generation under the same write lock, so no reader sees fresh projects
	// with a stale cache and no lock-free fallback keeps a stale Store (#2228).
	m.resolveCache.Clear()
	m.resolveGen.Add(1)
	m.mu.Unlock()

	m.index.replace(res.nextIndex)
	m.sweepLegacyStubs()

	slog.Info("scanned projects", "root", m.root, "count", len(res.projects))
	return nil
}

// scanResult is one scanDisk pass: the project set to publish and the
// CreatedAt index that goes with it.
type scanResult struct {
	projects  map[string]*Project
	nextIndex map[string]int64
}

// scanDisk reads root and every project's config, git remote and mtime, and
// resolves CreatedAt. It touches no m.mu-guarded state (m.index is scanMu's),
// so it is safe both lock-free and under m.mu.Lock.
func (m *Manager) scanDisk() (scanResult, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return scanResult{}, fmt.Errorf("scan projects root: %w", err)
	}

	projects := make(map[string]*Project, len(entries))
	// listed holds every directory not hidden or excluded, including ones
	// skipped for a bad config, so fixing a project.yaml keeps its place.
	listed := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || m.excluded(name) {
			continue
		}

		absPath := filepath.Join(m.root, name)
		listed = append(listed, absPath)

		cfg, err := loadConfig(absPath)
		if err != nil {
			slog.Warn("skip project with bad config", "name", name, "err", err)
			continue
		}
		// Defense-in-depth: a tampered project.yaml (git pull / direct edit) could
		// otherwise land invalid values in CLI argv or bindingIndex.
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
			DirModTime:   dirModTimeMillis(entry, absPath),
		}
	}

	// include_root: register the root itself so files directly under root
	// resolve to an owner. A real subdirectory with the same basename wins;
	// resolution is longest-prefix everywhere, so root only catches leftovers.
	if m.includeRoot {
		rootName := filepath.Base(m.root)
		if err := ValidateProjectName(rootName); err != nil {
			slog.Warn("include_root: root basename is not a valid project name; skipping root project",
				"root", m.root, "name", rootName, "err", err)
		} else if _, clash := projects[rootName]; clash {
			slog.Warn("include_root: a subdirectory already uses the root basename; skipping root project",
				"root", m.root, "name", rootName)
		} else {
			cfg, err := loadConfig(m.root)
			if err != nil {
				slog.Warn("include_root: skip root project with bad config", "root", m.root, "err", err)
			} else if err := ValidateConfig(cfg); err != nil {
				slog.Warn("include_root: skip root project with invalid config", "root", m.root, "err", err)
			} else {
				remote, isGH := DetectGitHubRemote(m.root)
				projects[rootName] = &Project{
					Name:         rootName,
					Path:         m.root,
					PathPrefix:   m.root + string(filepath.Separator),
					Config:       cfg,
					GitRemoteURL: remote,
					IsGitHub:     isGH,
					IsRoot:       true,
					DirModTime:   dirModTimeMillis(nil, m.root),
				}
			}
		}
	}

	// Index entries of other roots survive, so switching projects.root back
	// restores their order; entries for dirs gone from this root are dropped.
	nextIndex := make(map[string]int64, len(m.index.createdAt))
	for path, ms := range m.index.createdAt {
		if filepath.Dir(path) != m.root {
			nextIndex[path] = ms
		}
	}
	for _, path := range listed {
		if ms, ok := m.index.createdAt[path]; ok {
			nextIndex[path] = ms
		}
	}
	// Sidebar order: a non-zero created_at in project.yaml wins, then the
	// index, then a fresh stamp in name order with 1ms spacing so first-seen
	// projects keep byte-name order and All() sorts stay strict. The synthetic
	// root project is ordered below and never indexed.
	missing := make([]string, 0, len(projects))
	for name, p := range projects {
		switch {
		case p.IsRoot:
		case p.Config.CreatedAt != 0:
			nextIndex[p.Path] = p.Config.CreatedAt
		case nextIndex[p.Path] != 0:
			p.Config.CreatedAt = nextIndex[p.Path]
		default:
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		base := time.Now().UnixMilli()
		for i, name := range missing {
			p := projects[name]
			p.Config.CreatedAt = base + int64(i)
			nextIndex[p.Path] = p.Config.CreatedAt
		}
	}
	// Root project sorts strictly LAST: in-memory-only CreatedAt = max + 1,
	// recomputed every boot. Override unconditionally so a real project.yaml
	// dropped at the workspace root cannot place root mid-list.
	for _, p := range projects {
		if p.IsRoot {
			var maxCreated int64
			for _, q := range projects {
				if q.IsRoot {
					continue
				}
				if q.Config.CreatedAt > maxCreated {
					maxCreated = q.Config.CreatedAt
				}
			}
			p.Config.CreatedAt = maxCreated + 1
		}
	}

	if m.scanDiskHook != nil {
		m.scanDiskHook()
	}
	return scanResult{projects: projects, nextIndex: nextIndex}, nil
}

// excluded reports whether name matches a projects.exclude pattern. NewManager
// validated the patterns; a Match error means ValidateExcludePattern missed one.
func (m *Manager) excluded(name string) bool {
	for _, pat := range m.exclude {
		ok, err := filepath.Match(pat, name)
		if err != nil {
			slog.Warn("projects.exclude pattern is malformed", "pattern", pat, "err", err)
		}
		if ok {
			return true
		}
	}
	return false
}

// Get returns a snapshot (copy) of the project by name, or nil if not found.
func (m *Manager) Get(name string) *Project {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p := m.projects[name]
	if p == nil {
		return nil
	}
	return p.snapshot()
}

// All returns snapshots of all projects sorted by CreatedAt ascending (name as
// tiebreaker) so the sidebar renders newly-added folders at the bottom.
func (m *Manager) All() []*Project {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*Project, 0, len(m.projects))
	for _, p := range m.projects {
		result = append(result, p.snapshot())
	}
	slices.SortFunc(result, func(a, b *Project) int {
		if c := cmp.Compare(a.Config.CreatedAt, b.Config.CreatedAt); c != 0 {
			return c
		}
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
	// IM /project <name> is a trust boundary into bindingIndex: enforce the same
	// field invariants as ValidateConfig so the key invariant holds on every ingress.
	if platform == "" || chatType == "" || chatID == "" {
		return fmt.Errorf("%w: BindChat requires non-empty platform/chatType/chatID", ErrInvalidConfig)
	}
	if err := validateBindingField(platform, chatType, chatID); err != nil {
		return fmt.Errorf("%w: BindChat: %s", ErrInvalidConfig, err.Error())
	}
	// Hold the write lock across saveConfigToPath: Scan swaps m.projects under
	// the same lock, re-reading disk when mutGen moved, so a save after Unlock
	// could miss that re-read and let Scan clobber the in-memory binding.
	// saveConfigToPath never re-enters m.mu.
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.projects[projectName]
	if !ok {
		// %q: defense-in-depth so bidi/C1/newline bytes in projectName cannot
		// forge structured log entries via err.Error().
		return fmt.Errorf("%w: %q", ErrNotFound, projectName)
	}

	binding := ChatBinding{Platform: platform, ChatID: chatID, ChatType: chatType}

	for _, b := range p.Config.ChatBindings {
		if b.Platform == platform && b.ChatID == chatID && b.ChatType == chatType {
			return nil // already bound
		}
	}

	p.Config.ChatBindings = append(p.Config.ChatBindings, binding)
	m.mutGen++
	m.rebuildBindingIndex()
	return saveConfigToPath(p.configPath(), snapshotConfig(p))
}

// UnbindAllChat removes all bindings for a given chat across all projects.
// A chat can be bound to several projects (BindChat allows cross-project
// duplicates), so the single-value bindingIndex is not a reliable enumeration:
// every project's ChatBindings is scanned; only modified ones persist (#1961).
func (m *Manager) UnbindAllChat(platform, chatType, chatID string) error {
	// Persist under the lock so a concurrent Scan() cannot resurrect the
	// bindings just stripped (see BindChat).
	m.mu.Lock()
	defer m.mu.Unlock()

	type pendingSave struct {
		path string
		cfg  ProjectConfig
	}
	var saves []pendingSave
	changed := false
	for _, p := range m.projects {
		filtered := p.Config.ChatBindings[:0]
		removed := false
		for _, b := range p.Config.ChatBindings {
			if b.Platform != platform || b.ChatID != chatID || b.ChatType != chatType {
				filtered = append(filtered, b)
			} else {
				removed = true
			}
		}
		if !removed {
			continue
		}
		p.Config.ChatBindings = filtered
		changed = true
		saves = append(saves, pendingSave{path: p.configPath(), cfg: snapshotConfig(p)})
	}
	if changed {
		m.mutGen++
		m.rebuildBindingIndex()
	}

	var firstErr error
	for _, s := range saves {
		if err := saveConfigToPath(s.path, s.cfg); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SetFavorite sets a project's Favorite flag and persists it; other fields are untouched.
func (m *Manager) SetFavorite(name string, favorite bool) error {
	// Persist under the lock so a concurrent Scan() cannot drop this Favorite
	// flip (see BindChat).
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.projects[name]
	if !ok {
		// %q: defense-in-depth against log forging via err.Error().
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if p.Config.Favorite == favorite {
		return nil
	}
	p.Config.Favorite = favorite
	m.mutGen++
	return saveConfigToPath(p.configPath(), snapshotConfig(p))
}

// UpdateConfig updates a project's config and persists it.
func (m *Manager) UpdateConfig(name string, cfg ProjectConfig) error {
	// Persist under the lock so a concurrent Scan() cannot drop this update
	// (see BindChat).
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.projects[name]
	if !ok {
		// name comes from reverse-RPC frames and dashboard query strings; %q
		// escapes bidi/C1/newline so the error cannot forge log entries.
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	// created_at is sidebar order: a client that omits it means "unchanged",
	// not "re-stamp at the bottom on the next Scan".
	if cfg.CreatedAt == 0 {
		cfg.CreatedAt = p.Config.CreatedAt
	}
	p.Config = cfg
	m.mutGen++
	m.rebuildBindingIndex()
	return saveConfigToPath(p.configPath(), snapshotConfig(p))
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

// ResolveWorkspaces maps workspace paths to project names in a single lock
// acquisition; unmatched paths are omitted. Longest-prefix: a byte-wise
// strings.HasPrefix first, then an inode-aware containment walk so a
// case-insensitive FS resolves when root and recorded cwd differ only in case.
func (m *Manager) ResolveWorkspaces(paths []string) map[string]string {
	// The byte-prefix fast path runs under the RLock; the inode fallback Stats
	// every ancestor and must NOT hold the lock (it would stall every writer),
	// so it runs after RUnlock against a (name, Path) snapshot (#2228).
	result := make(map[string]string, len(paths))
	seen := make(map[string]struct{}, len(paths))
	var fallbacks []string // ws paths that missed the byte prefix
	var projSnap []projRef

	m.mu.RLock()
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
			continue
		}
		// Byte prefix missed: defer the inode probe (Stats the FS) past the lock.
		fallbacks = append(fallbacks, ws)
	}
	var snapGen uint64
	if len(fallbacks) > 0 {
		projSnap = make([]projRef, 0, len(m.projects))
		for _, p := range m.projects {
			projSnap = append(projSnap, projRef{name: p.Name, path: p.Path})
		}
		// Same RLock as the snapshot, so a later Scan is detectable.
		snapGen = m.resolveGen.Load()
	}
	m.mu.RUnlock()

	// Inode fallback runs lock-free against the snapshot (cached; Stats the FS).
	for _, ws := range fallbacks {
		if name := m.resolveWorkspaceByInode(ws, projSnap, snapGen); name != "" {
			result[ws] = name
		}
	}
	return result
}

// resolveWorkspaceByInode is the inode-aware longest-prefix fallback for
// ResolveWorkspaces, reached only when the byte-wise prefix scan misses. It
// asks osutil.PathContainedInRoot (os.SameFile ancestor walk, honouring the
// kernel's case folding) for each snapshotted project and keeps the deepest
// match. Called WITHOUT m.mu held because the walk Stats the FS. Results
// (including "" = no match) are memoised in resolveCache since the dashboard
// re-resolves the same workspaces at 1 Hz; if resolveGen moved past snapGen
// meanwhile, the just-stored value is stale and is deleted again (#2228).
func (m *Manager) resolveWorkspaceByInode(ws string, projects []projRef, snapGen uint64) string {
	if v, ok := m.resolveCache.Load(ws); ok {
		return v.(string)
	}
	var bestName string
	var bestLen int
	for _, p := range projects {
		if len(p.path) <= bestLen {
			continue // a longer match already won; SameFile walk is the cost we skip
		}
		if osutil.PathContainedInRoot(ws, p.path) {
			bestName = p.name
			bestLen = len(p.path)
		}
	}
	m.resolveCache.Store(ws, bestName)
	// A Scan replaced the project set while we walked lock-free: the stored
	// value is stale and its Clear() may already have run, so drop it.
	if m.resolveGen.Load() != snapGen {
		m.resolveCache.Delete(ws)
	}
	return bestName
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
// Prompt 最终拼进 argv 的 `--append-system-prompt <prompt>` 传给 CLI 子进程，且来自
// 磁盘 CLAUDE.md（Claude tool 可写），必须在源头拦截 NUL / C0 控制字节 + 非法 UTF-8，
// 防止 argv 截断或 shim stream-json 受污染。非法 prompt 返回空串而非部分字符，避免静默截断。
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
		// tab / LF / CR 是 markdown 合法内容，放行；其余 C0 + NUL + DEL 会破坏 argv 或
		// stream-json，整串丢弃。与 project.ValidateConfig / config.validatePlannerPrompt 对齐。
		if c == 0 || (c < 0x20 && c != 0x09 && c != 0x0a && c != 0x0d) || c == 0x7f {
			slog.Warn("planner prompt contains control byte; dropping",
				"project", p.Name, "byte", c)
			return ""
		}
	}
	// 字节循环只覆盖 NUL/C0/DEL，跳不到 C1/bidi override/LS-PS（多字节 UTF-8）。
	// ValidateConfig 已在写路径扫过，但被篡改的 project.yaml 或全局默认仍可能落到此处，
	// 运行时再扫一遍是 defense-in-depth；rune 用 U+XXXX 格式化避免 bidi 字面值翻转日志渲染。
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
