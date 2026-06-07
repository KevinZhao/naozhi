# RFC: session catalog 边界 —— #577 / R237-ARCH-4 的 follow-up 设计

> **状态**: Draft v1（待评审）
> **作者**: naozhi team (cron-cr)
> **创建**: 2026-06-07
> **范围**: `router-god-object-split.md` §8.4 step 2 显式要求的 P7 follow-up RFC。本轮纯设计、不落地代码。
> **关联 issue**: #577（R237-ARCH-4，priority:p1，area:session，needs-design）
> **父 RFC**: `docs/rfc/router-god-object-split.md`（§8.1 P7 / §8.4）
> **关联代码**: `internal/discovery`、`internal/sysession`、`internal/session/router_discovery.go`、`internal/server/discovery_cache.go`、`internal/dashboard/{session,discovery}/handlers.go`

---

## 0. 本轮交付边界（先读这一段）

`router-god-object-split.md` §8.4 把 #577 收窄到"`discovery` ↔ `sysession` 的
session-catalog 职责边界"，并把"是否引入单一 catalog 抽象"明确**延后到本独立
follow-up RFC**（因为 R237-ARCH-4 标了 **Breaking: 是**，会动 RFC 接口，不能混进
P1-P6 的纯结构重排 PR）。

本 RFC 的任务是：**核实这条边界到底存不存在重叠，决定是否需要 catalog 抽象。**

**本轮结论（提前给）**：经 grep/read 核实，#577 表述的"discovery vs sysession
overlap"**在当前代码里技术上不成立**——这两个包**互不 import、catalog 语义不同
维度**。真正存在的、且**已被现有机制良好缝合**的双视角，是 `discovery` ↔
`session.Router` 之间，而非 `discovery` ↔ `sysession`。因此本 RFC 的建议是
**§3 候选 A：关闭 #577（won't-do / superseded），不引入任何统一 catalog 抽象**，
理由是它会重蹈 `internal/session/api` 被 #1600 删除的覆辙（预先 hoist 无消费者的
union 抽象）。

本轮**不落地任何 .go 代码**。

---

## 1. Background & problem

### 1.1 #577 的原始表述

> **Symptom**: discovery and sysession have duplicated responsibilities.
> **Proposal**: Single session catalog abstraction — discovery produces
> `DiscoveredSession` (external CLI processes); sysession consumes router +
> catalog. Breaking on RFC interfaces.

父 RFC `router-god-object-split.md` §8.4 已经做过一轮收窄：
- R234-ARCH-3 的 "RouterView/DTO 统一" 部分 **已由 #1600 关闭**（`internal/session/api`
  仅剩 `SessionVisitor`，`assert.go` 钉死）。
- #577 被收窄到"`discovery` 产出 `DiscoveredSession`、`sysession` 消费 router +
  catalog 的边界重叠"。

本 RFC 接手验证这条收窄后的边界。

### 1.2 三个"catalog"的实际语义（已核实，2026-06-07）

| catalog | 维护者 | "session 存在"的认定 | 代表类型 | file:line |
| :--- | :--- | :--- | :--- | :--- |
| **discovery（外部活跃）** | `internal/discovery` Scanner | 扫 `~/.claude/sessions/*.json` + 进程存活 + 有 JSONL | `DiscoveredSession` | `scanner.go:291`、`Scan` `scanner.go:335` |
| **discovery（历史）** | `internal/discovery` | 扫 `~/.claude/projects/*/*.jsonl`，maxAge 窗口内 | `RecentSession` | `recent.go:84` |
| **session.Router（托管）** | `internal/session.Router` | naozhi 自己 spawn/resume/stub 的 `ManagedSession` | `SessionSnapshot` | `router_core.go` `ss.sessions`，`VisitSessions` `router_discovery.go:145` |
| **sysession（daemon）** | `internal/sysession.Manager` | **编译期固定的内置 daemon 列表** | `daemonRecord` | `manager.go:215`、`registry.go` `builtinDaemons` |

关键观察：

- **discovery 的 catalog = naozhi 不拥有的东西**（外部 CLI 进程 + Claude 写的
  JSONL）。这是 `doc.go` 开宗明义的定位："look at what Claude wrote and what
  Claude is doing"。
- **sysession 的 catalog 根本不是 session 清单**，而是 **daemon 清单**（谁在跑
  AutoTitler / AttachmentGC）。`registry.go` 的 `builtinDaemons` 是静态切片字面量，
  运行时不增删。
- **session.Router 才是唯一的"naozhi 托管 session 清单"**。

### 1.3 核心事实：discovery 与 sysession 零耦合

```
grep -rl "internal/discovery" internal/sysession/   # 空
grep -rl "internal/sysession" internal/discovery/    # 空
```

两个包**互不 import**（父 RFC §8.4 closure 段也已自承："`internal/discovery`
与 `internal/sysession` 当前无互相 import，故 P7 是 net-new breaking 抽象"）。

sysession 消费的是 **Router**，不是 discovery：
- `internal/sysession/router.go:44` `SystemSessionRouter` 接口（consumer-side），
  嵌入 `api.SessionVisitor`（`VisitSessions`）+ `SetUserLabelWithOrigin` /
  `ClearUserLabelOrigin` / `RegisterSystemStub` / `EventEntriesForKey`。
- `internal/sysession/router_adapter.go:22` `RawSystemSessionRouter` + `routerAdapter`
  是**唯一** import `internal/cli` 的桥接层。
- `auto_titler.go:311` 调 `a.router.VisitSessions(...)` 流式读 Router 的 session，
  从不触碰 discovery 的 `DiscoveredSession` / `RecentSession`。

**所以 #577 表述的 "discovery vs sysession overlap" 在当前代码里不存在。**
它们既无 import 边、catalog 语义又分属完全不同维度（外部进程发现 vs 内置 daemon
调度），不存在可被"统一 catalog"消除的重复职责。

### 1.4 真正的双视角在 discovery ↔ Router，且已被缝合

唯一**真实**的"两个视角看同一批 session"，是 `discovery`（外部/历史）与
`session.Router`（托管）之间——但这条边界**早已有显式缝合机制**，并非未解决的重叠：

1. **历史去重**：`dashboard/session/handlers.go:1760` 取
   `h.router.DiscoveryExcludeIDs()`（`router_discovery.go:287`，只排除**有活跃
   进程**的 session ID），作为黑名单传给 `discovery.RecentSessions(...)`。
   → history popover 不会重复显示 Router 已托管的活跃 session。
2. **外部→托管转化**：`internal/server/takeover.go` 的 `Takeover` 流程把一个
   `DiscoveredSession` 杀进程 + `WaitAndCleanup` 后，调
   `router.Takeover(...)`（`handlers.go:414` 附近）转成 Router 的 `ManagedSession`。
3. **UI 有意分面板**：`/api/sessions`（Router 托管，`handlers.go:489` `HandleList`）
   与 `/api/discovered`（外部进程，`dashboard/discovery/handlers.go:175`）是
   **两个独立端点、两个独立 UI 区域**。这是有意的关注点分离，不是 bug。

### 1.5 不存在、也无消费者需要统一的 session 类型

系统里**没有**任何 `SessionView` / `SessionDTO` / unified entry 试图"一个类型
表示来自 Router 或 discovery 的 session"。三套 DTO 各自独立、按 API 字段分离：

| DTO | 来源 | API 字段 |
| :--- | :--- | :--- |
| `session.SessionSnapshot` | Router | `/api/sessions.sessions[]` |
| `discovery.RecentSession` | 历史 JSONL | `/api/sessions.history_sessions[]` |
| `discovery.DiscoveredSession` | 外部进程 | `/api/discovered[]` |

这是**有意的设计**，不是碎片化遗留。三者字段含义不同（`SessionSnapshot` 有
naozhi 的 key/state/workspace；`DiscoveredSession` 有 PID/ProcStartTime 用于
PID-reuse 检测；`RecentSession` 有 RetiredAt 排序提示）。强行统一会引入一个
最宽并集类型，大部分字段对大部分来源无意义。

### 1.6 Summary 的双来源是物理必然，不是可消除的重叠

对抗性审查时提出过一个候选反驳："`Summary`（会话标题）有两个独立来源——
discovery 从 `~/.claude/sessions-index.json` 读（`LookupSummaries`
`scanner.go:992`），Router 的 `SessionSnapshot.Summary` 从**进程内事件日志**
（`proc.LastResponseSummary`，`managed.go:606-610`）来——是否构成 metadata 重叠？"

核实后**证否**：

- 两个 Summary 来源服务**不相交**的对象集，是**物理必然**而非可统一的重复：
  - 托管 session **有活进程**，标题取自进程内事件流（实时、无需读盘）。
  - 外部/历史 session **没有 naozhi 进程**，只能从 Claude 写的 sessions-index
    文件读——这正是 discovery "看 Claude 写了什么"的定位（`doc.go`）。
- **`LookupSummaries` 的消费者只有 `dashboard/session/handlers.go`**（server 层
  HTTP，`handlers.go:1717`），**零流向 sysession**（grep 确认）。
- **AutoTitler 决策不读 `Summary`**：它读 `UserLabel`/`LabelOrigin`/
  `MessageCount`/`LastActive`（`auto_titler.go:329-354`），excerpt 取自
  `EventEntriesForKey` 返回的 Router 事件日志（`auto_titler.go:555`
  `SystemEventEntry.Summary`），与 discovery 的 `LookupSummaries` 无任何数据流。

所以 Summary 双来源**不在 discovery↔sysession 之间**，也不构成可被统一 catalog
消除的重复——把没有进程的外部 session 的标题"统一"到 Router 里读，等于要 Router
去读盘做 discovery 已经在做的事，纯增成本。此点记入 §4 判据备查。

---

## 2. Goals & non-goals

### 2.1 Goals

- G1：核实 #577 收窄后的边界（discovery ↔ sysession）是否真实存在重叠 → **§1 已证否**。
- G2：给出 #577 的处置决策（关闭 / 收窄再延后 / 引入抽象）并陈述理由（§3）。
- G3：若未来确有需求，记录"什么信号才触发统一 catalog"的判据，避免无消费者预建（§4）。

### 2.2 Non-goals

- NG1：本轮不落地任何生产代码。
- NG2：不引入统一 session catalog 抽象（§3 决策）。
- NG3：不改 `discovery` ↔ `Router` 现有缝合机制（`DiscoveryExcludeIDs` / `Takeover`
  / 双端点）——它们工作正常，非本 issue 范围。
- NG4：不复活 `internal/session/api` 被 #1600 删除的 union mixin（父 RFC NG4）。
- NG5：不做 `internal/discovery` 内部的 path/proc/history 三分（R222-ARCH-16，
  独立 ticket，见 `discovery/doc.go`）。

---

## 3. Alternatives considered

### 3.1 候选 A — 关闭 #577（won't-do / superseded）（**推荐**）

**理由**：
1. **前提不成立**：§1.3 证明 discovery 与 sysession 零耦合、catalog 不同维度，
   "duplicated responsibilities" 在代码里找不到落点。
2. **真实边界已缝合**：§1.4 的 discovery↔Router 双视角已有
   `DiscoveryExcludeIDs` + `Takeover` + 双端点三重显式机制，无未解决问题。
3. **无消费者**：§1.5 没有任何代码想要统一的 session 视图；强行造抽象 = 预先
   hoist。这正是 #1600 删 `api` union 的教训（父 RFC §1.3 / §7.2：Lookup/
   Lifecycle/Mutator union "一年零消费者被删"）。
4. **Breaking 收益为负**：R237-ARCH-4 自标 Breaking。一个无消费者、还要破坏
   RFC 接口的抽象，ROI 显著为负。

**动作**：在 #577 上贴本 RFC 链接，标 `wontfix` + close，注明"边界已由
`DiscoveryExcludeIDs`/`Takeover`/双端点缝合，无统一抽象需求；未来按 §4 判据
重新立项"。

### 3.2 候选 B — 引入 `SessionCatalog` 接口统一三视角

形如：
```go
type SessionCatalog interface {
    ListManaged() []SessionSnapshot      // from Router
    ListDiscovered() []DiscoveredSession // from discovery live
    ListHistory() []RecentSession        // from discovery history
}
```

- 缺点（致命）：
  1. 这只是把三个现有调用点用一个接口**包一层**，不消除任何重复——因为本就
     没有重复。纯增加间接层。
  2. 谁实现它？只能是 server 层（已经分别持有 `discoveryCache` 和 `router`），
     等于把 `dashboard/{session,discovery}/handlers.go` 现有的清晰分工塞进一个
     god-interface。与 `consumer-interfaces.md`（Implemented v3）"每 consumer
     各自单接口、不拆/不并 sub-interface"的既定方向相悖。
  3. Breaking RFC 接口（R237-ARCH-4 已标），却零功能收益。
- **否决。**

### 3.3 候选 C — 把 sysession 也接入"catalog"（按 issue 字面）

issue 字面建议 "sysession consumes router + catalog"。但 sysession 当前只需
`VisitSessions` 读 Router 托管 session 来给它们打标题（AutoTitler）/做 GC
（AttachmentGC）。它**没有任何理由**去消费 discovery 的外部进程或历史 JSONL——
那些不是 naozhi 托管的，sysession daemon 对它们无操作权。

- 缺点：给 sysession 引入一条它根本不需要的 discovery 依赖边，凭空制造 §1.3
  目前没有的耦合。
- **否决。**

**决策：选 A。** 关闭 #577，不引入抽象。

---

## 4. 未来重新立项的判据（防止本决策被误读为"永不做"）

只有当**同时**出现下列信号时，才重开统一 catalog 的设计：

1. **出现真实消费者**：某个 UI 或 API 需要在**同一个列表**里混合展示托管 +
   外部 + 历史 session（例如"全局会话搜索"跨三源），且去重/排序逻辑在调用点
   重复出现 ≥2 处。
2. **sysession 真的需要 discovery 数据**：某个未来 daemon（非 AutoTitler/
   AttachmentGC）需要对**外部 CLI 进程**或**历史 JSONL**做调度决策。
3. 判据沿用父 RFC §8.2 / `consumer-interfaces.md` 的成功范式（`SessionVisitor`）：
   **"有真实消费者再加"**——先有 ≥1 个活消费者主动 embed，才在合适的包新增
   capability + `assert.go` 钉签名，**禁止**预先 hoist。

在此之前，三套 DTO + 三个端点 + `DiscoveryExcludeIDs`/`Takeover` 缝合是
正确的、低耦合的稳态。

---

## 5. Risk & rollback

- 本轮零代码，无运行时风险。
- 唯一"风险"是决策性的：若评审认为 §1 的核实有遗漏（例如存在本 RFC 未发现的、
  确实把 discovery 与 sysession 耦合的调用点），则应推翻候选 A，转候选 B/C 重审。
  → 缓解：§6 列出可复现的核实命令，评审可独立复跑证伪。

---

## 6. 核实命令（2026-06-07，可独立复跑）

```bash
# discovery 与 sysession 零互相 import
grep -rl "internal/discovery" internal/sysession/   # 期望: 空
grep -rl "internal/sysession" internal/discovery/    # 期望: 空

# sysession 消费的是 Router（VisitSessions），不是 discovery
grep -n "VisitSessions\|DiscoveredSession\|RecentSession" internal/sysession/*.go
# 期望: 只命中 VisitSessions / SessionVisitor，零 DiscoveredSession/RecentSession

# discovery↔Router 的缝合点
grep -n "DiscoveryExcludeIDs" internal/session/router_discovery.go        # :287
grep -n "DiscoveryExcludeIDs" internal/dashboard/session/handlers.go      # history 去重
grep -n "router.Takeover" internal/dashboard/discovery/handlers.go        # 外部→托管

# 三个独立端点
grep -n "func.*HandleList" internal/dashboard/session/handlers.go         # /api/sessions
grep -n "func.*HandlerList" internal/dashboard/discovery/handlers.go      # /api/discovered

# 无统一 session DTO
grep -rn "SessionView\|SessionDTO\|UnifiedSession" internal/   # 期望: 空
```

---

## 7. 决策摘要

| 问题 | 结论 |
| :--- | :--- |
| discovery 与 sysession 有职责重叠吗？ | **否**。零互相 import；catalog 不同维度（外部进程 vs 内置 daemon）。 |
| 真实的双视角在哪？ | `discovery` ↔ `session.Router`，且已由 `DiscoveryExcludeIDs`/`Takeover`/双端点缝合。 |
| 需要统一 catalog 抽象吗？ | **否**。无消费者，会重蹈 #1600 覆辙；Breaking 但零收益。 |
| #577 处置 | **关闭**（superseded by 现有缝合机制 + 本 RFC §4 判据）；父 RFC §8.1 P7 标记完成。 |
| 何时重开 | §4 两个信号同时出现，且沿 `SessionVisitor` "有消费者再加"范式。 |
