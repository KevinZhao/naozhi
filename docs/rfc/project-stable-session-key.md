# RFC: 路线 B — 项目级稳定 Session Key（精确接续，取代 auto-workspace-chain 的语义猜测）

- **Status**: Draft v2.1（v2 二轮：Arch APPROVE-WITH-CHANGES，Go 唯一有效项 Go-BLOCKING-2 措辞未紧；v2.1 已强制 `RebuildChainFiltered` 单锁重建 + 补 hash 碰撞定量 / workspace override / config 语义。设计阶段闭环，待实施。见 §13）
- **Date**: 2026-06-02
- **Owner**: Kevin
- **Bucket**: A（Feature；触碰 session router / key 生成 / 持久化 / dashboard 前端 / 历史面板；Risk Checklist 命中 ≥4 项 → 完整设计路径）
- **取代**: `docs/rfc/auto-workspace-chain.md`（该 RFC 实现的 spawn-attach + 启动 backfill 将下线，见 §9 迁移）
- **使用画像（决定多项设计取舍）**: naozhi 是**单用户单机**系统。"多标签"绝大多数是**刷新页面 / 切回旧 tab / 断线重连**（同 key 重新订阅），而非两个 tab 同时在**同一项目**并行输入。真正的并发同项目输入对单用户极罕见；要并行通常是开**不同项目**（不同 workspace → 不同稳定 key，天然隔离）。这一画像是 §4.6 把"多标签共享进程"定为期望行为、并把 `/clear` 互相影响降级为已知限制的依据。

---

## 1. Background & problem

### 现象（auto-workspace-chain 的失败案例）

session `dashboard:direct:2026-06-02-105927-14-workspace:general`（workspace=`/home/ec2-user/workspace`）只问了一句话，`prev_session_ids` 却被填进 **31 条** origin=`auto-spawn` 的无关会话——拉肚子、打印机选型、升级 CLI、ECC skill 等主题各异的 one-off 全被串成"历史上下文"。

### 根因（已通过可行性实验确认）

auto-workspace-chain 的核心假设是 **「同一 workspace slug 目录 + 7 天时间窗 = 语义连续的对话」**，靠 `pickWorkspaceChain` 扫盘按 mtime 拼 chain。这是一个**语义近似**，在以下情况必然失准：

- `/home/ec2-user/workspace` 这种**公共父目录 / 默认 workspace**：所有"随手开个会话问点啥"的 one-off 都落在同一个 slug（`-home-ec2-user-workspace`），被机械按时间串起。

### 可行性实验结论（2026-06-02）

我们验证了"能否找到一个精确、唯一的源来确定会话接续"：

| 候选源 | 验证结果 | 能否做 single source of truth |
|---|---|---|
| JSONL `parentUuid` | naozhi slug 下 1063 个真实会话文件，**0 个**首行 parentUuid 跨文件指向另一个会话；全部文件内自洽或 NULL | ❌ 不携带跨会话接续信息 |
| JSONL `summary`/`leafUuid` | 抽样未发现 CLI 用其记录跨会话 resume 关系 | ❌ |
| naozhi `session-ids.json` | 仅 2271 个 sessionID 的扁平去重集合 | ❌ 无归属映射 |
| **naozhi `sessions.json` 的同 key 内 sessionID 轮换链**（origin=manual/resume） | `gaokao-4` 的 `prev=26 auto=0` 完全真实——同一 naozhi key 内 `/clear`/`/new`/resume 时 `collectPreviousHistory`（router_lifecycle.go:589）自然 append 旧 ID | ✅ **唯一精确源** |

**核心洞察**：精确的接续关系 naozhi 本来就有，且无需扫盘——它是**「同一个 naozhi session key 内的 CLI sessionID 轮换链」**。auto-chain 想做的"跨不同 key 的归并"在任何精确数据源里都不存在记录，于是只能猜，于是在公共目录上崩掉。

### 为什么会需要"跨 key 归并"

因为 dashboard 每次进同一个项目都生成**带时间戳的新 key**（`dashboard:direct:2026-06-02-105927-14-workspace:general`，时间戳来自 `dashboard.js:6037` 的 `sessionCounter` + ISO 时间），所以同一项目的对话天然碎成一堆独立 key → 事后才需要猜着拼。

> **路线 B 的命题**：让同一项目复用**稳定 key**，使接续关系从一开始就精确，根本不需要事后猜测扫盘。

---

## 2. Goals & non-goals

### Goals

- **G1（精确性）**：同一项目（同 workspace 绝对路径）的"延续对话"复用同一个稳定 key `dashboard:project:<workspace-hash>:<agent>`，其历史接续完全由同 key 内 sessionID 轮换链精确表达，**不再有任何扫盘/时间窗猜测**。
- **G2（唯一锚定）**：key 中的项目标识必须**唯一锚定 workspace 绝对路径**，不能像现有 slug 那样用 basename（`/a/foo` 与 `/b/foo` 都得 `foo` → 碰撞）。
- **G3（不破坏侧边栏多会话）**：用户仍能在同一项目下**显式新建**多个并行会话（保留"我在这个项目里开了 3 个独立对话"的能力）。稳定 key 是"延续"的默认落点，不是"唯一落点"。
- **G4（不破坏进程隔离）**：稳定 key 复用活进程是**期望行为**（延续就该接着同一个 CLI），但多标签页并发写同一 key 必须有明确语义（§4.6）。
- **G5（历史面板不隐藏活动会话）**：稳定 key 长期复用同一 sessionID 时，历史面板不能因 `excludeSessionIDs` 永久隐藏其 JSONL（§4.5）。
- **G6（默认开启，可回退）**：通过 `config.yaml` 开关；关闭后回到"每次时间戳新 key"的旧行为。
- **G7（清退 auto-chain）**：下线 spawn-attach + 启动 backfill；已写入的 auto-* origin chain 提供一次性清理（§9）。

### Non-goals

- ❌ 不做跨 workspace 归并
- ❌ 不做语义聚类（与 auto-chain 一致）
- ❌ 不改 CLI JSONL 格式 / dashboard 翻历史协议
- ❌ 不引入数据库；映射仍文件化
- ❌ 不动 cron / sys / scratch 的 key 命名

---

## 3. Alternatives considered

### A. 修补 auto-chain：给它加 workspace 资格判断（黑名单根目录 / 要求 .git）

✘ 治标不治本。它仍是"同目录 + 时间窗"猜测，只是缩小了出错域。公共目录之外，同一项目里两段完全无关的对话（周一调 bug、周三写文档）照样被时间窗串起。**精确性无法通过过滤候选获得。**

### B. 项目级稳定 key（本 RFC 推荐）

✓ 接续关系从生成时就精确；复用现有同 key sessionID 轮换链（已被 `gaokao-4` 实证为可靠）；零扫盘。

### C. 在 JSONL 写入自定义"接续指针"字段

✘ 改 CLI 行为/落盘格式，naozhi 不应污染 Claude CLI 的 JSONL；且 `--setting-sources ""` 隔离下不可控。

### D. 完全删除 chain 功能（路线 A）

✘ 用户明确想要"项目级连续对话"。路线 A 是止血方案，不是终态。

### E. 后端维护 `workspace → lastSessionKey` 内存映射，前端 key 生成不变（v2 应 Arch 评审补入）

后端 router 持一个 `map[workspaceAbs]lastKey`。用户点项目时，前端不生成稳定 key，而是查后端"该 workspace 上次的 key"，拿到后 `GetOrCreate(lastKey)`（进程死则 resume）。

- **优点**：不改 key 生成语义，`sessionkey` 完全不动；逻辑集中在 router；后端能灵活决定多标签是否共享进程；天然向"未来跨 node/Redis"演进。
- **缺点**：(1) 映射是**内存态**，重启即丢——重启后点项目又会生成新 key，"接续"在最该用的重启场景下失效（与 G1"精确"目标正面冲突）；要持久化就等于再造一份 `sessions.json` 的影子索引，反而更重。(2) 多一次 HTTP 往返。(3) key 仍是旧的时间戳形态 → basename 碰撞问题（G2）未解决，侧边栏可读性/唯一锚定还得另想办法。

**对比结论**：E 的"改动小"是表象——它把"精确性"寄托在易失的内存映射上，恰好在重启这个高价值续接点失效；而 B 把精确性编码进 **key 本身**（确定性、可持久化、重启无损、顺带修 G2 碰撞）。B 的"动 key 生成"换来的是精确性的**单一可信来源**。故仍选 B；E 的"router 内聚"优点已通过"后端唯一生成 stableKey、前端只消费"（§4.2）部分吸收。

**选 B。**

---

## 4. Design

### 4.0 整体数据流

```
用户在 dashboard 点某个项目 "继续对话"
        │
        ▼
前端拿后端项目列表 API 返回的 stableKey（前端不算 hash，见 §4.2）
   key = "dashboard:pj:" + <hash> + ":" + agent
   （时间戳 key 只用于"显式新开一个并行会话"，见 §4.4）
        │
        ▼
后端 GetOrCreate(key)
   ├─ key 已存在且 alive → 复用活进程（精确延续）✓
   ├─ key 存在但进程已退出 → spawnSession(resumeID=旧sessionID)
   │     → collectPreviousHistory append 旧 ID 到 prev（精确链）
   │       origin 标 "manual"（v2 修正：见 §4.4，现有 resume 路径
   │       不写 origin，落默认 manual；与 auto-* 已能区分，§9.2 清理够用）
   └─ key 全新 → spawnSession(resumeID="")，prev 为空（全新项目对话）
        │
        ▼
持久化 sessions.json：key 稳定，prev_session_ids 只含真实轮换链
   （无 auto-* origin）
```

显式"新开并行会话"仍走旧的时间戳 key 路径（§4.4），保留 G3。

### 4.1 Key 形态与唯一锚定（解决 G2）

#### 新增 key 形态

```
dashboard:pj:<workspace-hash>:<agent>
```

- chatType 段用 `pj`（不是 `project`）：**v2 修正 BLOCKING-3**。现有 `internal/sessionkey` 已有 planner key `project:<name>:planner`（platform 段=`project`）。虽然新 key 的 platform 段是 `dashboard`、`IsPlannerKey` 严格判前缀 `project:` 不会误判（已核实 `sessionkey/key.go:107-117`），但若 chatType 也叫 `project`，未来任何 `if parts[1]=="project"` 的判定都会踩雷。改用独特的 `pj` 段从源头消除歧义，成本几乎为零。
- `<workspace-hash>`：workspace **绝对路径**的稳定短哈希（见下），保证 G2 唯一锚定。
- `<agent>`：沿用现有 agent 段（`general` 等）。同一项目不同 agent 是不同的延续线，合理。

#### 为什么不能直接用现有 slug

现有 `sanitizeKeySlug(basename)`（`dashboard.js:5546`）取 `/home/ec2-user/workspace` → `workspace`、`/a/proj` 与 `/b/proj` 都 → `proj`。作为**稳定主键**会跨路径碰撞，把两个不同项目的对话并到一个 key。必须用绝对路径派生。

#### workspace-hash 方案

```
shortHash(absPath) = hex(sha256(filepath.Clean(absPath)))[:16]
```

- 16 hex（64 bit）碰撞概率：birthday paradox 下 n 个项目的碰撞概率 ≈ n²/2^65。单机 n=10^4 时 ≈ 10^8 / 3.7×10^19 ≈ **2.7×10⁻¹²**——可忽略，且不做缓解（§6 风险表已列）。即便天文级巧合命中，后果是两项目对话合并，与下线前 auto-chain 的错接同级、不更糟。
- **v2.1 补充（workspace override 交互）**：hash 基于 **session 当前生效的 workspace 绝对路径**（即 `ManagedSession.Workspace()` 返回值，已合并 `workspaceOverrides`）。含义：若用户用 SetWorkspace 把某 chat 的工作目录改到另一项目，下次"延续"会落到**新目录**的稳定 key——这是正确语义（工作目录变了就是换了项目延续线）。同一 absPath 始终派生同一 key（幂等），override 不会让同一目录裂成两个 key。测试 `TestProjectStableKey_OverrideChangesKeyConsistently` 钉住。
- `filepath.Clean` 归一化尾斜杠 / `.` / `..`，避免 `/a/` 与 `/a` 派生不同 key。
- **v2 修正 BLOCKING-4（抽象归属）**：`ProjectStableKey(absPath, agent) string` 实现放在 **`internal/session`**，不放 `internal/sessionkey`。理由：(1) key 生成职责历来在 `internal/session`（SessionKey / TakeoverKey 等都在此）；(2) `sessionkey` 是零依赖叶子包（depguard 强制，见 `sessionkey/key.go:13-15`），只该承载"前缀常量 + 纯字符串判定"。`sessionkey` 仅新增**纯字符串判定** `IsDashboardProjectKey(key) bool`（无 crypto/filepath 依赖，符合其定位）。`server` 层生成 API 字段时 import `internal/session`（已在 import），不新增对 `sessionkey` 的生成依赖。

#### 可读性补偿

hash 不可读，侧边栏需展示项目名。复用现有 `workspace` 字段（ManagedSession 已存 workspace 绝对路径）+ basename 渲染 label，key 本身只做唯一锚定。**label 与 key 解耦**——这正是修复"basename 碰撞"的关键。

### 4.2 Key 由谁生成（前端 vs 后端）— 关键决策点

两个选项：

| | 前端生成（复刻 sha256） | 后端生成（新增 `GET /api/projects/stable-key?ws=`） |
|---|---|---|
| 一致性风险 | 前后端算法漂移 → key 不匹配 → 静默新建 | 单一实现，无漂移 |
| 往返成本 | 0 | 1 次轻量请求（可与项目列表合并返回） |
| 推荐 | | ✓ **后端生成**，前端拿到即用 |

**决策**：后端在**项目列表 API** 里为每个 project 直接附带 `stableKey` 字段（零额外往返），前端发起延续会话时直接用该值。`session.ProjectStableKey`（§4.1 修正后归属 `internal/session`）是唯一实现源——前端**不复刻 sha256**，彻底消除前后端算法漂移。

**v2 补充 MINOR — 项目列表 API 具体定位**：实现前需先 Grep 确认确切 handler。候选是 `internal/dashboard/project/` 下的项目列表 handler（dashboard 前端拉取项目卡片的数据源）。实现 checklist（§12）已加"定位项目列表 handler 并在其响应 struct 加 `StableKey string json:\"stableKey\"` 字段"。若该 handler 不便 import `internal/session`（避免分层倒置），退化为：handler 返回 workspace 绝对路径，前端拿到后调用一个**专用轻量端点** `GET /api/projects/stable-key?ws=<abs>` 由后端算并返回（仍是后端唯一实现，只多一次往返）。优先前者。

### 4.3 包扩展与抽象归属（v2 修正 BLOCKING-3 / BLOCKING-4）

#### `internal/sessionkey`（零依赖叶子包，只加纯字符串判定）

```go
// DashboardProjectChatType 是项目级稳定 dashboard 会话占据的 chatType 段。
// 用 "pj" 而非 "project"，与现有 planner key（platform 段="project"）
// 的命名空间彻底区分，杜绝任何 parts[1]=="project" 误判。
const DashboardProjectChatType = "pj"

// IsDashboardProjectKey 判断 4 段 key 是否为项目级稳定 dashboard key：
// platform 段=="dashboard" 且 chatType 段=="pj"。纯字符串运算，
// 不引入 crypto/filepath，保持 sessionkey 零依赖契约。
func IsDashboardProjectKey(key string) bool
```

#### `internal/session`（key 生成职责归属层）

```go
// ProjectStableKey 返回项目级稳定 key。absPath 必须是 workspace 绝对路径。
// agent 为空时回退 "general"。
//   dashboard:pj:<sha256(filepath.Clean(absPath))[:16]>:<agent>
// 实现放在 internal/session 而非 sessionkey：key 生成历来归 session，
// 且需要 crypto/sha256 + path/filepath，不应污染零依赖叶子包。
func ProjectStableKey(absPath, agent string) string
```

> 选择把项目标识塞进 **chatType 段**（`dashboard:pj:<hash>:<agent>`）而非新 platform，是因为现有大量路径用 `parts[0]==dashboard` / `parts[3]==agent` 做判定（侧边栏、订阅、agent 解析）。保持 platform=dashboard、agent 在 parts[3]，能让绝大多数路径**零改动**。用 `pj`（而非 `project`）做 chatType 段，既享受零改动，又不与 planner 命名空间产生概念过载。

### 4.4 "延续" vs "显式新建" 的二分（解决 G3）

dashboard 提供两个入口：

| 入口 | 行为 | key |
|---|---|---|
| 点项目卡片 / "继续对话" | 延续：复用稳定 key | `dashboard:pj:<hash>:<agent>` |
| "＋ 新会话"（项目内显式新开） | 新建并行线：旧时间戳路径 | `dashboard:direct:<ts>-<n>-<slug>:<agent>`（保留现状） |

- 默认双击项目 = 延续（解决"我就是想接着上次聊"）。
- 想要干净的新对话 = 显式新建（保留 G3 多会话能力）。
- 现有 quick session（空态"问点什么？"）：保持时间戳 key（它本就是 one-off，不该延续）。

**前端改动点**：`dashboard.js:6037 createProjectSession` 增加 `mode: 'continue' | 'new'` 分支；continue 用后端给的 stableKey，new 走原逻辑。为可测，抽出纯函数 `resolveSessionKey(mode, project, agent)`。

**v2 修正 BLOCKING-1（origin 标记的真相）**：v1 §4.0 写"resume 路径 origin=resume"是**错的**。已核实 `collectPreviousHistory`（router_lifecycle.go:546-604）只返回 ID 链、**不返回 origin**，调用方在 resume 路径下**不调用** `SetPrevSessionOrigins`，于是 resume append 进来的旧 ID 落**默认 manual**（`SnapshotPrevSessionOrigins` 对未标段兜底 "manual"，managed_history.go:138）。

这对本 RFC **无害**，因为 §9.2 清理只需区分 "auto-* vs 其余"，而真实轮换链（无论标 manual 还是 resume）都属"其余"应保留。**因此 v2 不引入 resume 标记**——避免为一个清理逻辑用不到的区分去改 spawn 热路径。若未来 UI 想区分"手动接续 vs resume 接续"，再单独加（OQ §10）。本 RFC 全程统一表述：真实轮换链 origin ∈ {manual}（resume 段亦落 manual）。

### 4.5 历史面板与稳定 key 生命周期（解决 G5，v2 收紧措辞 + 生命周期表）

机制（已核实 `router_discovery.go:282-302` `DiscoveryExcludeIDs`）：exclude 集合按**"有无活进程"**判定，与 key 类型无关——`s.loadProcess()==nil` 的 session 不被 exclude（进历史面板）；有活进程的 session，其当前 sessionID **及整条 prev 链**都被 exclude（不在历史面板重复显示进行中的对话）。

**v2 修正 MINOR — 不说"No-op / 无需处理"，改说"与现有行为一致"并给全生命周期表**：

| 阶段 | 触发 | 有活进程? | 侧边栏 | 历史面板 |
|---|---|---|---|---|
| 活跃 | 刚建 / 正在聊 | ✓ | 显示 | 隐藏（exclude 当前+prev 链）|
| 休眠 | 进程被 idle 回收 / LRU 淘汰（`loadProcess()==nil`，但 router 仍持 key）| ✗ | 显示（suspended 态）| 该 sessionID 解除 exclude，可在历史面板出现 |
| 退休/移除 | Reset / Remove | — | 移除 | JSONL 进历史面板 |

与旧时间戳 key 的**唯一差别**：稳定 key 活得更久（用户反复回到同项目都命中它），所以"活跃期隐藏于历史面板"的窗口更长。这是延续语义的**自然结果**，不是缺陷：进行中的对话本就该在侧边栏而非历史面板。

**测试 pin**：`TestStableKey_ActiveSessionVisibleInSidebarNotHistory` + `TestStableKey_LifecycleVisibility`（活→休眠→重激活，§5.2），防止未来改 exclude 逻辑时回归。

> 注：稳定 key 是否/何时被 idle 回收，沿用现有 router 的进程回收策略（本 RFC 不改）。RFC 不假设它"永久保活"。

### 4.6 多标签页同一稳定 key（解决 G4，v2 依使用画像降级）

**前提（见扉页使用画像）**：naozhi 单用户单机，"多标签"绝大多数是**刷新 / 切回旧 tab / 断线重连**——同一 key 重新订阅。这恰是稳定 key 的**优点**：刷新后用同一稳定 key，`GetOrCreate` 直接 `SessionExisting`（router_lifecycle.go:312）接回同一活进程，历史还在；而旧时间戳 key 刷新后可能生成新 key、接不回原会话。WS hub 早已支持每 key 多订阅（`maxSubscribersPerKey=20`，`wshub_subscribe.go:72`），刷新/重连产生的短暂双订阅本就被正确处理。

行为定义：
- 刷新 / 重连 / 切回 → 接回同一进程，事件广播同步 ✓（这是设计目标）
- 标签 A 发消息、并行打开的标签 B 实时看到 ✓（已支持）

**已知限制（v2 明确降级，不做重 UX）**：若真的两个标签**同时活跃**在同一项目，标签 A 执行 `/clear` 会 Reset 整个进程，标签 B 视图也被清空。对单用户这是**极罕见**场景（要并行通常开不同项目 → 不同 key 天然隔离），故：

- **不引入** v1 设想的 `/clear` 确认框 / "仅本标签本地清除" / 标签数 badge——为极低频场景增加的复杂度不划算。
- 记为**已知限制**，用测试**显式钉住该行为**（而非假装不存在）：`TestStableKey_ResetAffectsAllTabs_KnownLimitation`（§5.2）。
- 不引入"每标签独立进程"——那等于放弃精确延续、退回碎片化（违反 G1）。

> 若未来出现真实多用户 / 频繁同项目并发需求，再评估 §10 OQ 里的"按 tab 维度的本地清除"。当前 YAGNI。

**测试 pin**：`TestStableKey_ConcurrentTabsShareProcess`（并发首建只 spawn 一次）+ `TestStableKey_ResetAffectsAllTabs_KnownLimitation`（§5.2）。

### 4.7 持久化（无 schema 变更）

`sessions.json` 的 key 字段就是数据，不是会产生碰撞的主键（探查确认 store.go:221）。稳定 key 写入 / 重启恢复与现状一致。`prev_session_origins` 字段保留（用于区分 manual/resume），但**不再写入 auto-spawn/auto-backfill**。

### 4.8 与 auto-chain 的关系（下线）

- 删除 `maybeAttachAutoChainOnSpawn` 调用（router_lifecycle.go:789）。
- 删除 `runAutoChainBackfillOnce` 调用（NewRouter 启动路径）。
- 保留 `prev_session_ids` / `prev_session_origins` 持久化机制本身（manual/resume 仍用）。
- `auto_chain.go` / `auto_chain_router.go` 整体移除或降级为 dead code 清理（§9）。
- config `session.auto_chain.*` 字段保留解析但标记 deprecated，读到时 log 一次 warn。

### 4.9 Lock / 并发

不新增 lock 契约——复用现有 GetOrCreate 的 `spawningKeys` inflight 协调（router_lifecycle.go:325）。inflight 去重是**所有 key 共有**的机制，不是稳定 key 独有（v2 修正 MINOR 措辞）。区别在**命中率**：旧时间戳 key 每次唯一，几乎不命中 inflight 门；稳定 key 在同一 workspace 复用，多标签/刷新并发进同项目时使用同一 key，**更容易命中**，从而自然去重并发首建（刷新风暴下尤其有用）。

---

## 5. Test strategy

### 5.1 Unit（`internal/sessionkey/key_test.go`）

| 测试 | 期望 |
|---|---|
| `TestProjectStableKey_Deterministic` | 同 absPath+agent 多次调用结果一致 |
| `TestProjectStableKey_CleanNormalizes` | `/a/`、`/a`、`/a/.` 派生同 key |
| `TestProjectStableKey_DistinctPaths` | `/a/foo` 与 `/b/foo` 不同 key（修复 basename 碰撞） |
| `TestProjectStableKey_EmptyAgentDefaultsGeneral` | agent 空 → general |
| `TestProjectStableKey_4SegmentShape` | 结果是合法 4 段 key，platform=dashboard、agent 在 parts[3] |
| `TestIsDashboardProjectKey_*` | 正/负判定 |

### 5.2 Integration（`internal/session/stable_key_test.go`）

| 测试 | 期望 |
|---|---|
| `TestStableKey_ContinueReusesAliveProcess` | 同 stableKey 第二次 GetOrCreate 返回 SessionExisting，不 spawn |
| `TestStableKey_ContinueResumesDeadProcess` | 进程退出后同 key GetOrCreate → SessionResumed，prev append 旧 sessionID（origin 落 manual，见 §4.4）|
| `TestStableKey_FreshProjectEmptyPrev` | 全新项目 stableKey 首建，prev 为空，**无 auto origin** |
| `TestStableKey_ExplicitNewParallelSession` | "新会话"入口仍生成不同时间戳 key，不与稳定 key 冲突（G3）|
| `TestStableKey_ConcurrentTabsShareProcess` | 两 goroutine 同时 GetOrCreate 同稳定 key，只 spawn 一次（inflight 门）|
| `TestStableKey_ResetAffectsAllTabs_KnownLimitation` | 显式钉住 §4.6 已知限制：一个订阅者 Reset 后，该 key 进程被清、prev 清空（记录而非假装不存在）|
| `TestStableKey_ActiveSessionVisibleInSidebarNotHistory` | 活动稳定 key 在侧边栏可见、不在历史面板重复（G5 pin）|
| `TestStableKey_LifecycleVisibility` | 活→休眠(loadProcess==nil)→重激活 过程中侧边栏/历史面板可见性符合 §4.5 表 |
| `TestStableKey_ChainGrowsOnlyByRealRotation` | 多次 /clear 后 prev 链增长，全部 origin=manual，零 auto |

### 5.3 回归 / 不能 break

- `TestSnapshotChainIDs_*`、`collectPreviousHistory` 相关
- planner key（`project:<name>:planner`）判定不受 `dashboard:pj:<hash>` 影响：前者 platform 段=`project`，后者 platform=`dashboard`/chatType=`pj`，双重区分。新增 `TestKeyParts_DashboardPjVsPlannerNoCollision` 钉住（含"未来 chatType==project 误判"反例）
- `cron_stub_test.go`、`router_history_test.go`
- auto-chain 下线后：所有 `auto_chain_*_test.go` 删除或改为"下线后行为"断言

### 5.4 前端

- `dashboard.js` continue/new 二分的最小可测性：抽出 `resolveSessionKey(mode, project, agent)` 纯函数便于单测。
- e2e（test/e2e）：点项目两次进同一会话、显式新建得到两个会话。

### 5.5 Race

`go test -race ./internal/session/... -run StableKey` 全绿，重点 `ConcurrentTabsShareProcess`。

---

## 6. Risk & rollback

| 风险 | 概率 | 严重性 | 缓解 |
|---|---|---|---|
| 前后端 key 算法漂移 | 中 | 高 | §4.2 决策：唯一后端实现，前端只消费 |
| 侧边栏延续 vs 新建混淆 | 中 | 中 | §4.4 双入口 + UI 文案；label 用 workspace basename |
| 多标签 /clear 互相影响 | 低 | 低 | §4.6 已知限制（单用户画像）；测试钉住，不做重 UX |
| basename 碰撞被新 hash 修复但旧数据仍是时间戳 key | 低 | 低 | 旧 key 自然老化退休；不迁移 |
| planner key 与 dashboard:pj 混淆 | 低 | 中 | `pj` 段 + platform 段双重区分；§5.1/§5.3 测试 pin |
| **hash 碰撞（两项目派生同 key）** | 极低（~2.7×10⁻¹²）| 中 | 不做缓解；后果与下线前 auto-chain 错接同级 |
| auto-chain 下线后用户"翻历史变少" | 中 | 中 | 预期内：原本就是错误拼接；§9 清理脏 chain |

### 回滚

- **配置回滚**：`session.project_stable_key.enabled: false` 的精确语义（v2.1 明确）：
  - 后端仍可在项目列表 API 返回 stableKey 字段（无害），**前端不消费**——continue 入口回退到生成时间戳 key（与今天完全一致）。即"前端是否走稳定 key"由该开关单点控制，无需后端配合下线。
  - **已存在的稳定 key 会话**：自然老化，不主动清理。用户若再点该项目，因前端改走时间戳 key，会**新建**一条时间戳 key 会话；旧稳定 key 仍可从侧边栏/历史访问，直到正常退休。不会卡在损坏状态。
  - auto-chain **不因此开关复活**（§9 下线是独立、不可逆的清理，与本开关解耦）。
- **代码回滚**：feature 集中在 sessionkey 判定 + session.ProjectStableKey + dashboard.js 二分 + 项目列表 API 加字段 + auto-chain 下线 5 处，revert 可控。
- **数据回滚**：升级前备份 sessions.json（沿用 auto-chain 已有的 `.bak.before-*` 钩子）。

---

## 7. Observability

```
DashboardProjectStableKeyContinueTotal   // 走延续路径次数
DashboardProjectStableKeyNewParallelTotal// 显式新建并行会话次数
DashboardProjectStableKeyResumeTotal     // 稳定 key 进程退出后 resume 次数
AutoChainRetiredOnStartup                // 下线时清理的 auto-* chain session 数（一次性）
```

slog：
- 延续命中活进程：`Debug "stable-key continue: reuse alive" key= workspace=`
- 稳定 key resume：`Info "stable-key continue: resume" key= prev_len= old_sid=`
- 启动清理 auto chain：`Info "auto-chain retired: cleared N auto-origin chains" count=`

---

## 8. Compatibility & migration

### 向后兼容

- ✅ 旧时间戳 key 会话继续工作（不迁移，自然老化）
- ✅ `sessions.json` schema 不变
- ✅ planner / cron / sys / scratch key 不受影响
- ✅ 关闭开关 = 完全回到旧行为

### Migration

见 §9。

---

## 9. 下线 auto-chain + 清理脏数据

### 9.1 代码下线

1. router_lifecycle.go: 删 `maybeAttachAutoChainOnSpawn` 调用 + origin stamp 块（789-823）。
2. router_core.go / NewRouter: 删 `runAutoChainBackfillOnce` 调用。
3. 删除 `internal/session/auto_chain.go`、`auto_chain_router.go` 及其测试（refactor-cleaner 跑一遍）。
4. `internal/discovery/workspace_jsonl.go`：删除前用 `git grep ListWorkspaceJSONL` + `git log --all -S ListWorkspaceJSONL` 确认仅 auto-chain 消费（v1 评审已初步 Grep：消费者仅 `auto_chain.go` / `auto_chain_router.go` / 各自测试 + 自身单测，无隐蔽引用）。确认后一并删。
5. config：`AutoChainConfig` 保留解析、读到非默认值时 warn deprecated。
6. metrics：auto-chain 系列标记 deprecated，下个版本删。

### 9.2 启动一次性清理脏 chain（替代 backfill）

NewRouter 启动时，对每个 session 按 `prev_session_origins` 做原子重建（**v2 修正 Go-BLOCKING：两个平行 slice 必须原子同步删除**）：

- 逐 index 计算"保留掩码"：origin ∈ {`auto-spawn`,`auto-backfill`} 的 index 删除，其余（manual / 空 / 未来的 resume）保留。
- **必须**新增专用方法 `RebuildChainFiltered(keepMask []bool)`，在**单次** `historyMu.Lock()` 内**同时**重建 `prevSessionIDs` 和 `prevSessionOrigins`（v2.1 修正 Go 二轮：见下）。
  - **为什么不能用现有两个方法组合**：已核实 `ReplacePrevSessionIDs`（managed_history.go:169）与 `SetPrevSessionOrigins`（:73）**各自独立加锁**，两次调用之间锁会释放。中间态下 reader 调 `SnapshotPrevSessionOrigins` 会看到 `len(prevSessionIDs)=N, len(prevSessionOrigins)=0`，被兜底成 N 个错误的 "manual"（managed_history.go:138）；更糟的是若先 Replace 再 Set，`SetPrevSessionOrigins` 的 drift 检测虽以 post-replace 为基线（`start = len(prevSessionIDs)-len(ids)`，传等长 origins 时 start==0 不报 drift），但**中间窗口的 torn read 无法用"传等长切片"消除**——根因是两个锁段。所以两步组合方案作废。
  - `RebuildChainFiltered` 契约：`len(keepMask) == len(prevSessionIDs)`；在持锁内一次性算出 `newIDs` + `newOrigins`（仅保留 mask 为 true 的 index，两 slice 用同一 mask 同步过滤），直接赋值两个字段后解锁。reader 在锁外只能观察到"重建前"或"重建后"的完整一致状态，无中间态。lock-order 遵守 r.mu→historyMu（启动清理在 NewRouter 单线程，未持 r.mu 进入，安全）。
- 全段都是 auto → 新链为空：`RebuildChainFiltered` 全 false mask，两 slice 同步清空。
- 计数落 `AutoChainRetiredOnStartup` + 单行 slog。

当前脏数据（实测）：`workspace-14`(31)、`naozhi-11`(31)、`naozhi-5`(31)、`naozhi-9`(31)、`gaokao-1`(18) 共 5 条全 auto，启动后会被清空；`gaokao-4`(26 manual)、`JD`(2 manual) 不受影响。

> 一次性，幂等：清理后 origin 中不再有 auto，再次启动 no-op。
> 测试：`TestAutoChainRetire_RemovesAutoSegmentsKeepsManual`（混合链只删 auto 段）、`TestAutoChainRetire_AllAutoClearsChain`、`TestAutoChainRetire_NoDriftMetricFired`（重建后 `AutoChainOriginsLengthMismatch` 不增长）、`TestAutoChainRetire_Idempotent`（二次启动 no-op）。

---

## 10. Open questions

| 问题 | 倾向 |
|---|---|
| 稳定 key 用 sha256[:16] 还是直接用 cleaned-path 转义？ | hash 更短更稳；path 转义可读但超长（key 有 ~515B 上限，深层路径风险）。倾向 hash |
| 显式"新会话"是否也该能"提升为项目延续线"？ | Phase 2；本 RFC 不做 |
| 不同 agent 是否各自独立延续线？ | 是（key 含 agent 段），符合"换 agent = 换对话上下文"直觉 |
| 历史面板是否要按项目分组展示稳定 key 的轮换链？ | Phase 2 UI；本 RFC 只保证数据精确 |
| quick session 是否要可选"落到项目延续线"？ | 默认否（one-off）；OQ 追踪 |

---

## 11. Out of scope

- 跨 workspace / 跨 node 归并
- 历史面板按项目分组 UI
- "新会话"提升为延续线
- 旧时间戳 key 数据迁移到稳定 key

---

## 12. Implementation checklist

- [ ] `internal/sessionkey/key.go` — `DashboardProjectChatType = "pj"` + `IsDashboardProjectKey`（纯字符串判定，零依赖）+ 单测
- [ ] `internal/session/` — `ProjectStableKey(absPath, agent)`（sha256[:16]，归 session 不归 sessionkey）+ 单测
- [ ] 定位项目列表 handler（候选 `internal/dashboard/project/`），响应 struct 加 `StableKey string json:"stableKey"`；不便 import session 则退化为 `GET /api/projects/stable-key?ws=`
- [ ] config schema — 新增 `session.project_stable_key.enabled`（默认 true）；前端依此决定 continue 走稳定 key 还是回退时间戳
- [ ] `dashboard.js` — `resolveSessionKey(mode, project, agent)` 纯函数二分；continue 用后端 stableKey，new 保留时间戳
- [ ] dashboard UI — 项目卡片"继续" vs "＋新会话"双入口（多标签 /clear 为已知限制，不做确认框/badge，见 §4.6）
- [ ] router_lifecycle.go — 删 auto-chain spawn-attach（789-823）
- [ ] router_core.go — 删 backfill 调用，加启动一次性 auto-chain 清理（§9.2，原子重建两 slice，必要时新增 `RebuildChainFiltered`）
- [ ] 删 `auto_chain.go` / `auto_chain_router.go` / `workspace_jsonl.go`(若无他用) + 测试
- [ ] config — AutoChainConfig deprecated warn
- [ ] metrics — 新 4 个 + auto-chain 系列 deprecated
- [ ] 集成 + 单元 + race 测试（§5）
- [ ] e2e（§5.4）
- [ ] `go test ./... -race` 全绿 / `go vet ./...` 无新告警
- [ ] 部署前备份 sessions.json

---

## 13. Reviewer history

### v1 verdicts

- **Go reviewer**: REJECT-NEEDS-V2（3 BLOCKING + 4 MINOR）
- **Architecture reviewer**: APPROVE-WITH-CHANGES（3 BLOCKING + 3 MINOR）

### v1 → v2 diff

| ID | 来源 | 问题 | v2 处理 |
|---|---|---|---|
| Go-BLOCKING-1/3 | Go | §4.4/§4.0 写"origin=resume"，但 `collectPreviousHistory` 不写 origin，resume 段实际落 manual | §4.0/§4.4 改正：真实轮换链统一 origin=manual；§9.2 清理只分 auto-* vs 其余，不需要 resume 标记，**不改 spawn 热路径** |
| Go-BLOCKING-2 | Go | §9.2 清理时 `prevSessionIDs` / `prevSessionOrigins` 两平行 slice 删除可能错位、触发 drift 误报 | §9.2 改为单 historyMu 持有期内原子重建两 slice（`ReplacePrevSessionIDs`→`SetPrevSessionOrigins` 全量等长，start==0 不触发 drift），必要时新增 `RebuildChainFiltered`；加 4 个测试含 `NoDriftMetricFired` |
| Arch-BLOCKING-1 | Arch | chatType 段用 `project` 与 planner `project:` 命名空间概念过载 | §4.1/§4.3 改 chatType 段为 `pj`；新增 `TestKeyParts_DashboardPjVsPlannerNoCollision`（含 chatType==project 误判反例） |
| Arch-BLOCKING-2 | Arch | 多标签共享进程 + /clear 互相清空，心智崩裂，定 P0 | 据"单用户单机"使用画像（扉页）降级为**已知限制**：刷新/重连是常态且为优点；真并发极罕见。不做重 UX，用 `TestStableKey_ResetAffectsAllTabs_KnownLimitation` 显式钉住 |
| Arch-BLOCKING-3 | Arch | `ProjectStableKey` 放零依赖叶子包 sessionkey 致职责泄漏 | §4.1/§4.3 把 `ProjectStableKey` 移至 `internal/session`；sessionkey 仅留纯字符串判定 `IsDashboardProjectKey` |
| Go-MINOR-4 / Arch-MINOR-2 | both | §4.5 "No-op/无需处理"措辞不严谨 + 生命周期未定义 | §4.5 改"与现有行为一致"+ 加活/休眠/退休三阶段可见性表 + `TestStableKey_LifecycleVisibility` |
| Go-MINOR-5 | Go | §4.9 inflight 去重说成稳定 key 独有 | §4.9 改：inflight 是所有 key 共有机制，稳定 key 只是命中率更高 |
| Go-MINOR-6 | Go | config 回退字段未列入 checklist | §12 加 `session.project_stable_key.enabled` |
| Go-MINOR-7 / Arch（API 定位）| both | 项目列表 API 未定位 | §4.2 + §12 给候选 handler + 退化端点方案 |
| Arch-MINOR-1 | Arch | auto-chain 下线前未充分确认 `workspace_jsonl.go` 消费者 | §9.1 加 `git log -S` 确认步骤 |
| Arch-MINOR-3 | Arch | §3 Alternatives 遗漏"后端映射表"方案 | §3 补 Alternative E + 为何仍选 B（E 的精确性寄托于易失内存，重启失效） |

### v2 二轮 verdicts

- **Architecture reviewer**: APPROVE-WITH-CHANGES — 3 个 Arch BLOCKING 全 CLOSED；Alternative E 反驳被认定充分；使用画像有效（核 server_cookie 后确认"多认证身份 ≠ 同项目多客户端并发写"）。变更要求皆为实现澄清（API handler 定位 / config 语义 / hash 定量 / override 交互）。
- **Go reviewer**: REJECT-NEEDS-V3 — **但前提错误**：该评审把"设计未实现"（代码里无 ProjectStableKey / pj 段 / auto-chain 仍在调用）误判为 BLOCKING STILL-OPEN。我们处于**设计阶段**，尚未写实现，这些当然不存在，不构成设计缺陷。剔除该误解后，Go 二轮唯一有效发现是 Go-BLOCKING-2 措辞未紧（见下），其余"STILL-OPEN"作废。Go-BLOCKING-1/3 被 Go 自己判 CLOSED。

### v2 → v2.1 diff

| ID | 来源 | 问题 | v2.1 处理 |
|---|---|---|---|
| **Go-BLOCKING-2（有效）** | Go 二轮 | §9.2 "同一 historyMu 持有期内 ReplacePrevSessionIDs+SetPrevSessionOrigins"——已核实两方法各自独立加锁，组合做不到原子；"必要时新增 RebuildChainFiltered" 措辞太软 | §9.2 改为**强制**新增 `RebuildChainFiltered(keepMask)` 单锁重建两 slice，明确作废两步组合方案 + 给契约 + lock-order |
| Arch-change-1 | Arch 二轮 | hash 碰撞概率未定量 | §4.1/§6 补 birthday paradox ≈2.7×10⁻¹² + 风险表加行 |
| Arch-change-2 | Arch 二轮 | workspace override 与稳定 key 交互未定义 | §4.1 明确 hash 基于 `Workspace()`（含 override），override=换延续线为正确语义 + 测试 pin |
| Arch-change-3 | Arch 二轮 | config enabled=false 语义不精准 | §6 回滚段补完整语义（前端单点控制 / 旧稳定 key 自然老化 / 与 auto-chain 下线解耦）|
| Arch-defer-1 | Arch 二轮 | 项目列表 API handler 未定位 | 维持 §4.2：PR 阶段 Grep 确认（不阻塞设计）|

### v2.1 status

**设计阶段闭环**。Go-BLOCKING-2 措辞已紧、Arch 变更要求已落。剩余项（API handler 定位、UX 防误触）属实现/前端 PR 阶段细化，不阻塞。Implementation 可开始。
