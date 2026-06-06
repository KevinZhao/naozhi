# UI-CC-ASSET-BROWSER — Claude Code 资产只读浏览器

| 字段 | 值 |
| :--- | :--- |
| 状态 | Draft v6（待评审） |
| 作者 | naozhi team |
| 创建日期 | 2026-05-30 |
| 修订日期 | 2026-05-30 |
| 关联代码 | `internal/cli/backend/profile.go`<br/>`internal/cli/backend/profile_claude.go`<br/>`internal/dashboard/ext/memory/handler.go`（安全读文件范式）<br/>`internal/server/claude_paths.go`（`resolveClaudeDir`）<br/>`internal/server/dashboard.go`（路由注册）<br/>`internal/server/static/dashboard.html` + `dashboard.js`（前端面板） |
| 关联 RFC | `docs/rfc/multi-backend.md`（`backend.Profile` 抽象）<br/>`docs/rfc/memory-link-rendering.md`（`ext/memory` 路径门控）<br/>`docs/rfc/codex-backend.md`（未来 backend 扩展位） |
| 关联 issue | TBD |

## 0. 修订历史

### v6 (2026-05-31) — 缓存改纯事件驱动，删除 TTL

与维护者讨论后,缓存失效策略从「TTL(30s)+mtime」改为**纯事件驱动**——澄清了
TTL 是惰性失效(非后台轮询)的误解后,进一步发现 TTL 其实可被事件驱动完全替代:

- **删除 TTL**。失效改为三类**事件**任一触发,无任何后台定时器/轮询:
  1. **naozhi 经手的变更**(`naozhi upgrade`、plugin 装卸、未来的 dashboard 内
     编辑 N1)→ 变更方主动 `cache.Invalidate()`。覆盖绝大多数真实变更,零延迟。
  2. **请求到达时 mtime 指纹变化**(打开面板/刷新即"事件")→ 失效该 repoRoot
     桶。覆盖外部增删 skill/plugin 目录。
  3. **手动「↻ 刷新」按钮** → 强制失效当前桶。兜底"外部编辑了文件内容但
     mtime 未变"这一罕见情况(原本是 TTL 的唯一职责)。
- **不采用 fsnotify**:watch `~/.claude` 整树(plugin cache 上千目录)会让
  inotify watch 数爆炸 + 跨平台 watcher 生命周期复杂;收益仅是"文件变的瞬间
  知道",但面板没打开时无意义。列为后续可选,首期不做。
- 详见改写后的 §3.4;§9 待评审项 1(TTL 取值)随之关闭。

### v5 (2026-05-31) — 第三轮深度评审，修数据模型缺陷

聚焦数据模型的对抗性评审 + 真实文件核验（hooks.json/.mcp.json/memory 三处
结构假设**全部核实正确**）后，修复 2 个 HIGH + 若干缺口：

- **C（HIGH）Ref 无法唯一定位 hook**：8 个 hook 共享同一 `hooks/hooks.json`，
  `Ref{kind,source,rel}` 完全相同，点开看原文只能返回整个 JSON、无法定位单条。
  **修**：`Ref` + `Asset` 增 `Anchor` 字段（hook 用 hook id；其它资产为空）。
  ReadRaw 对 hook 返回整文件 + `Anchor` 供前端高亮定位。见 §3.2/§4。
- **A（HIGH）缓存 key 缺 RepoRoot**：全量 Inventory 进缓存，但不同 `RepoRoot`
  的请求会命中同一缓存 → 串项目级资产。**修**：缓存改为 `map[repoRoot]entry`，
  key 含 `realpath(RepoRoot)`；用户级/plugin 部分可跨 key 共享子缓存。见 §3.4。
- **B 计数混合来源**：`PluginInfo.AssetCounts` 只覆盖 plugin 来源，user/project/
  memory 的 tab 总数无处取。**修**：`Inventory` 增 `Totals map[kind]int`（全来源
  合计，喂前端 tab 徽标）。见 §3.2。
- **D memory 扫描范围**：本机 19 个 `projects/<encoded>/` 目录，全扫违反"本项目"
  语义。**定**：首期只扫**当前 RepoRoot 对应的那一个**项目的 memory（由
  RepoRoot 反推 encoded 名）；RepoRoot 为空则不出 memory。见 §3.2/§5。
- **E/H 澄清**：handler 负责按 kind 切片（provider 恒返全量）；`repoRootFn` 解析
  契约写明（`?repo=` + 已知 workspace 校验 + 防穿越）。见 §3.3/§9.3。
- **I/K 补充**：P0 阶段 DTO 即定稿形态（Plugins/Totals 可空）；ReadRaw 上限
  1 MiB 超限 413。见 §7/§5。
- **不采纳**：评审建议把 `memory_project` 改名 `memory`——保留原名，语义已清晰；
  "plugin 内嵌 memory"是 N4 外臆测，YAGNI。

### v4 (2026-05-31) — 数据模型决策定稿

与维护者逐条敲定 6 项数据模型决策（原 §9 待定项收口）：

- **D1 类型强度**：`Kind` / `Source.Kind` 用**裸 string**（与现有 `backend.ID`
  约定一致，JSON 最简；靠测试兜拼写）。见 §3.2。
- **D2 hook 粒度**：**每个 hook 条目一行 Asset**（Name=hook id 如
  `pre:bash:dispatcher`），多个 hook 可共享同一 `RelPath`（hooks.json）。可
  观测性是本功能核心诉求,"装了哪些 hook"比"有个 hooks.json"有用。见 §3.2/§3.3。
- **D3 MCP 范围**：首期**只读 `~/.claude/.mcp.json`**（Source=user）；plugin
  内嵌 MCP 启用状态依赖 plugin、易误导，留后续。见 §3.3。
- **D4+D5 聚合粒度（关键）**：**provider 内部恒做完整 Scan → 全量 Inventory
  进缓存，`AssetCounts` 在此一步顺带聚合**；HTTP 层 `?kind=` 只是**从缓存全量
  切片**再序列化。因此 `ScanRequest.Kind` 语义从"只扫这类"收窄为"**只返回
  这类**"——只扫一次盘，消除 `/plugins` 重复扫盘 + "切 tab 时 counts 不全"
  隐患。见 §3.1/§3.4。
- **D6 memory 归属**：**统一进 `Asset`（Kind=memory）**，不单独建模——读取与
  安全门控逻辑同 skill，差异用 `Source.Kind=memory_project` 表达足矣。见 §3.2。

### v3 (2026-05-30) — 第二轮深度评审，修编译级缺陷

v2 的 provider 接口方案在解决 B1（抽象污染）时**引入了一个编译级 BLOCKER**，
另有一处安全 bug 与若干签名/数据不一致。本轮修复：

- **B2（BLOCKER，v2 引入）解决**：`AssetProvider` 接口曾放在
  `internal/cli/backend` 且签名引用 `ccassets.Inventory`/`ccassets.Ref` →
  `backend` ↔ `ccassets` **循环导入，无法编译**；且 `backend` 是被 cli/session
  广泛依赖的底层包，让它依赖含 YAML/文件扫描的重包会污染依赖树。
  **改**：接口与 DTO 类型下沉到**新的零依赖叶子包 `internal/assets`**
  （只放纯类型 + 接口），`backend` 和 `ccassets` 都只依赖它，单向无环。见 §3.0/§3.1。
- **SEC（安全 bug）解决**：`Asset.Path`（绝对路径）原是**导出字段**，
  `json.Marshal` 会泄露给前端，直接违背 §5.6「绝对路径不出网」。
  **改**：绝对路径不进 DTO；服务端按 `Ref` 现场解析，见 §3.2/§4。
- **接口签名修正**：`home`/`repoRoot` 不再既说"构造时传入"又出现在每次调用的
  方法签名里（自相矛盾，且 provider 挂 Profile 是全局单例、构造时拿不到
  per-workspace 的 repoRoot）。**改**：用 `ScanRequest`/`RawRequest` 显式入参，
  调用时传，provider 无状态。见 §3.1。
- **ReadRaw 白名单来源澄清**：`ReadRaw` 内部自行构造并校验 resolvedRoots
  （与 Scan 同一套根推导逻辑），不依赖一个外部传入的 Inventory 句柄。见 §5。
- **数据订正**：全篇"1610"是 v1 误把 marketplace（可安装）计入的初值；**已安装
  skill 实测 792**。§1.1/§2/§6/§3.6 统一订正。
- **memory 来源分类补全**：`Source.Kind` 增加 `"memory_project"`，并明确
  memory 的 `rel`/根推导。见 §3.2/§5。

### v2 (2026-05-30) — 按独立评审修订

经架构评审（1 BLOCKER + 4 重要）+ 代码事实核验（6/6 引用准确）后修订：

- **B1（BLOCKER）解决**：`AssetLayout` 不再塞进 `backend.Profile`。改为 Profile
  只持有一个窄接口 `AssetProvider`（nil = 无资产视图），CC 的目录布局知识
  封装在 `internal/ccassets` 内的 claude provider 里，不污染通用抽象。见 §3.1。
- **I1 解决**：缓存指纹从"裸 mtime"改为 **TTL(30s) + mtime 双条件**——手编
  `SKILL.md` 不改父目录 mtime，裸 mtime 会让列表元数据陈旧。见 §3.4 / §9.4。
- **I3 解决**：memory 原文**不复用** `/api/memory/{slug}`（slug 全局去重有
  身份歧义），统一走 `/api/cc/assets/raw`。见 §4 / §9.1。
- **I4 解决**：并发冷扫描加 `singleflight` 合并，避免 thundering herd +
  并发写缓存竞态（项目强制 `-race`）。见 §3.4 / §8。
- **S2/S3**：构造不写死 `"claude"`，遍历 `backend.All()` 取有 provider 的
  backend；分期新增 **P0 垂直最小切片**（仅用户级+项目级 skill，无缓存无
  plugin），最早交付价值。见 §3.5 / §7。
- **I2/S4/S5/S6**：前端大列表（数百~上千行）须虚拟化/分块、plugin 组默认收起、`/raw`
  多根白名单随 Inventory 刷新、UI 标注"本节点资产"。见 §3.6 / §5 / §6。

### v1 (2026-05-30) — 初稿

首期范围锁定：**backend = Claude Code 单一后端**，**读写 = 纯只读**。
覆盖 7 类资产：skills / plugins / agents / commands / hooks / mcp / **memory**。
写操作（编辑 SKILL.md、enable/disable、plugin 生命周期）与 Kiro/Codex
适配明确列为非目标，留待后续 RFC。

**v1 含一轮真实数据结构验证**（见 §1.2）——对本机 `~/.claude` 实测，证伪了
初稿两处错误假设并据此修订了 §3.1/§3.2：
- plugin 组件路径必须读 `.claude-plugin/plugin.json`，**不能硬编码目录名**；
  且 manifest **禁止声明 `agents`**（靠约定发现）。
- 对 plugin 根**不能全递归** `find SKILL.md`：实测一个 plugin 递归命中 790 个
  但真 skill 仅 249 个，其余是翻译副本/内部镜像噪音。
- marketplaces（可安装）≠ cache（已安装），只读后者。

---

## 1. 背景与动机

Claude Code（以及未来的 Kiro / Codex）通过文件系统约定管理自己的扩展能力：
skills、plugins、agents、commands、hooks、MCP server、memory。这些内容散落在
`~/.claude/` 与 plugin cache 的几十个目录里，用户在 dashboard 上**看不到自己
到底装了什么**——某个 skill 从哪个 plugin 来、某个 agent 的定义长什么样、
memory 里沉淀了哪些事实，全靠到终端里 `find` / `cat`。

naozhi 的 dashboard 已经是用户与 Claude 能力交互的主入口。把"已安装资产"
可视化进来是顺理成章的一步，且**架构上几乎零摩擦**：

1. **数据全在文件系统**——无需调用任何 CLI 子进程，naozhi 直接读文件即可。
2. **`backend.Profile` 是天然挂载点**——现有 `HistoryDir` / `Features` /
   `CostUnit` 字段已建立"per-backend 路径知识集中到 Profile、消费方走
   `backend.Get()`"的约定（`profile.go:71-114`）。本功能延续该模式。
3. **`ext/memory` 是现成的安全读文件范例**——它已解决本功能最大的坑：
   slug 白名单 + `EvalSymlinks` 后用缓存的 `resolvedPrefix` 做 `HasPrefix`
   二次校验，防目录穿越 / symlink 逃逸（`memory/handler.go` 头部注释）。

### 1.1 实地确认的数据源（2026-05-30 本机）

| 资产 | 磁盘位置 | 形态 | 规模 |
| :--- | :--- | :--- | :--- |
| Skills（用户级） | `~/.claude/skills/<name>/SKILL.md` | YAML frontmatter + Markdown | 少量 |
| Skills（项目级） | `<repo>/.claude/skills/<name>/SKILL.md` | 同上 | 本仓 2 个 |
| Skills（plugin 内嵌） | `~/.claude/plugins/cache/<mp>/<plugin>/<ver>/skills/<name>/SKILL.md` | 同上 | **已安装 792 个**（见 §1.2-4） |
| Plugins（清单） | `~/.claude/plugins/installed_plugins.json` | JSON `version:2`，含 scope/installPath/version/installedAt/gitCommitSha | — |
| Marketplaces | `~/.claude/plugins/known_marketplaces.json` | JSON，github repo + installLocation | — |
| Agents | `~/.claude/agents/`、plugin 内 `agents/*.md` | Markdown | — |
| Commands | `~/.claude/commands/`、plugin 内 `commands/*.md` | Markdown | — |
| Hooks | `~/.claude/hooks/`、plugin 内 `hooks/hooks.json` | JSON + 脚本 | — |
| MCP | `~/.claude/.mcp.json`、plugin 内 `.mcp.json` | JSON | — |
| Memory（项目） | `~/.claude/projects/<encoded>/memory/*.md` + `MEMORY.md` | frontmatter + Markdown | — |
| Memory（全局/项目 CLAUDE.md） | `~/.claude/CLAUDE.md`、`<repo>/CLAUDE.md` | Markdown | — |

> **关键观察**：每个 `SKILL.md` 都带规整的 YAML frontmatter（`name` /
> `description` / 可选 `metadata`），解析成本低且稳定。plugin 来源关系可由
> `installPath` 前缀反推。

### 1.2 数据结构实地验证结论（2026-05-30，本机真实数据）

为避免实现踩坑，对核心假设逐一用真实文件证实/证伪，结论如下——**这些直接
约束 §3 扫描器设计，必须遵守**：

1. **`installed_plugins.json` 的 `installPath` 已指向具体版本目录**，扫描器
   直接以它为 plugin 根，**不需自己拼版本号**。例：
   `~/.claude/plugins/cache/ecc/ecc/2.0.0-rc.1`、
   `~/.claude/plugins/cache/claude-plugins-official/gopls-lsp/1.0.0`。

2. **plugin 组件路径必须读 `<installPath>/.claude-plugin/plugin.json`，
   不能硬编码目录名**。实测 `plugin.json`：
   - `skills` / `commands` 是**目录路径数组**（如 `["./skills/"]`），即使
     只有一个也是数组、不接受字符串。
   - **`agents` 字段被 CC 验证器明确禁止**（`agents: Invalid input`）——
     agents `.md` 按**约定**从 `<installPath>/agents/` 自动发现，**不在
     manifest 声明**。hooks 同理（约定 `<installPath>/hooks/hooks.json`）。
   - 简单 plugin（如 gopls-lsp，纯 LSP）**可能完全没有 `skills`/`commands`
     字段**——扫描器必须容忍字段缺失。
   - 权威依据：marketplace 自带 `.claude-plugin/PLUGIN_SCHEMA_NOTES.md`。

3. **绝不能对 plugin 根做全递归 `find -name SKILL.md`**。实测 ecc 一个
   plugin：全递归命中 **790** 个 SKILL.md，但真正的 skill 只有 **249** 个
   （`plugin.json` 声明的 `./skills/` 下）。其余 541 个是噪音：
   - `docs/ja-JP/`、`docs/zh-CN/`、`docs/zh-TW/` 等**翻译副本**；
   - `.agents/skills/` 等内部镜像目录。
   其中 5 个翻译副本**首行不是 `---`、无 frontmatter**（是纯 Markdown 文档）。
   → 扫描器只走 `plugin.json` 声明的 skills 目录的**直接子目录**
   （`<skillsDir>/<name>/SKILL.md`），不递归。

4. **marketplace（可安装）≠ cache（已安装）**。
   `~/.claude/plugins/marketplaces/` 是 marketplace 仓库镜像（818 个
   SKILL.md，"可安装"清单）；`~/.claude/plugins/cache/` 才是**已安装**内容
   （792 个）。本 RFC 只展示**已安装**资产，数据源是 `installed_plugins.json`
   + 各 `installPath`，**不扫 marketplaces 目录**。

5. **各类资产的 frontmatter 形态**（实测）：
   - skill `SKILL.md`：`name` + `description`（+ 可选 `metadata`）。
   - agent `.md`：`name` + `description` + `tools`（数组）+ `model`。
   - command `.md`：**无 `name`**（靠文件名）+ `description` + `argument-hint`。
   - hooks：`hooks.json`（JSON，无 frontmatter；command 字段常是内联脚本，
     仅作展示、不执行）。
   → 解析器需按 kind 区分；command 的展示名取文件名（去 `.md`）。

6. **健壮性要求**：约 0.6% 的 `SKILL.md`（5/792）无 frontmatter。frontmatter
   解析失败**不得中断整个扫描**——降级为"name 取目录名、description 留空"，
   继续扫其余文件。

---

## 2. 目标与非目标

### 2.1 目标（首期）

- G1：dashboard 新增"资产"面板，**只读**展示 Claude Code 已安装的 skills /
  plugins / agents / commands / hooks / mcp / memory。
- G2：按**来源**分组：用户级（`~/.claude`）、项目级（当前 repo `.claude`）、
  plugin 内嵌（标注来自哪个 plugin@marketplace）。
- G3：列表展示元数据（name / description / 来源 / 路径），点击可**查看原文**
  （SKILL.md / agent.md / json 配置 / memory .md）。
- G4：plugins 视图展示安装清单（版本、marketplace、安装时间、commit sha）及
  该 plugin 贡献的资产数量。
- G5：数百~上千 SKILL.md 量级（本机已安装 792）下首屏 < 500ms（靠缓存 + 懒加载原文）。
- G6：复用 `ext/memory` 的路径安全门控，**零目录穿越风险**。

### 2.2 非目标（明确排除，留待后续 RFC）

- N1：**任何写操作**——编辑 SKILL.md、新建、删除。
- N2：**enable / disable** skill / plugin（CC 无统一的 per-item 开关文件，需
  先调研其当前机制）。
- N3：**plugin 生命周期**——安装 / 卸载 / 更新（敏感；应走 CC 官方入口而非
  自己改 `installed_plugins.json` + 动 cache 目录）。
- N4：**Kiro / Codex 后端**——结构各异，每个需独立适配器。本 RFC 只为它们
  在 `backend.Profile` 上**预留扩展字段**，不实现。
- N5：跨节点（reverse-node）资产聚合——首期只看本机 `~/.claude`。

---

## 3. 架构设计

### 3.0 包依赖图（避免 v2 的循环导入 B2）

provider 接口要挂在 `backend.Profile` 上，但接口签名引用 `Inventory`/`Ref`
等 DTO；若把 DTO 放在实现包 `ccassets`，则 `backend` 必须 import `ccassets`
来定义接口、`ccassets` 又必须 import `backend` 来实现接口 → **循环导入**。

**解法**：把接口与 DTO 下沉到一个新的**零依赖叶子包** `internal/assets`：

```
internal/assets/         ← 叶子包：只有纯类型 + 接口，import 标准库之外什么都不依赖
    types.go             // Inventory / Asset / PluginInfo / Source / Ref
    provider.go          // type Provider interface { Scan / ReadRaw }

internal/cli/backend/    ← import internal/assets（在 Profile 上挂 assets.Provider）
internal/ccassets/       ← import internal/assets + backend，实现 assets.Provider
internal/dashboard/ext/ccassets/ ← import internal/assets，调 provider，不碰布局
```

依赖方向单向无环：`backend → assets`、`ccassets → {assets, backend}`、
`ext/ccassets → assets`。`assets` 是叶子，谁都能依赖、它不依赖谁。这与项目
既有 `internal/cli`（接口）vs `internal/cli/backend`（实现）的分层同构。

### 3.1 `backend.Profile` 扩展 —— 只挂一个 provider 接口

**[v2 修订，原 v1 方案被评审 B1 否决]** v1 曾打算把 13 个字段的 `AssetLayout`
直接塞进 `Profile`。评审指出：现有 Profile 字段（`HistoryDir`/`CostUnit`/
`Features`）的共性是"**所有 backend 都有该概念、只是取值不同**"；而 CC 的资产
布局字段（`UserSkillDir` / `PluginComponentDirs` / `MarketplaceManifest` …）
是**CC 文件系统约定的硬编码镜像**，对 kiro/codex 大概率"概念根本不同"而非
"取值不同"。预先为未知形状的 backend 定义 13 个字段，是 premature abstraction，
会把单个 backend 的实现细节抬成全 backend 的接口契约。

**采纳方案**：`Profile` 只新增**一个窄接口字段**（类型来自叶子包 `assets`，
见 §3.0），布局知识封装在 provider 实现内：

```go
// internal/cli/backend/profile.go — 只 import internal/assets
type Profile struct {
    // ... 现有字段 ...

    // AssetProvider 非 nil 时表示该 backend 向 dashboard 暴露"已安装资产"
    // 只读视图。具体磁盘布局（skill/plugin/agent 目录、manifest 路径等）
    // 完全封装在实现内部——Profile 不感知任何 backend 的目录形状,保持
    // "capability description"纯粹性。nil = 不暴露资产视图,前端隐藏入口。
    // CC 的实现由 internal/ccassets 提供并在 profile_claude.go 注入。
    AssetProvider assets.Provider
}
```

```go
// internal/assets/provider.go — 零依赖叶子包(§3.0)
//
// Provider 是 backend 资产视图的最小接口(accept interfaces 原则)。实现无状态:
// 每次调用通过 Request 显式传入运行环境(home/repoRoot),provider 自身不缓存
// 环境、不探测进程状态——这样全局单例的 Profile.AssetProvider 才能服务
// per-workspace 的不同 repoRoot(修正 v2 "构造时传入" 与单例的矛盾)。
type Provider interface {
    // Scan 返回已安装资产快照。【D4/D5】实现内部**恒做完整扫描**并把全量
    // Inventory 连同聚合好的 PluginInfo.AssetCounts 一起缓存(§3.4);req.Kind
    // 非空时只是**从全量结果切片**返回该 kind(按 tab 懒加载,§3.6),底层只扫
    // 一次盘——故 counts 永远完整,且 /plugins 不必重复扫盘。
    Scan(ScanRequest) (*Inventory, error)
    // ReadRaw 在实现自己现场构造并校验的安全白名单内读单个资产原文(§5)。
    // 白名单由 req(home/repoRoot)+ref 重新推导,不依赖外部 Inventory 句柄。
    ReadRaw(RawRequest) ([]byte, error)
}

type ScanRequest struct {
    Home     string // 已解析家目录(server 注入 resolveClaudeDir 的家目录)
    RepoRoot string // 当前工作区根;空 = 跳过项目级来源(§9.3)
    Kind     string // 空=返回全部;否则只**返回**该 kind(不影响内部全量扫描,D4/D5)
}

type RawRequest struct {
    Home, RepoRoot string
    Ref            Ref // 精确定位三元组(§3.2)
}
```

> **为何接口而非独立 registry**：与现有 `NewProtocol func(ProtocolDeps)
> cli.Protocol` 字段同构——Profile 已有"挂构造器/行为"先例;dashboard 遍历
> `backend.All()` 即可发现所有 provider(§3.5),不需第二套注册时序。布局结构体
> (原 v1 `AssetLayout`)降级为 `ccassets` 包内私有类型,不进任何公共 API。

> **plugin 内嵌资产**不混入固定根:provider 以各 plugin 的
> `.claude-plugin/plugin.json` 为准发现(§1.2 结论 2/3),保留 plugin 归属。

### 3.2 扫描器 `internal/ccassets`（新包，含 CC provider 实现）

放在 `internal/` 而非 `internal/dashboard/ext/`，因为它是纯数据采集逻辑、
无 HTTP 依赖，可被 doctor / 测试独立调用（对齐 `internal/discovery` 的定位）。
**本包同时提供 §3.1 的 `AssetProvider` 实现**（claude provider），CC 的目录
布局是包内私有常量/类型,不外泄。

```
internal/ccassets/
  provider_claude.go // 实现 assets.Provider；持有 CC 布局(原 AssetLayout，私有)
  scanner.go        // scan(layout, req) (*assets.Inventory, error)
  frontmatter.go    // 解析 SKILL.md / agent.md 的 YAML frontmatter
  plugins.go        // 解析 installed_plugins.json + known_marketplaces.json
  roots.go          // 由 req+ref 推导并 EvalSymlinks 出 resolvedRoots(§5)
  cache.go          // TTL+mtime 缓存 + singleflight（见 §3.4）
  doc.go
```

> 布局结构体(原 v1 的 `AssetLayout`)降级为本包私有类型 `claudeLayout`,只被
> `provider_claude.go` 使用,不进任何公共 API（B1 修订）。DTO 类型(下)住在
> 叶子包 `internal/assets`,不在本包(§3.0)。

核心 DTO（住 `internal/assets/types.go`，纯类型零依赖）：

```go
type Asset struct {
    Kind        string `json:"kind"`        // skill|agent|command|hook|mcp|memory
    Name        string `json:"name"`
    Description string `json:"description,omitempty"`
    Source      Source `json:"source"`
    RelPath     string `json:"rel_path"`    // 相对其根的路径,作 Ref 回传键
    Anchor      string `json:"anchor,omitempty"` // 【C】文件内子对象定位:hook=hook id;
                                            // mcp=server key;其它资产为空。多个 hook
                                            // 共享同一 RelPath,靠 Anchor 区分单条。
    // 注意:不含绝对路径字段。绝对路径只在 provider 内部由 (Source+RelPath)
    // 现场推导,绝不进 DTO/JSON(修复 v2 的 Asset.Path 导出泄露,见 §0 SEC)。
}

type Source struct {
    // user | project | plugin | memory_project
    //   user           ~/.claude/{skills,agents,...}
    //   project         <repoRoot>/.claude/...
    //   plugin          plugin 内嵌,Plugin 字段标明来源
    //   memory_project  ~/.claude/projects/<encoded>/memory/(memory tab,§5)
    Kind    string `json:"kind"`
    Plugin  string `json:"plugin,omitempty"`  // Kind==plugin: 如 "ecc@ecc"
    Project string `json:"project,omitempty"` // Kind==memory_project: encoded 项目目录名
}

type PluginInfo struct {
    ID          string         `json:"id"`          // "ecc@ecc"
    Version     string         `json:"version"`
    Scope       string         `json:"scope"`
    Marketplace string         `json:"marketplace"` // "ecc" -> github affaan-m/everything-claude-code
    InstalledAt string         `json:"installed_at"`
    CommitSHA   string         `json:"commit_sha,omitempty"`
    AssetCounts map[string]int `json:"asset_counts"` // {"skill":249,"command":79,...}
}

type Inventory struct {
    Assets  []Asset        `json:"assets"`
    Plugins []PluginInfo   `json:"plugins,omitempty"`
    // 【B】各 kind 的**全来源**合计(user+project+plugin+memory),喂前端 tab
    // 徽标。PluginInfo.AssetCounts 只含 plugin 来源,无法表达 tab 总数,故单列。
    Totals  map[string]int `json:"totals"` // {"skill":806,"agent":64,...}
}

// Ref 精确定位单个资产 = Source + Kind + RelPath (+ Anchor)。不用 slug(避免
// ext/memory 的 slug 全局去重歧义,§9.1)。服务端据此选根+校验(§5),浏览器永远
// 不见绝对路径。Anchor 仅 hook/mcp 这类"一个文件多对象"需要(§4 ReadRaw)。
type Ref struct {
    Kind    string
    Source  Source
    RelPath string
    Anchor  string // 同 Asset.Anchor;ReadRaw 对 hook 返回整文件,Anchor 供前端高亮
}
```

`Scan` 流程（**恒全量**，D4/D5；每步对照 §1.2 验证结论）：
1. **用户级 / 项目级固定根**：对 skill 根 walk **直接子目录**读
   `<name>/SKILL.md` frontmatter；agent/command 读 `<dir>/*.md`。打
   `Source{Kind:"user"|"project"}`。`RepoRoot==""` 时跳过项目级。
2. **plugin 内嵌资产**：读 `installed_plugins.json` → 每个 plugin 取
   `installPath`（已含版本，§1.2-1）→ 读 `<installPath>/.claude-plugin/plugin.json`：
   - skills/commands：优先用 `plugin.json` 声明的目录数组；字段缺失回退约定
     目录（§1.2-2）。**只扫直接子目录/直接 `.md`，绝不递归**（§1.2-3）。
   - agents：约定目录 `<installPath>/agents/*.md`（manifest 禁声明，§1.2-2）。
   - 资产打 `Source{Kind:"plugin", Plugin:"<name>@<marketplace>"}`。
3. **hooks【D2】**：读用户级 `~/.claude/hooks/*` 与每个 plugin 的
   `<installPath>/hooks/hooks.json`，**解析 JSON 把每个 hook 条目展开成一行
   Asset**——Name 取条目的 `id`（如 `pre:bash:dispatcher`）、Description 取
   `description`、`RelPath` 都指向该 hooks.json（多 Asset 共享同一文件，正常）。
   解析失败/无 id → 降级 Name 取 `<matcher>:<event>`。
4. **mcp【D3】**：仅读 `~/.claude/.mcp.json`，每个 server 一行 Asset
   （Source=user，Name=server key，Description 取 command 摘要）。**首期不扫
   plugin 内嵌 .mcp.json**（启用状态依赖 plugin、易误导）。
5. **memory【D6 + 评审 D】**：**仅扫当前 RepoRoot 对应的那一个项目**的
   `<Home>/projects/<encoded>/memory/*.md`——`encoded` 由 `RepoRoot` 反推
   （CC 的编码规则：绝对路径里 `/` 替换为 `-`，如
   `/home/ec2-user/workspace/naozhi` → `-home-ec2-user-workspace-naozhi`；
   实现时以该规则生成候选目录名,存在才扫）。`RepoRoot==""` → 不出 memory。
   **不全扫 `projects/*`**（本机 19 个项目目录,全扫违反"本项目"语义）。每文件
   一行 Asset,`Source{Kind:"memory_project", Project:<encoded>}`;frontmatter
   有 `name`/`description` 用之,否则 name 取文件名。`MEMORY.md` 一并列出。
6. 读 `known_marketplaces.json` 补全 plugin 的 github repo 归属（PluginInfo）。
7. **容错**（§1.2-6）：单文件解析失败 → 降级（name 取目录/文件名、description
   空）继续；plugin.json 缺失/坏 → 跳过该 plugin 的 skills/commands，仍尝试
   约定目录。
8. **聚合计数**：全量扫描天然得到 ① 每 plugin 各 kind 计数（`PluginInfo.
   AssetCounts`，仅 plugin 来源）② 各 kind 全来源合计（`Inventory.Totals`，喂
   前端 tab 徽标，评审 B）。两者都在这一次扫描里算出,无需二次扫盘。

> **明确不做**：不扫 `~/.claude/plugins/marketplaces/`（"可安装"镜像，§1.2-4）；
> 不对 plugin 根全递归 `find`；首期不扫 plugin 内嵌 `.mcp.json`（D3）。

### 3.3 dashboard handler `internal/dashboard/ext/ccassets`

严格复刻 `ext/memory` 的注入风格——**不反向 import server**，依赖以接口注入。
handler 本身不持有布局/缓存,只持有 provider 列表(由 server 构造时从
`backend.All()` 收集,见 §3.5)与读环境:

```go
package ccassets // internal/dashboard/ext/ccassets

type Handler struct {
    providers map[string]assets.Provider // backendID -> provider(仅含 provider!=nil 者)
    home      string         // resolveClaudeDir 的家目录(server 注入)
    repoRootFn func(*http.Request) string // 按请求解析当前工作区根(§9.3),无则 ""
    limiter   IPLimiter      // 照抄 ext/memory/deps.go 的接口(Allow/AllowRequest)
}

// GET /api/cc/assets?backend=claude&kind=&repo=        -> Inventory（列表，不含原文）
// GET /api/cc/assets/raw?backend=&kind=&source=&plugin=&project=&rel=&anchor=&repo=  -> 原文
// GET /api/cc/plugins?backend=claude                   -> []PluginInfo
```

`backend` 查询参数缺省为唯一有 provider 的 backend（首期只有 claude）。
`repo` 是当前 workspace（§9.3，服务端校验后映射为 `ScanRequest.RepoRoot`）;
`kind` 用于按 tab 懒加载;`project` 仅 memory 用（`Source.Project`）。

**职责边界【评审 E】**:provider 的 `Scan` **恒返回全量 Inventory**（含 Totals）;
**按 `kind` 的切片在 handler 做**(从全量 Assets filter 出该 kind 再序列化)。
这样缓存只缓全量(§3.4)、Totals 永远完整,handler 是无状态薄层。缓存与安全
白名单都在 **provider 内部**(§3.4/§5)——handler 只做参数解析、限流、调
`provider.Scan/ReadRaw`、切片、写 JSON。`IPLimiter` 接口照抄
`ext/memory/deps.go`（核验确认其形状,handler.go:100-132 / deps.go:9-15）。

### 3.4 缓存策略 —— 纯事件驱动（无 TTL、无后台轮询）

**[v6 修订：删除 TTL，改纯事件驱动]** 背景:裸目录 mtime 指纹有缺陷——编辑
`<name>/SKILL.md` **内容**不改 `<name>/` 目录 mtime(文件系统特性),裸 mtime
会让列表 name/description 陈旧(评审 I1)。v5 曾用「TTL 30s 兜底」,v6 进一步发现
TTL 可被事件驱动完全替代,遂删除——**没有任何后台定时器,平时 0 扫描**。

**采纳方案**(provider 内部,`cache.go`):
- **缓存 key 含 RepoRoot【评审 A,HIGH】**:全量 Inventory **按 `realpath(RepoRoot)`
  分桶缓存**(`map[string]*entry`),`RepoRoot==""` 自成一桶。否则两个不同
  workspace 的请求会命中同一缓存、串入彼此的项目级/memory 资产。singleflight
  的合并键同样含 RepoRoot(否则会把 A 的扫描结果发给 B)。
  - 注:user/plugin 来源与 RepoRoot 无关,理论上可抽成跨桶共享的子缓存以省内存;
    首期**不做**这层优化(workspace 数量有限,整桶缓存够简单),列为后续。
- **失效 = 以下三类事件任一触发**(无 TTL、无轮询;平时纹丝不动):
  1. **naozhi 经手的变更主动失效**:`naozhi upgrade`、plugin 装卸、未来的
     dashboard 内编辑(N1)等 naozhi 自己执行的操作完成后,直接调
     `cache.Invalidate()`(或失效相关桶)。覆盖绝大多数真实变更,**零延迟**。
  2. **请求到达时比对 mtime 指纹**:打开面板/刷新这一动作本身就是"事件"——
     此刻比对指纹(plugin cache 根 + 用户级 skill 根 + 该桶 repo 的
     `.claude/skills` 根 + 该桶 `projects/<encoded>/memory` 根);变了则重扫该桶。
     覆盖**外部进程增删** skill/plugin 目录。
  3. **手动「↻ 刷新」按钮**:强制失效当前桶 → 下次请求重扫。**兜底**"外部
     编辑了文件内容、但 mtime 碰巧没变"这一罕见情况(原 TTL 的唯一职责)。
- **不采用 fsnotify**:watch `~/.claude` 整树(plugin cache 上千目录)→ inotify
  watch 数爆炸 + 跨平台 watcher 生命周期复杂;收益仅"文件变的瞬间知道",但
  面板没打开时无意义。列为后续可选,首期不做。
- **并发(评审 I4)**:用 `golang.org/x/sync/singleflight`(key 含 RepoRoot)把并发
  冷扫描合并成一次(多 tab/多刷新同时打 `/api/cc/assets` 时,数百~上千文件只
  walk 一次,不 thundering herd);缓存读写用 `sync.RWMutex` 保护。项目强制
  `-race`,§8 含并发测试。
- **frontmatter bounded read**:只读到第二个 `---` 为止,不读正文。`bufio`
  边界读,参考 `discovery/scanner_readbounded`(核验确认存在)。

> **失效事件 1 的接线**:provider 需暴露 `Invalidate(repoRoot string)` /
> `InvalidateAll()`。naozhi 现有的 selfupdate / plugin 操作路径调用它。这是
> provider 接口之外的一个**可选**方法(只 ccassets 实现用得到,不进
> `assets.Provider` 公共接口,避免污染——见 §9 接口边界讨论)。

### 3.5 路由注册与构造（遍历 backend，不写死 "claude"）

**[v2 修订：评审 S2]** 路由注册三行(对齐 `dashboard.go:204-250` 实测风格):

```go
s.mux.HandleFunc("GET /api/cc/assets", auth(s.ccAssetsH.HandleList))
s.mux.HandleFunc("GET /api/cc/assets/raw", auth(s.ccAssetsH.HandleRaw))
s.mux.HandleFunc("GET /api/cc/plugins", auth(s.ccAssetsH.HandlePlugins))
```

handler 在 `Server.New`/`registerDashboard` 里构造时**遍历 `backend.All()`,
收集所有 `AssetProvider != nil` 的 backend**,而非硬编码 `"claude"`——这样
kiro/codex 一旦注册自己的 provider 就自动出现在面板,N4 的"预留"才名副其实:

```go
providers := map[string]assets.Provider{}
for _, p := range backend.All() {
    if p.AssetProvider != nil {
        providers[p.ID] = p.AssetProvider
    }
}
// repoRootFn 按请求解析当前工作区(§9.3);home 是 resolveClaudeDir 的家目录。
s.ccAssetsH = extccassets.New(providers, claudeHome, s.repoRootForRequest, limiter)
```

无任何 provider 时三个路由仍注册,返回空 Inventory(不 404,前端据空列表
隐藏入口)。

### 3.6 前端面板（虚拟化 + 按 tab 懒加载 + plugin 组默认收起）

沿用 cron 面板范式(核验确认:`btn-cron` + `openCronPanel()` overlay,
dashboard.js:11619):

- `dashboard.html` 侧栏 header 加 `hdr-btn`(拼图图标)`onclick="openAssetsPanel()"`,
  与 `btn-cron` 并列(`dashboard.html:2235`)。
- 新增 overlay `#assets-panel`:
  - 左侧分类 tab:Skills / Plugins / Agents / Commands / Hooks / MCP / Memory。
  - **按 tab 懒加载(评审 S6)**:切到某 tab 才请求/渲染该 kind,不一次拉全量
    1MB Inventory。`/api/cc/assets?backend=&kind=` 支持按 kind 过滤。
  - 右侧列表按 Source 分组:用户级 / 项目级 **置顶展开**,**各 plugin 组默认
    收起(评审 I2/§9.2)**——避免数百个 plugin skill 淹没用户自己的内容。
  - **大列表虚拟化(评审 I2)**:skill 列表可达上千行(本机 792),必须虚拟滚动/分页懒
    渲染,否则全量 DOM + 搜索重排会卡。这是 G5 首屏目标的前端侧关键,不止
    后端扫描。
  - 行点击 → 右侧抽屉展示 `/api/cc/assets/raw` 原文(复用现有 `inlineMd`
    Markdown 渲染;plugin 来源顶部显示 `来自 ecc@ecc` chip)。
  - Plugins tab:卡片列出版本/marketplace/安装时间/贡献资产数。
- **范围标注(评审 S5)**:面板标题注明"本节点资产"——dashboard 是多节点聚合
  入口,但首期只看主节点 `~/.claude`(N5),避免用户误以为是全局/子节点视图。
- 纯 vanilla JS,无构建链;CSP 不引入新外部资源。

---

## 4. API 设计

### `GET /api/cc/assets`

响应：

```json
{
  "assets": [
    {
      "kind": "skill",
      "name": "deep-research",
      "description": "Deep research harness ...",
      "source": { "kind": "plugin", "plugin": "ecc@ecc" },
      "rel_path": "skills/deep-research/SKILL.md"
    },
    {
      "kind": "skill",
      "name": "dev-workflow",
      "source": { "kind": "project" },
      "rel_path": ".claude/skills/dev-workflow/SKILL.md"
    }
  ]
}
```

> DTO **不含绝对路径字段**（§3.2）；前端用 `kind`+`source`+`rel_path`
> 三元组（=`Ref`）作 `/raw` 查询键，服务端在 provider 内部据此推导绝对路径
> 并白名单校验（§5）。浏览器永远拿不到服务端绝对路径，也不自己拼路径。

### `GET /api/cc/assets/raw?backend=&kind=&source=&plugin=&project=&rel=&anchor=`

参数即 `Ref`(`kind`+`source`(+`plugin`/`project`)+`rel`(+`anchor`))。返回
`text/plain`(或 `text/markdown`)原文。服务端流程在 **provider 内部**:
1. 按 `kind`/`source`/`plugin`/`project` 推导该资产所属的**单个**根(§5)。
2. `filepath.Join(root, rel)` → lexical `HasPrefix` → `EvalSymlinks` →
   再 `HasPrefix(resolvedRoot)` 双重校验(§5)。
3. 大小上限 **1 MiB**(评审 K)+ `ReadBounded`;超限 **413**。

> **hook 的原文【评审 C】**:多个 hook 共享同一 `hooks/hooks.json`,`rel` 相同。
> ReadRaw 对 hook **返回整个 hooks.json**(不能只切 JSON 子树给用户),由前端用
> `anchor`(=hook id)在渲染时**高亮/滚动到对应条目**。即"文件级读取 + 客户端
> 锚点定位",而非服务端切片。mcp 同理(anchor=server key)。skill/agent/command/
> memory 是"一资产一文件",anchor 为空,行为不变。

> **memory 也走这里**(评审 I3):memory 原文**不复用** `/api/memory/{slug}`
> ——那个端点的 slug 查找是全局去重的(同名只返第一个),与本面板"按 path
> 精确定位"语义冲突,会出现"列表显示这条、点开是另一 project 同名文件"。
> 统一用 `Ref{kind:"memory",source,rel}` 精确读。`/api/memory` 保持只服务
> inlineMd 的 `[[slug]]` hover。

### `GET /api/cc/plugins`

返回 `[]PluginInfo`（§3.2）。

---

## 5. 安全边界

继承并复用 `ext/memory` 的成熟门控（核验确认 R242-SEC-7 双重校验存在,
handler.go:273-309）:

1. **读白名单(多根,ReadRaw 现场推导——评审 S4 / v3 修正)**:原文只允许从一组
   **已 `EvalSymlinks` 解析**的根读取。与 `ext/memory` 单根不同,本功能有 8+
   类根,且 **plugin 根是动态的**(`cache/<mp>/<plugin>/<ver>/`,每装一个一个,
   卸载即失效)。`ReadRaw(RawRequest)` **不依赖外部 Inventory 句柄**(v2 含糊处),
   而是在 `roots.go` 内**按 `req.Home`/`req.RepoRoot`/`ref.Source` 现场推导出
   该资产所属的那**一个**根并 `EvalSymlinks`:
   - `Source.Kind==plugin`:读 `installed_plugins.json` 取该 `Plugin` 的
     `installPath` 作根(已含版本,§1.2-1)。plugin 不存在/已卸载 → 404。
   - `user`/`project`/`memory_project`:固定根(见下)+ `ref.Source.Project`。
   推导逻辑与 `Scan` 共用 `roots.go`,保证"能列出 ⟺ 能读"一致;无需把全量
   resolvedRoots 随 Inventory 缓存,按需单根解析即可,内存与一致性都更简单。
   - **memory 根**:`<Home>/projects/<ref.Source.Project>/memory/`;`Project`
     字段须先过白名单正则(下条),再 join+EvalSymlinks+HasPrefix(`<Home>/projects/`)。
2. **双重前缀校验**:lexical `HasPrefix`(join 后立即)+ `EvalSymlinks` 后再
   `HasPrefix`,挡 `../` 与 symlink 逃逸。照搬 `memory/handler.go` R242-SEC-7。
3. **rel 参数白名单正则**:禁止绝对路径、`..`、控制字节。
4. **大小/行数上限** + `ReadBounded`,防超大文件 OOM。
5. **auth + 限流**:所有路由走现有 `auth()` + `IPLimiter`,与 `/api/memory`
   一致。**只读 GET,无状态变更,无需 CSRF**(与项目既有约定一致)。
6. **绝对路径不出网**:前端只拿 `Ref` 三元组,永远拿不到服务端绝对路径。

---

## 6. 性能

- 冷启动:walk plugin 已安装 skill(本机 ~792,非 marketplace 的 1610)+ 各自
  frontmatter bounded read(前 ~20 行)。数百 ms 量级,面板按需打开、非首屏。
- 热路径:TTL 内 + mtime 未变 → 命中缓存 `Inventory`,O(1);singleflight 合并
  并发冷扫描(§3.4)。
- **前端侧(评审 S6)**:`/api/cc/assets` 按 `kind` 分块,前端按 tab 懒加载 +
  大列表虚拟化,避免一次传/渲染 ~1MB JSON + 上千 DOM 节点。响应启用 gzip。
  G5"首屏<500ms"指**打开面板默认 tab**的端到端,不是一次拉全部 7 类。

---

## 7. 分期与工作量

**[v2 新增 P0 垂直最小切片——评审 S3]** 先交付用户最高频诉求("我自己手写的
skill 在 dashboard 看得到、看得了原文"),完全避开 plugin 解析与缓存复杂度:

| 阶段 | 内容 | 估时 |
| :--- | :--- | :--- |
| **P0** | **垂直切片**:`AssetProvider` 接口 + claude provider 仅扫**用户级+项目级 skill**(无 plugin、无缓存、直接实扫,量小)+ list/raw 两路由 + 极简前端列表。端到端可用 | **1.5d** |
| P1 | provider 扩展:plugin manifest/plugin.json 解析 + agents/commands/hooks/mcp + frontmatter 容错(§1.2-6) | 2d |
| P2 | 缓存(TTL+mtime+singleflight)+ 表驱动/并发测试 | 1d |
| P3 | 前端完整面板:7 tab + Source 分组折叠 + 虚拟化 + plugins 卡片 + 原文抽屉 | 2.5d |
| P4 | memory tab(走 Ref 精确读)+ 范围标注 + e2e + 文档 | 1.5d |
| | **合计** | **~8.5d** |

P0 自带价值、可独立合入并收集反馈;P1/P2 纯后端无 UI 依赖;P3 前端依赖 P1
的完整数据。每阶段独立可测。

---

## 8. 测试计划

- **ccassets 扫描器**：表驱动，构造临时 `~/.claude` fixture（含 plugin cache
  目录树 + installed_plugins.json），断言 `Inventory` 分组/计数正确；含
  symlink 逃逸、坏 frontmatter、空目录等边界。
- **缓存（事件驱动，v6）**：① mtime 不变 → 命中缓存不重扫 ② mtime 变 → 失效
  重扫 ③ 主动 `Invalidate(repoRoot)` → 该桶下次请求重扫、其它桶不受影响
  ④ 手动刷新等价于强制失效。参考 recent_parsed_index 测试。**无 TTL 测试**
  （v6 已删 TTL）。
- **并发(I4)**:`-race` 下 N goroutine 同时 `Scan` 冷启动,断言只 walk 一次
  (singleflight 生效)、无数据竞争。
- **缓存分桶(评审 A)**:两个不同 `RepoRoot` 的 Scan 各自只见自己的项目级+memory
  资产,不串桶;`RepoRoot==""` 桶无项目级/memory。
- **计数(评审 B)**:`Inventory.Totals` = 各 kind 全来源合计;`PluginInfo.
  AssetCounts` 仅 plugin 来源;两者对同一 plugin skill 的计数自洽。
- **hook 展开(评审 C/D2)**:一个 hooks.json 含 N 个条目 → N 行 Asset,各带
  不同 `Anchor`、相同 `RelPath`;ReadRaw 返回整文件。
- **memory 范围(评审 D)**:仅当前 RepoRoot 对应 encoded 目录被扫,其它 project
  的 memory 不出现。
- **handler 安全**：`../` 穿越、symlink 逃逸、绝对路径、超大文件、控制字节
  全部 403/400/413；正常路径 200。**plugin 卸载后旧 Ref 读不到**(动态白名单 S4)。
- **e2e**：打开面板 → 切 tab(懒加载)→ 点 skill 看原文 → 搜索过滤 → plugin
  组默认收起 → plugins 卡片。
- `go test -race ./internal/ccassets/... ./internal/dashboard/ext/ccassets/...`

---

## 9. 未决问题

已定夺项(留存记录):

- ~~memory 复用 `/api/memory/{slug}` 还是独立~~ → **定:独立走 `/raw` Ref
  精确读**(I3,§4)。
- ~~plugin command/agent 是否全列~~ → **定:全列但 plugin 组默认收起**(I2,§3.6)。
- ~~缓存指纹是否够~~ → **定:不裸 mtime;最终为纯事件驱动**(I1→v6,§3.4)。
- ~~frontmatter 缺失~~ → **定:降级取目录/文件名,description 留空**(§1.2-6)。
- ~~D1 类型强度~~ → **定:裸 string**(v4,§3.2)。
- ~~D2 hook 粒度~~ → **定:每个 hook 条目一行 Asset**(v4,§3.2 步骤 3)。
- ~~D3 MCP 范围~~ → **定:首期只读 `~/.claude/.mcp.json`**(v4,§3.2 步骤 4)。
- ~~D4/D5 聚合粒度~~ → **定:内部恒全量扫描+缓存,HTTP 按 kind 切片;counts
  在全量时聚合**(v4,§3.1/§3.4)。
- ~~D6 memory 归属~~ → **定:统一进 Asset(Kind=memory)**(v4,§3.2 步骤 5)。
- ~~缓存失效策略(TTL?)~~ → **定:纯事件驱动,删除 TTL**(v6,§3.4):naozhi 经手
  变更主动失效 + 请求时比对 mtime + 手动刷新按钮兜底;不用 fsnotify。

仍待评审:

1. **repoRoot 解析契约(评审 H,v5 收口)**:`repoRootFn(*http.Request) string`
   按如下契约解析,**待你确认**:
   - 前端把当前选中会话的 workspace 路径作 `?repo=` 传入;
   - 服务端**校验该路径是 dashboard 已知 workspace 之一**(比对现有 session 的
     workspace 集合,杜绝任意路径——防 `?repo=/etc` 穿越);
   - 校验通过用作 `ScanRequest.RepoRoot`(已是 session 构造时 realpath 过的);
     不在已知集合 / 缺省 → `RepoRoot==""` → 跳过项目级 + memory 来源。
   开放问题:无选中会话(如刚进 dashboard)时,是否默认取"最近活跃 workspace"
   还是就空着?倾向空着,让用户先选会话。
2. **provider 接口边界**:`assets.Provider` 公共接口保持 `Scan`/`ReadRaw` 两方法;
   缓存失效(v6 §3.4 事件 1)所需的 `Invalidate(repoRoot)`/`InvalidateAll()` 作为
   `ccassets` **具体类型的方法**暴露给 selfupdate/plugin 路径调用,**不进公共
   接口**(避免污染、也因 kiro/codex 未必有同款失效语义)。未来 enable/disable(N2)
   的写方法同样倾向届时扩,不预留空方法。**待确认此分层。**
3. issue 号待分配。

---

## 10. 附录：为何不调用 `claude` CLI 列资产

CC 无稳定的 headless "list installed assets" 接口；即便有，spawn 子进程比直接
读文件慢且引入失败模式。所有数据均为本地静态文件，直接读是最简、最快、最可测
的方案。这也是 naozhi"所有状态文件化、不引入外部组件"设计哲学的自然延伸。
