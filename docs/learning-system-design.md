# Naozhi 自学习系统设计

灵感来源：Hermes Agent（NousResearch/hermes-agent, 52K stars）的闭环学习系统。
在 naozhi 的网关架构上实现"用得越久越聪明"的自我进化能力。

## 1. 设计背景

### 1.1 Hermes 的做法

Hermes Agent 是一个 Python 实现的完整 Agent Loop，直接控制 LLM API 调用。它的自学习系统核心：

1. **学习触发**：每 10 次 tool call 后，fork 一个后台 review agent
2. **Review Agent**：带完整对话历史，独立运行最多 8 轮，评估对话中是否有值得保存的技能或记忆
3. **技能文件**：`~/.hermes/skills/{name}/SKILL.md`，带 YAML frontmatter，可创建/编辑/patch/删除
4. **三层记忆**：MEMORY.md（环境事实）+ USER.md（用户画像）+ FTS5 会话搜索
5. **Frozen Snapshot**：记忆在会话开始时加载一次，中途不修改系统提示（保护 prefix cache）

### 1.2 Naozhi 的架构约束

Naozhi 是**路由层**，不控制 Agent Loop——认知能力完全委托给 Claude CLI 子进程。这意味着：

- ✅ 可以看到所有对话 event stream（`process.EventEntries()`）
- ✅ 可以管理会话生命周期（spawn/resume/cleanup/reset）
- ✅ 可以通过 `--append-system-prompt` 注入上下文
- ✅ 可以 spawn 独立 CLI 进程做后台 review（类似 cron 的执行模式）
- ❌ 不能在 agent loop 内部注入 tool call 计数器
- ❌ 不能动态修改运行中会话的系统提示

**核心策略**：利用会话结束（idle timeout / `/new` / process exit）作为天然的学习触发点，比 Hermes 的"每 N 次 tool call"更自然、更不打断工作流。

## 2. 总体架构

```
~/.naozhi/
├── config.yaml              # 新增 learning 配置段
├── sessions.json
├── learning/                # 新增：学习系统数据目录
│   ├── MEMORY.md            # 全局环境记忆
│   ├── USER.md              # 用户画像
│   ├── skills/              # 技能库
│   │   ├── go-testing/
│   │   │   └── SKILL.md
│   │   └── feishu-bot-debug/
│   │       └── SKILL.md
│   ├── projects/            # 项目级记忆
│   │   └── {project-name}/
│   │       └── CONTEXT.md
│   ├── sessions.db          # SQLite FTS5 会话索引
│   └── stats.json           # 学习效果统计
```

```
┌──────────────────────────────────────────────────────────┐
│                     Naozhi Gateway                        │
│                                                          │
│  ┌──────────┐    ┌───────────┐    ┌──────────────────┐  │
│  │ Platform  │───>│  Session   │───>│   CLI Process    │  │
│  │ Adapters  │    │  Router    │    │  (Claude/Kiro)   │  │
│  └──────────┘    └─────┬─────┘    └────────┬─────────┘  │
│                        │                    │             │
│                        │ 会话结束时          │ event stream│
│                        v                    v             │
│                  ┌─────────────────────────────┐         │
│                  │   Learning Engine (新增)      │         │
│                  │                             │         │
│                  │  ┌───────────┐ ┌──────────┐ │         │
│                  │  │  Reviewer  │ │ Injector │ │         │
│                  │  │  后台提炼   │ │ 启动注入  │ │         │
│                  │  └─────┬─────┘ └────┬─────┘ │         │
│                  │        │            │        │         │
│                  │  ┌─────v────────────v─────┐  │         │
│                  │  │    Store Layer          │  │         │
│                  │  │  Memory / Skill / FTS   │  │         │
│                  │  └────────────────────────┘  │         │
│                  └─────────────────────────────┘         │
└──────────────────────────────────────────────────────────┘
```

## 3. Phase 1：对话提炼 + 技能系统

### 3.1 学习触发器

**触发时机**（从 Router 层拦截）：

| 事件 | 位置 | 条件 | 动作 |
|------|------|------|------|
| 会话 idle timeout | `Router.Cleanup()` | 会话有 ≥ 3 条 event entries | 触发 Review |
| 用户重置 | `Router.Reset()` | 同上 | 触发 Review |
| 进程退出 | process.Alive() → false | 同上 | 触发 Review |
| 手动触发 | `/learn` 命令 | 任何时候 | 触发当前会话 Review |

**最小事件阈值**：只有 event entries ≥ 3 条才触发 review，避免对简单问答浪费资源。

```go
// internal/learning/trigger.go

// ShouldReview decides if a session's conversation is worth reviewing.
// Only sessions with meaningful interaction (≥ minEvents entries,
// at least 1 tool_use) qualify. Cron and history sessions are excluded.
func ShouldReview(key string, entries []cli.EventEntry) bool {
    // Skip cron and history sessions
    if strings.HasPrefix(key, "cron:") || strings.HasPrefix(key, "local:history:") {
        return false
    }

    const minEvents = 3
    if len(entries) < minEvents {
        return false
    }

    // Require at least one tool_use event (indicates non-trivial interaction)
    for _, e := range entries {
        if e.Type == "tool_use" {
            return true
        }
    }
    return false
}
```

### 3.2 Review Agent

后台 spawn 一个独立 CLI 进程，带对话历史摘要和 review prompt。用轻量模型降成本。

```go
// internal/learning/reviewer.go

package learning

import (
    "context"
    "encoding/json"
    "log/slog"
    "time"

    "github.com/naozhi/naozhi/internal/cli"
)

// ReviewConfig holds configuration for the background review agent.
type ReviewConfig struct {
    Wrapper     *cli.Wrapper
    Model       string        // review 模型，建议 haiku 或 sonnet
    Timeout     time.Duration // 单次 review 超时
    SkillDir    string        // ~/.naozhi/learning/skills/
    MemoryDir   string        // ~/.naozhi/learning/
}

// Reviewer runs background conversation reviews to extract skills and memory.
type Reviewer struct {
    cfg     ReviewConfig
    pending chan reviewTask
    done    chan struct{}
}

type reviewTask struct {
    sessionKey string
    workspace  string
    entries    []cli.EventEntry
}

// NewReviewer creates a reviewer with a bounded work queue.
func NewReviewer(cfg ReviewConfig) *Reviewer {
    return &Reviewer{
        cfg:     cfg,
        pending: make(chan reviewTask, 20), // 最多缓冲 20 个 review 任务
        done:    make(chan struct{}),
    }
}

// Start begins the background review worker (single goroutine, sequential execution).
func (r *Reviewer) Start() {
    go r.loop()
}

// Stop drains the queue and shuts down.
func (r *Reviewer) Stop() {
    close(r.pending)
    <-r.done
}

// Submit queues a session for background review. Non-blocking; drops if queue full.
func (r *Reviewer) Submit(key, workspace string, entries []cli.EventEntry) {
    // Deep-copy entries to avoid data races with session cleanup
    copied := make([]cli.EventEntry, len(entries))
    copy(copied, entries)

    select {
    case r.pending <- reviewTask{sessionKey: key, workspace: workspace, entries: copied}:
        slog.Info("learning: review queued", "key", key, "entries", len(entries))
    default:
        slog.Warn("learning: review queue full, dropping", "key", key)
    }
}

func (r *Reviewer) loop() {
    defer close(r.done)
    for task := range r.pending {
        r.review(task)
    }
}
```

### 3.3 Review 执行逻辑

```go
func (r *Reviewer) review(task reviewTask) {
    log := slog.With("key", task.sessionKey)
    log.Info("learning: starting review")

    ctx, cancel := context.WithTimeout(context.Background(), r.cfg.Timeout)
    defer cancel()

    // 1. 将对话 events 格式化为可读文本
    transcript := formatTranscript(task.entries)
    if len(transcript) > 80000 { // ~30K tokens，防止超长对话
        transcript = transcript[:80000] + "\n\n[... truncated ...]"
    }

    // 2. 构造 review prompt
    prompt := buildReviewPrompt(transcript, task.workspace)

    // 3. Spawn 临时 CLI 进程
    proc, err := r.cfg.Wrapper.Spawn(ctx, cli.SpawnOptions{
        Key:        "learning:review:" + task.sessionKey,
        Model:      r.cfg.Model,
        WorkingDir: task.workspace,
        ExtraArgs: []string{
            "--append-system-prompt", reviewSystemPrompt,
        },
    })
    if err != nil {
        log.Error("learning: spawn review agent failed", "err", err)
        return
    }
    defer proc.Close()

    // 4. 发送 transcript + 指令
    result, err := proc.Send(ctx, prompt, nil, nil)
    if err != nil {
        log.Error("learning: review send failed", "err", err)
        return
    }

    // 5. 解析输出中的 skill/memory 指令
    actions := parseReviewOutput(result.Text)
    for _, action := range actions {
        switch action.Type {
        case "skill_create":
            if err := r.createSkill(action.Name, action.Content); err != nil {
                log.Warn("learning: skill create failed", "name", action.Name, "err", err)
            } else {
                log.Info("learning: skill created", "name", action.Name)
            }
        case "skill_update":
            if err := r.updateSkill(action.Name, action.Content); err != nil {
                log.Warn("learning: skill update failed", "name", action.Name, "err", err)
            }
        case "memory_add":
            if err := r.addMemory(action.Target, action.Content); err != nil {
                log.Warn("learning: memory add failed", "target", action.Target, "err", err)
            }
        }
    }

    log.Info("learning: review complete", "actions", len(actions))
}
```

### 3.4 Review System Prompt

```go
// internal/learning/prompts.go

const reviewSystemPrompt = `You are a learning agent. Your job is to review
completed conversations and extract reusable knowledge.

You MUST output structured JSON actions. Output ONLY a JSON array, no other text.

Available action types:

1. Create a new skill (reusable procedure for a specific task type):
   {"type": "skill_create", "name": "kebab-case-name", "content": "SKILL.md content"}

2. Update an existing skill with new learnings:
   {"type": "skill_update", "name": "existing-skill-name", "content": "full updated SKILL.md"}

3. Save to memory (environment facts, conventions, tool quirks):
   {"type": "memory_add", "target": "memory", "content": "fact to remember"}

4. Save user preference (communication style, workflow habits):
   {"type": "memory_add", "target": "user", "content": "preference to remember"}

Rules:
- Only create skills for NON-TRIVIAL approaches that required trial-and-error
  or where the user corrected the approach
- Skills must be specific and actionable, not generic advice
- Memory entries should be things not derivable from code or git history
- If nothing is worth saving, output an empty array: []
- Skill names must be lowercase kebab-case, max 64 chars
- Each SKILL.md must have YAML frontmatter with name and description`
```

```go
func buildReviewPrompt(transcript, workspace string) string {
    var sb strings.Builder
    sb.WriteString("## Conversation Transcript\n\n")
    sb.WriteString(transcript)
    sb.WriteString("\n\n## Workspace\n\n")
    sb.WriteString(workspace)
    sb.WriteString("\n\n## Existing Skills\n\n")

    // List existing skill names so the agent can update rather than duplicate
    skills := listSkillNames(/* skillDir */)
    if len(skills) > 0 {
        for _, s := range skills {
            sb.WriteString("- " + s + "\n")
        }
    } else {
        sb.WriteString("(none yet)\n")
    }

    sb.WriteString("\n## Instructions\n\n")
    sb.WriteString("Review the conversation above. Extract reusable skills and important memories.\n")
    sb.WriteString("Output a JSON array of actions. If nothing worth saving, output [].\n")
    return sb.String()
}
```

### 3.5 技能存储

```go
// internal/learning/skillstore.go

package learning

import (
    "fmt"
    "os"
    "path/filepath"
    "regexp"
    "strings"

    "gopkg.in/yaml.v3"
)

var validNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

const (
    maxNameLength        = 64
    maxDescriptionLength = 1024
    maxSkillContentChars = 100_000
)

// SkillMeta is the YAML frontmatter of a SKILL.md file.
type SkillMeta struct {
    Name        string   `yaml:"name"`
    Description string   `yaml:"description"`
    Version     string   `yaml:"version,omitempty"`
    Tags        []string `yaml:"tags,omitempty"`
}

// SkillStore manages the skill directory at ~/.naozhi/learning/skills/.
type SkillStore struct {
    dir string
}

func NewSkillStore(dir string) *SkillStore {
    os.MkdirAll(dir, 0755)
    return &SkillStore{dir: dir}
}

// Create writes a new skill. Returns error if already exists.
func (s *SkillStore) Create(name, content string) error {
    if !validNameRe.MatchString(name) || len(name) > maxNameLength {
        return fmt.Errorf("invalid skill name %q", name)
    }
    if len(content) > maxSkillContentChars {
        return fmt.Errorf("skill content too large (%d chars)", len(content))
    }

    dir := filepath.Join(s.dir, name)
    if _, err := os.Stat(dir); err == nil {
        return fmt.Errorf("skill %q already exists", name)
    }

    if err := os.MkdirAll(dir, 0755); err != nil {
        return fmt.Errorf("create skill dir: %w", err)
    }

    path := filepath.Join(dir, "SKILL.md")
    return atomicWrite(path, []byte(content))
}

// Update overwrites an existing skill's SKILL.md.
func (s *SkillStore) Update(name, content string) error {
    path := filepath.Join(s.dir, name, "SKILL.md")
    if _, err := os.Stat(path); os.IsNotExist(err) {
        return fmt.Errorf("skill %q not found", name)
    }
    return atomicWrite(path, []byte(content))
}

// List returns metadata for all skills (name + description only, token-efficient).
func (s *SkillStore) List() ([]SkillMeta, error) {
    entries, err := os.ReadDir(s.dir)
    if err != nil {
        if os.IsNotExist(err) {
            return nil, nil
        }
        return nil, err
    }

    var skills []SkillMeta
    for _, e := range entries {
        if !e.IsDir() {
            continue
        }
        path := filepath.Join(s.dir, e.Name(), "SKILL.md")
        meta, err := parseSkillMeta(path)
        if err != nil {
            continue
        }
        skills = append(skills, meta)
    }
    return skills, nil
}

// Load reads the full SKILL.md content for injection.
func (s *SkillStore) Load(name string) (string, error) {
    path := filepath.Join(s.dir, name, "SKILL.md")
    data, err := os.ReadFile(path)
    if err != nil {
        return "", fmt.Errorf("load skill %q: %w", name, err)
    }
    return string(data), nil
}

// LoadAll reads and concatenates all skills into a single injection block.
// Used for --append-system-prompt injection at session start.
// Returns empty string if no skills exist.
func (s *SkillStore) LoadAll() string {
    skills, err := s.List()
    if err != nil || len(skills) == 0 {
        return ""
    }

    var sb strings.Builder
    sb.WriteString("\n\n## Learned Skills\n\n")
    sb.WriteString("The following skills were learned from past sessions. ")
    sb.WriteString("Use them when relevant. If a skill is outdated, note it.\n\n")

    for _, meta := range skills {
        content, err := s.Load(meta.Name)
        if err != nil {
            continue
        }
        sb.WriteString("### " + meta.Name + "\n\n")
        sb.WriteString(content)
        sb.WriteString("\n\n---\n\n")
    }
    return sb.String()
}

// parseSkillMeta extracts YAML frontmatter from a SKILL.md file.
func parseSkillMeta(path string) (SkillMeta, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return SkillMeta{}, err
    }

    content := string(data)
    if !strings.HasPrefix(content, "---\n") {
        return SkillMeta{}, fmt.Errorf("no frontmatter")
    }

    end := strings.Index(content[4:], "\n---")
    if end < 0 {
        return SkillMeta{}, fmt.Errorf("unterminated frontmatter")
    }

    var meta SkillMeta
    if err := yaml.Unmarshal([]byte(content[4:4+end]), &meta); err != nil {
        return SkillMeta{}, err
    }
    return meta, nil
}

// atomicWrite writes data to path atomically via temp file + rename.
func atomicWrite(path string, data []byte) error {
    dir := filepath.Dir(path)
    f, err := os.CreateTemp(dir, ".naozhi-skill-*")
    if err != nil {
        return err
    }
    tmp := f.Name()

    if _, err := f.Write(data); err != nil {
        f.Close()
        os.Remove(tmp)
        return err
    }
    if err := f.Close(); err != nil {
        os.Remove(tmp)
        return err
    }
    return os.Rename(tmp, path)
}
```

### 3.6 技能/记忆注入

在 `Router.spawnSession()` 中，新会话启动时注入已学知识。

```go
// internal/learning/injector.go

package learning

import (
    "strings"
)

const (
    memoryCharLimit = 2200  // ~800 tokens, matches Hermes
    userCharLimit   = 1375  // ~500 tokens
)

// Injector builds the system prompt supplement from learned knowledge.
type Injector struct {
    memoryDir string
    skillStore *SkillStore
}

func NewInjector(memoryDir string, skillStore *SkillStore) *Injector {
    return &Injector{
        memoryDir:  memoryDir,
        skillStore: skillStore,
    }
}

// BuildPromptSupplement returns the text to append via --append-system-prompt.
// Includes memory, user profile, and relevant skills.
// Returns empty string if nothing to inject.
func (inj *Injector) BuildPromptSupplement(workspace string) string {
    var sb strings.Builder

    // 1. Global memory
    memory := loadFileContent(inj.memoryDir, "MEMORY.md", memoryCharLimit)
    if memory != "" {
        sb.WriteString("\n\n## Agent Memory\n\n")
        sb.WriteString(memory)
    }

    // 2. User profile
    user := loadFileContent(inj.memoryDir, "USER.md", userCharLimit)
    if user != "" {
        sb.WriteString("\n\n## User Profile\n\n")
        sb.WriteString(user)
    }

    // 3. Project-level context
    if workspace != "" {
        projectCtx := loadProjectContext(inj.memoryDir, workspace)
        if projectCtx != "" {
            sb.WriteString("\n\n## Project Context\n\n")
            sb.WriteString(projectCtx)
        }
    }

    // 4. Learned skills
    skills := inj.skillStore.LoadAll()
    if skills != "" {
        sb.WriteString(skills)
    }

    return sb.String()
}
```

### 3.7 集成点：Router 修改

```go
// 修改 internal/session/router.go

// RouterConfig 新增字段
type RouterConfig struct {
    // ... existing fields ...
    Learning *learning.Engine // nil = learning disabled
}

// spawnSession 中注入学习上下文
func (r *Router) spawnSession(ctx context.Context, key string, resumeID string, opts AgentOpts) (*ManagedSession, error) {
    // ... existing capacity check logic ...

    // === 新增：注入学习上下文 ===
    if r.learning != nil {
        supplement := r.learning.Injector.BuildPromptSupplement(workspace)
        if supplement != "" {
            args = append(args, "--append-system-prompt", supplement)
        }
    }

    spawnOpts := cli.SpawnOptions{
        Key:             key,
        Model:           model,
        ResumeID:        resumeID,
        ExtraArgs:       args,
        WorkingDir:      workspace,
        // ...
    }

    // ... existing spawn logic ...
}

// Cleanup 中触发学习
func (r *Router) Cleanup() {
    // ... existing cleanup logic that collects expired sessions ...

    // === 新增：对过期会话触发学习 ===
    if r.learning != nil {
        for _, e := range expired {
            if sess, ok := r.sessions[e.key]; ok {
                entries := sess.EventEntries()
                if learning.ShouldReview(e.key, entries) {
                    r.learning.Reviewer.Submit(e.key, sess.workspace, entries)
                }
            }
        }
    }

    // ... rest of cleanup ...
}

// Reset 中触发学习
func (r *Router) Reset(key string) {
    r.mu.Lock()
    s, ok := r.sessions[key]
    // 在删除之前收集 entries
    var entries []cli.EventEntry
    var workspace string
    if ok && r.learning != nil {
        entries = s.EventEntries()
        workspace = s.workspace
    }
    // ... existing reset logic ...
    r.mu.Unlock()

    // 在锁外触发学习
    if r.learning != nil && learning.ShouldReview(key, entries) {
        r.learning.Reviewer.Submit(key, workspace, entries)
    }
}
```

### 3.8 新增命令

```go
// 修改 internal/dispatch/commands.go 或 internal/routing/resolve.go

// 新增学习相关命令:
//   /learn        — 手动触发当前会话的学习 review
//   /skills       — 列出已学技能
//   /skills view <name> — 查看技能详情
//   /skills delete <name> — 删除技能
//   /memory       — 查看当前记忆
//   /memory clear — 清空记忆
```

### 3.9 Config 扩展

```yaml
# config.yaml 新增段
learning:
  enabled: true
  model: "haiku"                    # review 用的模型（降成本）
  review_timeout: "120s"            # 单次 review 超时
  min_events: 3                     # 最小 event 数才触发
  data_dir: "~/.naozhi/learning"    # 数据目录
  memory:
    char_limit: 2200                # MEMORY.md 字符上限
    user_char_limit: 1375           # USER.md 字符上限
  skills:
    max_count: 100                  # 最大技能数
    max_content_chars: 100000       # 单个技能最大字符数
```

```go
// internal/config/config.go 新增

type LearningConfig struct {
    Enabled       bool               `yaml:"enabled"`
    Model         string             `yaml:"model"`
    ReviewTimeout string             `yaml:"review_timeout"`
    MinEvents     int                `yaml:"min_events"`
    DataDir       string             `yaml:"data_dir"`
    Memory        LearningMemoryConfig `yaml:"memory"`
    Skills        LearningSkillConfig  `yaml:"skills"`
}

type LearningMemoryConfig struct {
    CharLimit     int `yaml:"char_limit"`
    UserCharLimit int `yaml:"user_char_limit"`
}

type LearningSkillConfig struct {
    MaxCount        int `yaml:"max_count"`
    MaxContentChars int `yaml:"max_content_chars"`
}
```

## 4. Phase 2：持久化记忆

### 4.1 Memory Store

```go
// internal/learning/memory.go

package learning

import (
    "os"
    "path/filepath"
    "strings"
    "sync"
)

const entryDelimiter = "\n§\n"

// MemoryStore manages MEMORY.md and USER.md files.
// Thread-safe. Uses atomic writes to prevent corruption.
type MemoryStore struct {
    mu        sync.Mutex
    dir       string
    charLimit int // MEMORY.md limit
    userLimit int // USER.md limit
}

func NewMemoryStore(dir string, charLimit, userLimit int) *MemoryStore {
    os.MkdirAll(dir, 0755)
    if charLimit <= 0 {
        charLimit = 2200
    }
    if userLimit <= 0 {
        userLimit = 1375
    }
    return &MemoryStore{dir: dir, charLimit: charLimit, userLimit: userLimit}
}

// Add appends an entry to the specified target ("memory" or "user").
// Enforces character limit. Returns error if adding would exceed limit.
func (m *MemoryStore) Add(target, entry string) error {
    m.mu.Lock()
    defer m.mu.Unlock()

    file, limit := m.targetFile(target)
    current := m.readFile(file)

    newContent := current
    if newContent != "" {
        newContent += entryDelimiter
    }
    newContent += strings.TrimSpace(entry)

    if len(newContent) > limit {
        return fmt.Errorf("%s would exceed char limit (%d/%d)", target, len(newContent), limit)
    }

    return atomicWrite(file, []byte(newContent))
}

// Replace finds a substring and replaces it within the target file.
func (m *MemoryStore) Replace(target, oldStr, newStr string) error {
    m.mu.Lock()
    defer m.mu.Unlock()

    file, _ := m.targetFile(target)
    current := m.readFile(file)

    if !strings.Contains(current, oldStr) {
        return fmt.Errorf("substring not found in %s", target)
    }

    updated := strings.Replace(current, oldStr, newStr, 1)
    return atomicWrite(file, []byte(updated))
}

// Remove removes an entry containing the given substring.
func (m *MemoryStore) Remove(target, substring string) error {
    m.mu.Lock()
    defer m.mu.Unlock()

    file, _ := m.targetFile(target)
    current := m.readFile(file)

    entries := strings.Split(current, entryDelimiter)
    var kept []string
    removed := false
    for _, e := range entries {
        if !removed && strings.Contains(e, substring) {
            removed = true
            continue
        }
        kept = append(kept, e)
    }
    if !removed {
        return fmt.Errorf("no entry matching %q in %s", substring, target)
    }

    return atomicWrite(file, []byte(strings.Join(kept, entryDelimiter)))
}

// Read returns the full content of a target file.
func (m *MemoryStore) Read(target string) string {
    m.mu.Lock()
    defer m.mu.Unlock()
    file, _ := m.targetFile(target)
    return m.readFile(file)
}

func (m *MemoryStore) targetFile(target string) (string, int) {
    switch target {
    case "user":
        return filepath.Join(m.dir, "USER.md"), m.userLimit
    default:
        return filepath.Join(m.dir, "MEMORY.md"), m.charLimit
    }
}

func (m *MemoryStore) readFile(path string) string {
    data, err := os.ReadFile(path)
    if err != nil {
        return ""
    }
    return string(data)
}
```

### 4.2 项目级记忆

```go
// internal/learning/project_context.go

// loadProjectContext finds and loads the project-specific CONTEXT.md
// based on the workspace path.
// Convention: if workspace is /home/user/workspace/myapp,
// look for ~/.naozhi/learning/projects/myapp/CONTEXT.md
func loadProjectContext(memoryDir, workspace string) string {
    projectName := filepath.Base(workspace)
    if projectName == "" || projectName == "." {
        return ""
    }

    path := filepath.Join(memoryDir, "projects", projectName, "CONTEXT.md")
    data, err := os.ReadFile(path)
    if err != nil {
        return ""
    }

    content := string(data)
    const projectContextLimit = 1500 // ~550 tokens
    if len(content) > projectContextLimit {
        content = content[:projectContextLimit]
    }
    return content
}
```

### 4.3 Memory Review Prompt

在 review agent 的输出中，`memory_add` 类型的 action 会自动写入对应文件。
Review system prompt 已包含 memory 提取指令（见 3.4 节）。

额外的 memory-only review（不含 skill），用于短对话：

```go
const memoryOnlyReviewPrompt = `Review the conversation and extract user preferences
or environment facts worth remembering.

Output a JSON array. Each item: {"type": "memory_add", "target": "memory"|"user", "content": "..."}

Focus on:
- User revealed preferences about how they want to work
- User corrected your approach (remember the correction)
- Environment facts not derivable from code (e.g., "deploys happen on Fridays")
- User's role, expertise level, communication style

If nothing worth saving, output [].`
```

## 5. Phase 3：FTS 会话搜索 + 技能改进

### 5.1 SQLite FTS5 会话索引

```go
// internal/learning/fts.go

package learning

import (
    "database/sql"
    "path/filepath"
    "strings"
    "time"

    _ "github.com/mattn/go-sqlite3"
)

// FTSStore provides full-text search over past session conversations.
type FTSStore struct {
    db *sql.DB
}

func NewFTSStore(dataDir string) (*FTSStore, error) {
    dbPath := filepath.Join(dataDir, "sessions.db")
    db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
    if err != nil {
        return nil, err
    }

    // Create FTS5 virtual table if not exists
    _, err = db.Exec(`
        CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(
            session_key,
            workspace,
            content,
            timestamp UNINDEXED,
            tokenize='unicode61'
        )
    `)
    if err != nil {
        db.Close()
        return nil, err
    }

    // Metadata table for deduplication
    _, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS indexed_sessions (
            session_key TEXT PRIMARY KEY,
            indexed_at  INTEGER
        )
    `)
    if err != nil {
        db.Close()
        return nil, err
    }

    return &FTSStore{db: db}, nil
}

// Index adds a session's conversation to the FTS index.
// Skips if session_key is already indexed.
func (f *FTSStore) Index(sessionKey, workspace string, entries []cli.EventEntry) error {
    // Check if already indexed
    var count int
    f.db.QueryRow("SELECT COUNT(*) FROM indexed_sessions WHERE session_key = ?", sessionKey).Scan(&count)
    if count > 0 {
        return nil
    }

    tx, err := f.db.Begin()
    if err != nil {
        return err
    }
    defer tx.Rollback()

    stmt, err := tx.Prepare("INSERT INTO sessions_fts(session_key, workspace, content, timestamp) VALUES(?, ?, ?, ?)")
    if err != nil {
        return err
    }
    defer stmt.Close()

    for _, e := range entries {
        if e.Type == "user" || e.Type == "assistant" || e.Type == "result" {
            if e.Summary != "" {
                stmt.Exec(sessionKey, workspace, e.Summary, e.Time)
            }
        }
    }

    tx.Exec("INSERT OR REPLACE INTO indexed_sessions(session_key, indexed_at) VALUES(?, ?)",
        sessionKey, time.Now().Unix())

    return tx.Commit()
}

// SearchResult holds a single FTS search match.
type SearchResult struct {
    SessionKey string
    Workspace  string
    Snippet    string
    Timestamp  int64
    Rank       float64
}

// Search finds sessions matching the query.
// Returns results grouped by session_key, ordered by relevance.
func (f *FTSStore) Search(query string, limit int) ([]SearchResult, error) {
    if limit <= 0 {
        limit = 10
    }

    rows, err := f.db.Query(`
        SELECT session_key, workspace, snippet(sessions_fts, 2, '**', '**', '...', 32),
               timestamp, rank
        FROM sessions_fts
        WHERE sessions_fts MATCH ?
        ORDER BY rank
        LIMIT ?
    `, query, limit)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var results []SearchResult
    for rows.Next() {
        var r SearchResult
        if err := rows.Scan(&r.SessionKey, &r.Workspace, &r.Snippet, &r.Timestamp, &r.Rank); err != nil {
            continue
        }
        results = append(results, r)
    }
    return results, nil
}

func (f *FTSStore) Close() error {
    return f.db.Close()
}
```

### 5.2 搜索命令

```
/recall <query>    — 搜索历史会话中的相关内容
```

搜索结果通过 `/recall` 命令返回给用户，或在新会话启动时自动查找相关上下文。

### 5.3 技能自我改进

技能改进通过两种途径：

1. **Review Agent 发现**：review 时如果发现已有技能需要更新，输出 `skill_update` action
2. **手动触发**：用户通过 `/skills update <name>` 触发特定技能的重新评估

```go
// 在 review prompt 中追加已有技能内容，让 review agent 判断是否需要更新
func buildReviewPromptWithSkills(transcript, workspace string, skillStore *SkillStore) string {
    prompt := buildReviewPrompt(transcript, workspace)

    // 加载全部技能供 review agent 参考
    skills, _ := skillStore.List()
    if len(skills) == 0 {
        return prompt
    }

    var sb strings.Builder
    sb.WriteString(prompt)
    sb.WriteString("\n\n## Existing Skills (for reference)\n\n")
    for _, meta := range skills {
        content, err := skillStore.Load(meta.Name)
        if err != nil {
            continue
        }
        sb.WriteString("### " + meta.Name + "\n")
        sb.WriteString(content)
        sb.WriteString("\n---\n")
    }
    sb.WriteString("\nIf any existing skill is outdated or incomplete based on this conversation, ")
    sb.WriteString("output a skill_update action with the FULL updated content.\n")

    return sb.String()
}
```

### 5.4 学习效果统计

```go
// internal/learning/stats.go

type Stats struct {
    TotalReviews    int       `json:"total_reviews"`
    SkillsCreated   int       `json:"skills_created"`
    SkillsUpdated   int       `json:"skills_updated"`
    MemoryAdds      int       `json:"memory_adds"`
    LastReviewAt    time.Time `json:"last_review_at"`
    ReviewErrors    int       `json:"review_errors"`
    ReviewsSkipped  int       `json:"reviews_skipped"` // below min_events threshold
}
```

Dashboard API 新增 `/api/learning/stats` 端点。

## 6. Engine：组装入口

```go
// internal/learning/engine.go

package learning

// Engine is the top-level coordinator for the learning system.
// Instantiated once in main.go and passed to the Router.
type Engine struct {
    Reviewer   *Reviewer
    Injector   *Injector
    Memory     *MemoryStore
    Skills     *SkillStore
    FTS        *FTSStore
    Stats      *Stats
}

// NewEngine creates and wires all learning components.
func NewEngine(cfg LearningConfig, wrapper *cli.Wrapper) (*Engine, error) {
    dataDir := expandPath(cfg.DataDir)

    skillStore := NewSkillStore(filepath.Join(dataDir, "skills"))
    memoryStore := NewMemoryStore(dataDir, cfg.Memory.CharLimit, cfg.Memory.UserCharLimit)
    injector := NewInjector(dataDir, skillStore)

    reviewer := NewReviewer(ReviewConfig{
        Wrapper:  wrapper,
        Model:    cfg.Model,
        Timeout:  parseDuration(cfg.ReviewTimeout, 120*time.Second),
        SkillDir: filepath.Join(dataDir, "skills"),
        MemDir:   dataDir,
    })

    var fts *FTSStore
    ftsStore, err := NewFTSStore(dataDir)
    if err != nil {
        slog.Warn("learning: FTS init failed, session search disabled", "err", err)
    } else {
        fts = ftsStore
    }

    return &Engine{
        Reviewer: reviewer,
        Injector: injector,
        Memory:   memoryStore,
        Skills:   skillStore,
        FTS:      fts,
        Stats:    &Stats{},
    }, nil
}

// Start begins background workers.
func (e *Engine) Start() {
    e.Reviewer.Start()
}

// Stop shuts down all learning components.
func (e *Engine) Stop() {
    e.Reviewer.Stop()
    if e.FTS != nil {
        e.FTS.Close()
    }
}
```

## 7. main.go 集成

```go
// cmd/naozhi/main.go 中的变更

func run(cfgPath string) error {
    cfg, err := config.Load(cfgPath)

    // ... existing setup ...

    // === 新增：初始化学习引擎 ===
    var learningEngine *learning.Engine
    if cfg.Learning.Enabled {
        learningEngine, err = learning.NewEngine(cfg.Learning, wrapper)
        if err != nil {
            slog.Error("learning engine init failed", "err", err)
            // 不 fatal，learning 是可选功能
        } else {
            learningEngine.Start()
            defer learningEngine.Stop()
        }
    }

    router := session.NewRouter(session.RouterConfig{
        // ... existing fields ...
        Learning: learningEngine,
    })

    // ... rest of setup ...
}
```

## 8. 模块依赖图

```
cmd/naozhi/main.go
  -> config           新增 LearningConfig
  -> learning          新增包
  |    -> engine.go    组装入口
  |    -> trigger.go   触发条件判断
  |    -> reviewer.go  后台 review agent
  |    -> prompts.go   review prompt 模板
  |    -> skillstore.go 技能 CRUD + 原子写入
  |    -> memory.go     记忆 CRUD + 字符限制
  |    -> injector.go   构建 --append-system-prompt 注入内容
  |    -> fts.go        SQLite FTS5 会话搜索 (Phase 3)
  |    -> project_context.go 项目级记忆 (Phase 2)
  |    -> stats.go      学习效果统计 (Phase 3)
  |    -> format.go     event entries → transcript 格式化
  |    -> parse.go      review agent 输出 JSON 解析
  -> session           修改 RouterConfig, spawnSession, Cleanup, Reset
  -> dispatch          新增 /learn, /skills, /memory, /recall 命令
  -> server            新增 /api/learning/* dashboard API
```

## 9. 与 Hermes 的关键差异

| 维度 | Hermes | Naozhi |
|------|--------|--------|
| 学习触发 | 每 10 次 tool call（agent loop 内部） | 会话结束时（更自然，零干扰） |
| Review 执行 | fork 同一 Python 进程，共享内存 | spawn 独立 CLI 进程，JSON 协议通信 |
| Review 模型 | 用主模型（贵） | 用 haiku（便宜 95%） |
| 技能格式 | 完全相同的 SKILL.md + frontmatter | 完全相同（可互通） |
| 记忆格式 | MEMORY.md + USER.md | 完全相同（可互通） |
| 注入方式 | 直接修改 system prompt 对象 | --append-system-prompt（CLI 参数） |
| FTS 搜索 | FTS5 + Gemini Flash 总结 | FTS5 + haiku 总结 |
| 多平台 | 每平台独立上下文 | 跨平台聚合（独特优势） |
| 项目级记忆 | 无 | CONTEXT.md per project（独特优势） |
| 安全扫描 | skills_guard.scan_skill() | Phase 2 加入（初期信任 review agent） |

## 10. 实施计划

### Phase 1（第 1-2 周）：核心学习能力

| 文件 | 行数估算 | 优先级 |
|------|---------|--------|
| `internal/learning/engine.go` | ~80 | P0 |
| `internal/learning/trigger.go` | ~30 | P0 |
| `internal/learning/reviewer.go` | ~200 | P0 |
| `internal/learning/prompts.go` | ~60 | P0 |
| `internal/learning/skillstore.go` | ~200 | P0 |
| `internal/learning/injector.go` | ~100 | P0 |
| `internal/learning/format.go` | ~80 | P0 |
| `internal/learning/parse.go` | ~60 | P0 |
| `internal/config/config.go` 修改 | ~30 | P0 |
| `internal/session/router.go` 修改 | ~40 | P0 |

### Phase 2（第 3 周）：持久化记忆

| 文件 | 行数估算 | 优先级 |
|------|---------|--------|
| `internal/learning/memory.go` | ~150 | P1 |
| `internal/learning/project_context.go` | ~50 | P1 |
| `internal/dispatch/commands.go` 修改 | ~80 | P1 |

### Phase 3（第 4 周）：FTS + 统计

| 文件 | 行数估算 | 优先级 |
|------|---------|--------|
| `internal/learning/fts.go` | ~150 | P2 |
| `internal/learning/stats.go` | ~60 | P2 |
| `internal/server/dashboard_learning.go` | ~100 | P2 |
| `go.mod` 新增 `github.com/mattn/go-sqlite3` | — | P2 |

### 总计：新增约 1,500 行 Go 代码，修改约 100 行现有代码。

## 11. 风险与缓解

| 风险 | 缓解 |
|------|------|
| Review CLI 进程消耗资源 | 用 haiku 模型 + 120s 超时 + 队列限 20 |
| 技能质量退化 | 定期人工 review + `/skills` 命令 |
| --append-system-prompt 过长 | 技能数量上限 100 + 字符限制 |
| CGO 依赖（go-sqlite3） | 可选；Phase 3 可延后或用纯 Go 替代（modernc.org/sqlite） |
| 并发安全 | MemoryStore 用 mutex，SkillStore 用 atomic write |
| Review 输出格式不稳定 | JSON 解析兼容 + 错误容忍 + 日志记录 |
