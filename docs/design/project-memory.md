# 项目记忆 (Project Memory) 设计

> 定期对项目中的对话做 review 和总结，沉淀成项目记忆，反向注入到后续会话。
>
> 本文档是 `docs/rfc/learning-system.md` 的**精简落地版**——只做"项目记忆"这一刀，砍掉 skill 库、用户画像、FTS5 检索等更重的组件。后续可按需再加。

## 1. 目标与非目标

### 1.1 目标

- 对每个 project 下发生的会话（planner + 绑定的 IM 会话），**定期**生成一份结构化的"项目记忆"
- 记忆包含：**当前目标、关键决策、已知限制、踩过的坑、约定偏好**，不是对话流水
- 记忆**反向注入**到该 project 下所有新会话的 system prompt
- 对运维可见：dashboard 能看、能改、能删某条记忆

### 1.2 非目标（本期不做）

- ❌ Skill 库（拆到下一期，独立 RFC 已有）
- ❌ 用户画像 / 跨 project 全局记忆
- ❌ 对话全文检索（FTS5）
- ❌ 在 agent loop 中实时注入（Claude CLI 不支持动态改 system prompt）

## 2. 存储布局

复用 `ProjectConfig.MemoryFile` 已有字段（当前未实装），规范成固定路径：

```
<project>/.naozhi/
├── project.yaml               # 已存在
└── memory/                    # 新增
    ├── MEMORY.md              # 索引 — 启动时注入（≤ 150 行）
    ├── goals.md               # 单条记忆（按类型）
    ├── decisions_<slug>.md
    ├── constraints_<slug>.md
    ├── conventions_<slug>.md
    └── .state.json            # review 状态（最后一次 review 时间 / cursor）
```

**为什么放进 `<project>/.naozhi/` 而不是 `~/.naozhi/`**
- 项目记忆应该跟着 project 走，git 可以选择是否提交（`MEMORY.md` 建议提交，详情 `.md` 可选）
- naozhi 已有约定：project 级配置就在 `<project>/.naozhi/project.yaml`

**记忆文件 frontmatter（借鉴 Claude Code memory 约定）**

```markdown
---
type: decision | goal | constraint | convention | reference
title: 短标题
created: 2026-05-02
updated: 2026-05-02
source_sessions: [session-id-1, session-id-2]   # 追溯来源
confidence: high | medium | low                 # review agent 打分
---

<正文：一句话规则 + **Why:** + **How to apply:**>
```

**索引文件 `MEMORY.md`**：永远短、永远是指针。

```markdown
# <project-name> 项目记忆

## 目标
- [当前迭代目标](goals.md) — 一句话 hook

## 决策
- [为什么选 stream-json 而非 SDK](decisions_stream_json.md) — 延迟 4-5x 优势
- [侧边栏 Remove 即真删](decisions_sidebar_lifecycle.md) — 不启动回填

## 约束
- [-p 模式 session 不可见](constraints_pipe_mode.md)

## 约定
- [部署用 systemctl](conventions_deploy.md)
```

## 3. 触发机制

三路触发，全部走同一个 `review.Submit()`：

| 触发源 | 何时 | 阈值 |
|--------|------|------|
| **Session 生命周期** | `Router.Cleanup()` / `Router.Reset()` / 进程退出 | 会话 events ≥ 3 且 含 ≥1 tool_use |
| **定时聚合** | Cron job（默认每日 03:00） | 自上次 review 后有新会话 |
| **手动** | Dashboard 按钮 / IM 命令 `/review` | 无 |

**生命周期触发** 只做"单会话摘要"：只生成一个 draft 文件到 `memory/.pending/`。
**定时聚合** 做合并：把 pending 的 drafts 合并进正式 `memory/*.md`，去重、更新索引。

这样避免"每结束一个会话就跑一次完整 review"的成本爆炸，也避免实时写入 `MEMORY.md` 导致索引抖动。

```
Session end ──> pending draft (便宜, haiku)
                      │
                      v
Cron 03:00 ──> aggregate pending ──> memory/*.md + MEMORY.md (贵, sonnet)
```

## 4. 数据流

```
┌─────────────────┐
│ Session ends    │  (idle / reset / exit / /review)
└────────┬────────┘
         │ events, workspace, project_name
         v
┌─────────────────────┐
│ memory.Trigger      │  ShouldReview() 过滤
└────────┬────────────┘
         │ Submit(task)
         v
┌─────────────────────┐
│ Pending Queue       │  bounded chan (cap=20)
│ (in-memory)         │  goroutine: single worker
└────────┬────────────┘
         │
         v
┌─────────────────────┐
│ Draft Reviewer      │  spawn `claude -p --model haiku`
│ (单会话 → draft)    │  transcript → JSON {findings: [...]}
└────────┬────────────┘
         │ write
         v
┌─────────────────────┐
│ <project>/.naozhi/  │
│ memory/.pending/    │
│   <session>.json    │
└─────────────────────┘
         ^
         │ 定时读取
┌─────────────────────┐
│ Aggregator (cron)   │  spawn `claude -p --model sonnet`
│ drafts → memory.md  │  输入 drafts + 现有 MEMORY.md
└────────┬────────────┘
         │ write
         v
┌─────────────────────┐
│ memory/*.md         │
│ MEMORY.md (索引)    │
└─────────────────────┘
```

## 5. 核心代码结构

新增 package `internal/memory/`：

```
internal/memory/
├── store.go         # 读写 .naozhi/memory/ 目录
├── store_test.go
├── trigger.go       # ShouldReview 判断
├── trigger_test.go
├── reviewer.go      # Draft Reviewer（单会话 → pending draft）
├── reviewer_test.go
├── aggregator.go    # 定时聚合 drafts → MEMORY.md
├── aggregator_test.go
├── injector.go      # Spawn 时读 MEMORY.md 注入 --append-system-prompt
├── injector_test.go
└── prompts.go       # review + aggregate 的 system prompts
```

**接口（小而明确）**

```go
// Store 负责文件落地，不关心 review 怎么做。
type Store interface {
    LoadIndex(projectPath string) (string, error)           // 读 MEMORY.md 全文
    WritePending(projectPath, sessionID string, draft Draft) error
    ListPending(projectPath string) ([]PendingDraft, error)
    Commit(projectPath string, entries []Entry) error       // 原子写入 memory/*.md + MEMORY.md
    Prune(projectPath string, olderThan time.Time) error    // 清 .pending/
}

// Reviewer 生成 draft（便宜模型）。
type Reviewer interface {
    Submit(task Task)   // 非阻塞，队满即丢并 slog.Warn
    Stop(ctx context.Context) error
}

// Aggregator 合并 pending → 正式记忆（贵模型）。
type Aggregator interface {
    Run(ctx context.Context, projectName string) (Report, error)
}

// Injector 在 session spawn 时提供附加 system prompt。
type Injector interface {
    ForProject(projectPath string) string   // 返回空串或 MEMORY.md 内容
}
```

**依赖方向**：`session` / `cron` / `server` → `memory`，反向不允许。

## 6. 与现有模块的集成点

### 6.1 Session spawn 时注入

在 `internal/session/router.go` 的 `GetOrCreate` 里，构造 `AgentOpts.ExtraArgs` 前：

```go
if r.memoryInjector != nil && workspace != "" {
    if mem := r.memoryInjector.ForProject(workspace); mem != "" {
        opts.ExtraArgs = append(opts.ExtraArgs,
            "--append-system-prompt", mem)
    }
}
```

**注意**：只有会话所在 workspace 命中某个 project 时才注入。非 project 会话（临时 chat）不注入，避免污染。

### 6.2 Session 结束时触发 draft review

在 `Router.Cleanup()` / `Router.Reset()` 里，session 被销毁前：

```go
if r.reviewer != nil {
    entries := managed.Process().EventEntries()
    if memory.ShouldReview(key, entries) {
        r.reviewer.Submit(memory.Task{
            SessionID: managed.ID(),
            Key:       key,
            Workspace: managed.Workspace(),
            Project:   managed.Project(),
            Entries:   entries,
        })
    }
}
```

### 6.3 定时聚合 — 复用 cron 引擎

**不另建 scheduler**，注册为 naozhi 内置 cron job（类似现有的 cron job 机制）：

```
key:      system:memory:aggregate
schedule: 0 3 * * *
handler:  memory.Aggregator.Run(ctx, allProjects)
```

好处：
- 运维面板能看、能暂停、能手动触发
- 跨进程重启幂等（cron store 已持久化）
- 不用自己写 ticker + shutdown 协调

### 6.4 Dashboard 集成

`internal/server/dashboard.go` 增加：

| 路由 | 作用 |
|------|------|
| `GET /api/projects/{name}/memory` | 返回 `MEMORY.md` + 每条记忆 |
| `PUT /api/projects/{name}/memory/{file}` | 手编某条 |
| `DELETE /api/projects/{name}/memory/{file}` | 删除某条，自动更新索引 |
| `POST /api/projects/{name}/memory/review` | 立即触发聚合 |

侧边栏项目面板加一个"记忆"Tab，显示索引和编辑按钮。

## 7. Review Agent 的 Prompt 设计

### 7.1 Draft Reviewer（便宜 · 单会话）

```
System:
你是一个会话审阅员。任务：从一段 naozhi 项目会话的 transcript 中提取"值得沉淀为项目记忆"的点。

严格只输出 JSON：{"findings": [{"type": "decision|goal|constraint|convention|reference",
"title": "≤30 字", "body": "一句规则 + Why + How to apply",
"confidence": "high|medium|low"}]}

什么值得提取：
- 决策及其理由（特别是权衡、否定选项）
- 坑、约束、平台限制
- 用户明确表达的偏好/约定（"不要 X"、"用 Y 部署"）
- 指向外部资源的 pointer

什么不要提取：
- 代码结构、文件路径、git 历史
- 调试过程中已经解决的 bug（解法在代码里）
- 临时状态、本会话内就失效的上下文
- 泛泛的最佳实践

如无可提取项，返回 {"findings": []}
```

### 7.2 Aggregator（贵 · 合并）

```
System:
你是一个项目记忆编辑员。输入：
1) 当前项目 MEMORY.md（索引）和现有记忆详情
2) 一批新的 draft findings

任务：
- 去重：新 finding 和现有某条讲同一件事 → 更新而非新建
- 合并：多条 drafts 讲同一件事 → 合成一条
- 取舍：confidence=low 且无 corroboration 的跳过
- 过时清理：现有记忆被 drafts 明确推翻的 → 标记 superseded

输出严格 JSON actions：
[
  {"op": "create", "type": "...", "slug": "...", "title": "...", "body": "..."},
  {"op": "update", "slug": "existing", "body": "..."},
  {"op": "retire", "slug": "existing", "reason": "..."}
]
```

## 8. 配置

`~/.naozhi/config.yaml` 新增：

```yaml
memory:
  enabled: true
  draft_model: haiku           # 单会话 draft
  aggregate_model: sonnet      # 每日聚合
  min_events: 3                # 触发阈值
  max_pending_per_project: 50  # drafts 超过此数强制触发聚合
  aggregate_cron: "0 3 * * *"
  inject_to_sessions: true
  max_inject_bytes: 8000       # 注入到 system prompt 的上限，超过只注入索引
```

Per-project override 走 `project.yaml`：

```yaml
memory:
  enabled: false               # 个别 project 禁用
  inject_to_sessions: false
```

## 9. 失败模式与降级

| 失败 | 影响 | 处理 |
|------|------|------|
| Draft 模型超时 | 单次 draft 丢失 | log.Warn，不重试（下次会话会再生成） |
| 聚合模型失败 | 当日 drafts 堆积 | drafts 保留，次日 cron 重试；超过 `max_pending_per_project` 时紧急聚合 |
| 注入文件读失败 | session 正常启动但无记忆 | log.Warn，fallback 到空字符串 |
| JSON 解析失败 | 当次 review 结果丢弃 | log.Warn + 保留原始输出到 `memory/.rejected/` 供人工复查 |
| 记忆文件手动编辑冲突 | 聚合覆盖 | 聚合前做 mtime 检测，人工编辑 < 10 分钟内的记忆**跳过**更新 |

## 10. 安全与隐私

- **不跨 project 泄漏**：injector 严格按 project 路径匹配，planner 会话的 workspace 就是 project 根，bound chat 会话也是
- **敏感信息过滤**：draft prompt 明确禁止提取 secrets/tokens/密码；store 层额外跑一遍 regex 拒绝（复用 `internal/netutil` 或新增 redact helper）
- **文件权限**：`memory/` 目录 0700，文件 0600
- **审计**：每次 commit 记 slog，带 sessionID 和新增/修改的 slug 列表

## 11. 可观测性

新增 metrics（slog 结构化，后续可接 Prometheus）：

```
memory.draft.submitted      counter   labels: project
memory.draft.succeeded      counter   labels: project
memory.draft.failed         counter   labels: project,reason
memory.draft.queue_depth    gauge
memory.aggregate.duration   histogram labels: project
memory.aggregate.actions    counter   labels: project,op (create/update/retire)
memory.inject.bytes         histogram labels: project
```

Dashboard 加一个"记忆健康"卡片：最近 review 时间、pending 数、近 7 日 action 统计。

## 12. 实施路线

**P0（MVP，~1.5 周）**
1. `internal/memory/store.go` + 测试：读写 `.naozhi/memory/`、原子提交、prune
2. `trigger.go` ShouldReview + 测试
3. `reviewer.go` 单会话 draft + 测试（用 fake wrapper 验证 prompt/解析）
4. `injector.go` + 测试
5. 接入 `session.Router` 生命周期钩子
6. 接入 session spawn 时注入

**P1（~1 周）**
7. `aggregator.go` + 测试
8. 注册 cron job `system:memory:aggregate`
9. Dashboard 只读视图（列表 + 查看）
10. 手动触发 API

**P2（~3-5 天）**
11. Dashboard 编辑/删除
12. Per-project override
13. Metrics + 记忆健康卡片
14. IM 命令 `/review`

## 13. 遗留问题 / 待定

- **冷启动**：一个已有大量历史会话的 project，首次启用要不要回填？建议**默认不回填**，加一个管理员命令 `/review-backfill --since=30d` 手动触发
- **记忆老化**：超过 N 天未被"命中"的记忆要不要自动淡出？P2 再决定
- **注入格式**：是整段 concat MEMORY.md？还是用结构化 section？初版先直接 concat，观察 token 占用
- **多租户**：若将来一个 project 绑定多个 IM 群，drafts 要不要按来源分桶？初版共用

## 14. 与 `docs/rfc/learning-system.md` 的关系

learning-system.md 是上位 RFC（skill + 多层 memory + 检索）。本文档是其 **memory 子集的首个可交付切片**：

| learning-system.md | project-memory.md (本文) |
|--------------------|---------------------------|
| 全局 `~/.naozhi/learning/` | Per-project `<project>/.naozhi/memory/` |
| MEMORY + USER + skills + FTS5 | 只有 MEMORY |
| Hermes 风格"skill 库" | 不做 |
| 自定义 scheduler | 复用 cron 引擎 |

本文档做完后，learning-system.md 的 **Phase 1.skill 系统** 和 **用户画像** 可独立推进，不与本期耦合。
