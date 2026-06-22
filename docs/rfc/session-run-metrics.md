# Session Run 耗时度量与历史统计 — 设计 RFC

- 状态：Draft v2（架构/性能/前端三方独立评审已签核，blocking 项已纳入）
- 日期：2026-06-16
- 范围：为普通（非 cron）session 的"一次 run（一轮对话）"提供 naozhi 自测的 wall-clock 耗时度量，落盘成可统计历史，并在 dashboard 展示实时计时 / 历史时间轴 / 聚合指标。
- 关联：复用 [cron-run-history.md](cron-run-history.md) 的 `runStore` 持久化模式与时间轴 UI；复用 `internal/runtelemetry` 事件层。

## 0. 背景与问题

核心产品指标：**能否长时间运行 CC 且按要求完成任务。** 要回答它，需要观测每一次 session run 跑了多久、是否真正完成、有没有撞超时。

现状缺口：

- **cron run** 的耗时度量已完整：`CronRun.DurationMS`（`internal/cron/run.go:46`）落盘 + `/api/cron/runs` + 前端时间轴（`cron_view.js:1943` `formatRunDuration`）。
- **普通 session run** 几乎空白：唯一的 `EventMetadata.TurnDurationMs`（`internal/cli/event.go:114`）是 **CLI 自报**（后端不报则无）、**不持久化**、前端仅在详情区显示一个 ⏱ 图标（`dashboard.js:2654`）。无历史、无法聚合（平均/P95/最长/超时次数）、且时间未与"完成度"绑定——光有时间没有结果，指标无意义。

期望产出：naozhi 在 run 边界自测 wall-clock，落盘 `SessionRun` 记录（`DurationMS` + `Outcome`），dashboard 展示。

## 1. 非目标

- 不改 cron run 度量（已完成，只复用其代码）。
- 不替换 `EventMetadata.TurnDurationMs`（CLI 自报值保留作参考，但不作为权威耗时源）。
- 不做 per-tool / per-阶段 的细粒度耗时分解（首字节延迟之外）；留待后续 RFC。
- 不做跨 session 的全局排行/告警；本期只在单 session 维度统计。

## 2. 一次 run 的定义

一次 run = `ManagedSession.Send` 的一次调用（`internal/session/managed_send.go:135`），以及 passthrough 路径 `SendPassthrough`（`managed_send.go:33`）。

- **开始**：`proc.Send`/`proc.SendPassthrough` 调用前（消息发往 CLI）。
- **首字节**：本轮收到的第一个 CLI 事件（经 `onEvent` 回调拦截，`managed_send.go:176`）。
- **结束**：`proc.Send` 返回（本轮 `result` 事件已到 / idle）。
- **Outcome** 由返回 `err` 判定（见 §5）。

merge/passthrough 语义：一次 `Send` 可能因 merge 服务多个 slot（`SendResult.MergedCount`，`internal/cli/event.go:536`）。**以单次 `Send` 调用为记录粒度**，merge 元数据按需带入，不重复计数。

## 3. 数据模型

### 3.1 `SessionRun` 实体（新包 `internal/session/runhistory`）

照搬 `CronRun`（`internal/cron/run.go:39`）裁剪：

```go
type SessionRun struct {
    RunID       string    `json:"run_id"`       // 16-char hex，naozhi 生成
    SessionKey  string    `json:"session_key"`  // channel:chatType:id（持久身份）
    SessionID   string    `json:"session_id"`   // CLI session ID（运行时）
    StartedAt   time.Time `json:"started_at"`
    EndedAt     time.Time `json:"ended_at"`
    DurationMS  int64     `json:"duration_ms"`
    FirstByteMS int64     `json:"first_byte_ms,omitempty"` // StartedAt→首事件
    Outcome     string    `json:"outcome"`      // completed|error|timeout|canceled
    ErrorClass  string    `json:"error_class,omitempty"`
    CostUSD     float64   `json:"cost_usd,omitempty"`
}
```

不存 prompt/响应正文（避免跨租户泄漏 + 控制体积）；ErrorMsg 不落盘到广播路径（沿用 runtelemetry sysession 的 Sec-LOW-2 策略）。

DurationMS 单调时钟负值保护：照搬 `internal/cron/scheduler_finish.go:323-336`（`if dur < 0 { dur = 0 }`）。

### 3.2 聚合统计 `SessionRunStats`

```go
type SessionRunStats struct {
    Count        int     `json:"count"`
    TotalMS      int64   `json:"total_ms"`
    AvgMS        int64   `json:"avg_ms"`
    P50MS        int64   `json:"p50_ms"`
    P95MS        int64   `json:"p95_ms"`
    MaxMS        int64   `json:"max_ms"`
    CompletedCnt int     `json:"completed_count"`
    ErrorCnt     int     `json:"error_count"`
    TimeoutCnt   int     `json:"timeout_count"`
}
```

由 `Recent(key, N)` 的记录现算（N≤keepCount=200，纯内存排序，廉价）。

## 4. 持久化

照搬 `internal/cron/runstore.go`（`newRunStore` :232 / `Append` :449 / `Recent` / GC）的**真实做法**：内存 ring cache 服务读取，磁盘只存 per-run JSON，冷启动扫目录 warm。

### 4.1 目录布局（评审修订：无 index.json）

```
<store_root>/session-runs/
    <hash(sessionKey)>/
        <run_id>.json       # 完整 SessionRun（唯一落盘单元）
```

- **不写 index.json**（评审 blocking-1 纠正）：cron runStore 实际并不在 Append 时写 index.json，其 Recent/List 全部由内存 ring cache（`runstore.go:104` `recentCache sync.Map`）提供，冷启动靠扫目录 warm（`runstore_disklist.go`）。照 v1 字面"每轮更新 index.json"会造成每轮 4 fsync + 全量重写放大。session 同样只落 `<run_id>.json`。
- `hash(sessionKey)` 用 sha256 前 16 hex（sessionKey 含 `:` 与用户内容，不能直接做目录名 → 路径遍历防护）。
- **`<store_root> = filepath.Dir(cfg.Session.StorePath)`**（评审 blocking-2 纠正）：基于 session 自己的 store_path，**不是** cron 的。session 与 cron 是两个独立配置（`cfg.Session.StorePath` 默认 `~/.naozhi/sessions.json` vs `cfg.Cron.StorePath`），默认同目录但 operator 可分离，不能耦合 cron 配置。Router 已持有 `storePath`（`router_core.go:307`），inline 派生即可（与 cron 各用途 inline `filepath.Join` 的惯例一致，无现成 paths helper）。

### 4.2 保留策略

keepWindow=30d 复用 cron 默认（`internal/cron/limits.go:234-243`），AND 语义。**keepCount 降为 50**（评审 non-blocking-3）：单 session 历史展示 50 条足够，且每 session 一个 cap=keepCount 的常驻 ring，session 数量级（成百上千）远大于 cron job（个位~几十），200 槽常驻会内存放大。50 槽 × ~150B × N_session 更可控。

### 4.3 写盘契约（评审修订：异步）

- **Append 必须异步**（评审 blocking-2）：`osutil.WriteFileAtomic` 每次 = 2 fsync（`atomicfile.go:69,82`），同步执行会把 fsync 延迟注入用户对话返回路径。改为单 worker goroutine + 有界 channel（满则丢弃 + Warn，保持 best-effort）；进程 shutdown 时尽力 flush（可接受丢最后几条）。
- **不在 sendMu 持锁窗口内 Append**：埋点派发（向 channel 投递 payload）发生在 `ManagedSession.Send` 的 `defer s.sendMu.Unlock()`（`managed_send.go:137`）释放之后，或派发本身零阻塞（channel 非阻塞 send）。
- run 结束后投递（best-effort，失败仅 Warn 不阻塞）。原子写复用 cron 实现。
- **死 session 回收**：session reset/驱逐/移除时 `cacheInvalidate(hash(sessionKey))` 回收 ring entry（仿 cron `DeleteJob`→`cacheInvalidate`，`runstore.go:884`），防 sync.Map 无界增长。

## 5. Outcome 状态机

| 返回 | Outcome | 判定 |
|---|---|---|
| `err == nil` | `completed` | 正常返回 result |
| `errors.Is(err, cli.ErrTotalTimeout)` 或 `cli.ErrNoOutputTimeout` | `timeout` | `internal/cli/process.go:71-75` |
| `errors.Is(err, context.Canceled)` / Interrupt 触发 | `canceled` | 用户中断 |
| 其他 | `error` | 传输/进程错误 |

ErrorClass 类型复用 `runtelemetry.ErrorClass`（`state.go:64`，叶子包，session 可安全 import，含 `ErrClassDeadlineExceeded`/`ErrClassCanceled`）——**不引 `cron.ClassifyError`**（评审 blocking：那是 cron mutation-API→HTTP 状态码分类器 `error_class.go:105`，不处理 timeout/cancel，且会首次引入 production `session→cron` 反向依赖，`contract_test.go` 已禁止）。timeout sentinel 判定（`errors.Is(err, cli.ErrTotalTimeout/ErrNoOutputTimeout)`）留在 session 埋点处（session 本就依赖 `internal/cli`）；通用 ctx-error→ErrorClass 的映射可下沉 runtelemetry 新增 `ClassifyRunOutcome(err)`，或 runhistory 内置等价小函数（实现时择一，见 OQ-2 已决议）。

## 6. API

新增 `GET /api/sessions/runs?key=<sessionKey>&limit=&before=`（`internal/dashboard/session/handlers.go` + 注册于 `internal/server/routes.go:288`）：

```json
{
  "runs": [
    {"run_id":"…","outcome":"completed","started_at":1718…,"ended_at":1718…,
     "duration_ms":42100,"first_byte_ms":1200,"cost_usd":0.03,"error_class":""}
  ],
  "stats": {"count":12,"total_ms":…,"avg_ms":…,"p50_ms":…,"p95_ms":…,"max_ms":…,
            "completed_count":10,"error_count":1,"timeout_count":1}
}
```

鉴权走现有 `auth()` 中间件（与其他 `/api/sessions*` 一致）。

**Wiring（评审决议 OQ-3）**：session 版 runStore 挂在 **`session.Router`**（session 子系统 owner，`router_core.go:755`，已持 `storePath`），与 cron 让 `Scheduler` 持有 runStore 对称。
- 写入侧：`ManagedSession`（由 Router 创建）经 owner 引用/回调向 Router 的 store 投递。
- 读取侧：`dashsession.Deps.Router`（`handlers.go:1982`）**已注入**，给 Router 加导出读方法 `Router.SessionRuns(key, limit, before)` / `Router.SessionRunStats(key, n)`，handler 直接调——**无需新增 Deps 字段**，写读天然共享同一实例（挂 Handlers Deps 会导致写读两实例分裂，已否决）。

可选：`HandleList`（`handlers.go:489`）的每个 session 摘要附 `last_run{duration_ms, outcome}`，供列表卡片直接显示（不需额外请求）。

## 7. WebSocket 事件（开放问题 OQ-1）

两种方案，留待评审定：

- **方案 A（轻，倾向）**：不接 runtelemetry WS 广播。实时 elapsed 由前端基于已有的 `/api/sessions/events` SSE 流（用户正在看的 session）纯前端计算；run 结束后下一次 `/api/sessions/runs` 拉取或 list 摘要刷新历史。理由：普通 session run 频率远高于 cron，全局广播给所有 dashboard 客户端是噪音 + 扇出成本，而用户只关心自己正看的 session。
- **方案 B（重）**：扩展 `runtelemetry` 新增 `SubsystemSession`，并在 broadcaster 增加 sanitiser 分支（契约见 `internal/runtelemetry/event.go` 注释要求）+ server consumer 广播（仿 `BroadcastDaemonRunStarted/Ended`，`internal/server/consumer.go:100`）。仅当需要"侧边栏其他 session 的实时运行指示"时才值得。

默认按 A 实施，B 列为后续增量。

## 8. UI

严格复用设计系统 token（`dashboard.html:32-208`），**零 px 字面量**（`static_style_ratchet_test.go` ratchet baseline 30/24 只减不增）。明暗双主题靠 token 自动适配（`dashboard.html:201` light 覆盖）。

### 8.1 实时 / 上轮耗时

升级现有 ⏱ `.detail-turn-timer`（`dashboard.html:552`，`dashboard.js:2653`）：运行中显示实时 elapsed；结束按 outcome 着色（新增 `.ok`→`--nz-green` / `.err`/`.timeout`→`--nz-red` / `.run`→运行色）。
- **实时 tick 必须复刻 `ensureCronRunningTick`/`cronRunningTickPaintScoped`（`cron_view.js:1651/1623`）的三条件 stop guard**（评审 blocking：朴素 setInterval 会触发 timer-leak bug R220-FE-1）：仅 scoped 文本节点更新（不 innerHTML 重建），停止条件 = session 取消选中 / 非运行 / DOM 已移除；`prefers-reduced-motion` 降级静态。

### 8.2 run 历史时间轴

视觉同构复刻 cron `.cron-timeline-panel`（`dashboard.html:1818-1846`）为新块 `.session-runs-panel`。**注意（评审纠正）**：cron 时间轴已不在 mainShell（`dashboard.js:2691` 占位已移除，仅存于定时任务 drawer），故 session panel 是 **`main-header` 与 `.events` 之间的新 sibling 节点**，需新写 `sessionRunsState` + `renderSessionRunsPanel` + 行 builder（cron 的 `cronTimelineRowHtml` 等耦合 cron state，只作 copy-paste 模板，不能直接调用）。
- 行：状态圆点（ok/err/run-pulse/cancel）+ 状态名 + 开始时间（`formatAbsTime` 复用 + hover title）+ 右对齐耗时（`formatRunDuration` 复用 + `tabular-nums`）。子行放 outcome / 首字节 / cost（SessionRun 不存正文，无 detail body）。`:empty{display:none}` 自动隐藏；空但已加载给中文空态 hint+CTA（仿 `static_ux_contract_test.go` 的 cron 空态结构，并加对应断言 fragment 锁定）。
- **颜色 token**：running dot 用 `--nz-status-running`（琥珀，语义对齐）；canceled 紫色 cron 现用裸 hex `#a371f7`（无 token）——本 PR 顺手 tokenize 为 `--nz-purple` 而非复制第三处裸 hex（评审 non-blocking）。

### 8.3 汇总条

session 详情顶部一行极简 stat 胶囊：`count · 总时 · avg · P95 · max · ⚠超时数`，`--nz-fs-xs`/`--nz-text-mute`，超时数>0 才 `--nz-red`。

### 8.4 移动端（含上传场景）

断点矩阵已有（768/640/540/480 + `pointer:coarse`）。空间冲突已确认真实：移动端 `.input-area` 内的 `.file-preview`（`dashboard.html:768`，60px 缩略图）在排队图片时向上挤占 `.events` 高度，panel 插在 `.events` 之上会成为第三个争高度的固定块。
- `@media(max-width:768px)` 下 panel 默认**折叠为单行摘要**（复用原生 `<details>/<summary>` 的 `.doctor-panel`/`.doctor-summary` 折叠模式，`dashboard.html:557-566`，零 JS、可访问、▶/▼ 旋转），把 §8.3 汇总条与折叠头**合并**（如 `●12 · avg 8s · ⚠2`），点击展开。
- 展开高度用 `max-height:40vh` + `overflow-y:auto`（cron ≤640 用 50vh，session 留更多给 composer+preview）。
- **保持 in-flow，不引新 fixed/sticky 层**：`--nz-z-*`（`:136-141`）已被 lightbox/drawer/toast 占用，上传 spinner `.upload-status` 在 in-flow composer 内非 z-layer，in-flow 折叠彻底避开 z-index 冲突。
- `<summary>` 触控目标 ≥44px（`pointer:coarse`；`.doctor-summary` 现 `8px 12px`≈33px，需加 padding）。

## 9. 测试策略

- **runhistory store 单测**（`internal/session/runhistory`）：Append→List/Recent 顺序、GC keepCount/keepWindow 边界、负 duration 钳零、sessionKey→目录 hash 防路径遍历、并发 Append 原子性。照搬 cron runstore 测试模式。
- **埋点单测**（`internal/session`）：mock `processIface`，断言 completed/error/timeout/canceled 四种 Outcome 分类正确、FirstByteMS 仅记一次、SendPassthrough 路径也记录、注入假时钟断言 DurationMS。
- **stats 单测**：P50/P95 计算（含 1 条 / 偶数条 / 边界）、空集合返回零值。
- **API 测试**（`internal/dashboard/session`）：返回结构、stats 聚合、分页 before/limit、空 session、鉴权。
- **前端契约**：`go test ./internal/server/ -run 'StyleLiteralRatchet|UX'` 不破。
- **回归全量**：`go build ./...` + `go test ./...` + `go vet ./...`（internal/server -race 给 ≥480s timeout，见项目教训）。
- **手工 e2e**：部署后 dashboard 跑几轮对话，看实时计时 / 时间轴 / 汇总 / 移动端折叠 / 明暗主题。

## 10. 风险与回滚

- **风险**：埋点在 session 热路径（每轮对话），自测时间戳 + best-effort Append 必须零阻塞、零 panic（Append 失败仅 Warn）。错误分类引入 cron 依赖可能造成反向 import（OQ-2）。
- **回滚**：纯增量特性。store 写盘失败不影响对话；前端面板 `:empty` 自动隐藏。回滚 = revert PR，已落盘的 `session-runs/` 目录可留可手删，不影响运行。

## 11. 可观测性

- run 结束 Debug 日志一行：`session run done key=… dur=…ms outcome=…`。
- Append 失败 Warn 日志（含 sessionKey hash，不含正文）。
- 度量本身即为可观测性产物（dashboard 时间轴 + 汇总）。

## 12. 兼容与迁移

- **向后兼容**：新增 on-disk 目录 `session-runs/`，不触碰现有 session JSONL / cron runs。旧 session 无历史 → 面板空态自动隐藏。
- **迁移**：无。首次运行起累积。
- **配置**：keepCount/keepWindow 复用 cron 默认；如需独立可配置留待后续（本期硬编码默认）。

## 13. 实施分阶段

- **P1**：`runhistory` 包（SessionRun + store + GC + stats）+ 单测。
- **P2**：session 埋点 helper（Send/SendPassthrough 包裹 + Outcome 分类 + 写盘）+ 单测。
- **P3**：API `/api/sessions/runs` + list 摘要 + 测试。
- **P4**：前端时间轴 + 汇总 + 实时计时 + 移动端 + 契约测试。

四阶段可在同一 PR 分 commit，或 P1-P3（后端）先行 PR、P4（前端）随后——按 review 体量定。

## 开放问题（评审已决议）

- **OQ-1（WS 广播）✅ 决议：方案 A**（不接 runtelemetry WS 广播，前端基于已有 `/api/sessions/events` SSE 算实时 elapsed）。三方评审一致认可：session run 频率远高于 cron，全局扇出是噪音，且避开 runtelemetry 新增 Subsystem 的 `wire_stability_test`/`enum_complete_test` 维护成本。
- **OQ-2（ErrorClass）✅ 决议**：复用叶子包 `runtelemetry.ErrorClass`；timeout sentinel 判定留在 session 埋点；**不引 `cron.ClassifyError`**（错配语义 + 破坏依赖方向）。
- **OQ-3（store wiring）✅ 决议**：runStore 挂 `session.Router`（已被 `dashsession.Deps.Router` 注入），写读共享同一实例；store_root = `filepath.Dir(cfg.Session.StorePath)`。

## 评审签核

三方独立评审（架构 / 性能+并发 / 前端）已完成，verdict 均为"方向通过，blocking 项已纳入 v2"：
- 架构：OQ-2 复用目标纠正（runtelemetry 而非 cron）、store_root 基于 session 配置、store 挂 Router。
- 性能：删 index.json（cron 实际不写）、Append 异步、ring cap 200→50 + 死 session 回收。
- 前端：style ratchet 零余量（全 token）、timer 复刻三条件 stop guard、session panel 为新 sibling 节点、canceled 紫 tokenize。
无未解决分歧。
