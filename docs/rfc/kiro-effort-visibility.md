# RFC: kiro effort 可见性（dashboard 会话头部）

- 状态：Draft v1
- 日期：2026-08-01
- 作者：Kevin Zhao
- 范围：把 kiro backend 每轮上报的 `effort` 透出到 dashboard 对话窗体顶部
- 关联：`multi-backend.md` §8.8（normalize 层）、`multi-backend-validation.md`

## 1. Background & problem

kiro CLI 的 thinking effort 是一个**对输出质量与计费都有实质影响**的档位
（`low / medium / high / xhigh / max`），但 naozhi 目前对它完全无感知：

- 全仓 grep `effort` 只命中 `best-effort` 注释；`ACPProtocol.BuildArgs`
  (`internal/cli/protocol_acp.go:250`) 只传 `--model`，从不传 `--effort`。
- 因此 naozhi 起的 kiro session 的 effort **完全继承 kiro 自己的全局默认**
  （`~/.kiro/settings/cli.json` 的 `chat.modelDefaults[<model>].output_config.effort`），
  这是一条对操作者不可见的隐式通道。

### 实测证据（2026-08-01，kiro-cli 2.16.0）

手工跑 ACP 握手 + 一轮 `session/prompt`，kiro 每轮都在 `_kiro.dev/metadata`
通知里上报 effort：

```
{"jsonrpc":"2.0","method":"_kiro.dev/metadata","params":{
  "sessionId":"4f81ac85-...","contextUsagePercentage":12.94,"effort":"max"}}
{"jsonrpc":"2.0","method":"_kiro.dev/metadata","params":{
  "sessionId":"4f81ac85-...","contextUsagePercentage":12.94,
  "meteringUsage":[{"value":1.80,"unit":"credit","unitPlural":"credits"}],
  "turnDurationMs":6389,"effort":"max"}}
{"jsonrpc":"2.0","result":{"stopReason":"end_turn"},"id":3}
```

落盘的 `~/.kiro/sessions/cli/<sid>.json` 同步记录
`additional_fields.overrides.output_config.effort`。

**naozhi 收到了这个字段但丢掉了**：`kiroMetadataParams`
(`protocol_acp.go:1073-1078`) 解析了 `contextUsagePercentage` /
`turnDurationMs` / `meteringUsage`，唯独没有 `effort`。

### 症状

操作者在 dashboard 上无法回答"这个 session 现在跑在哪个 effort 档位"。实测
中同一台机器上历史 session 的 effort 实际是**混杂**的（一批 `max`、一批
`xhigh`，取决于当时的全局默认以及是否在交互式 TUI 里用过 `/effort`），而
dashboard 对此零显示。这直接影响两类判断：

1. 排查"这轮为什么特别慢/特别贵"时，effort 是一个一级解释变量
   （与已展示的 turn timer、metering credits 同源同帧）。
2. 确认一次配置变更（改 `chat.modelDefaults`）是否真的对 naozhi 侧生效。

## 2. Goals & non-goals

### Goals

- G1：`_kiro.dev/metadata.effort` 解析进 normalize 层，端到端到达
  `SessionSnapshot`，随 `/api/sessions` 透出。
- G2：dashboard 对话窗体顶部（`.main-header .detail`）显示当前 effort，
  仅在后端真实上报时出现；未上报（claude / codex / 尚未跑过一轮）不占位。
- G3：不上报 effort 的 backend 与旧版 kiro 保持零行为变化。

### Non-goals

- NG1：**不**新增 `--effort` 传参 / `cli.backends[].effort` 配置。本 RFC 只做
  "可见"，不做"可控"。理由见 §3 备选方案 C —— 控制面涉及配置校验、
  per-agent 覆盖与 arg-drift 同步，混进来会让本次 diff 失去可评审性。
  **后续 RFC 见 `kiro-effort-control.md`**（已确认要做，范围限定为纯配置驱动：
  不做 per-session 覆盖、不做运行时改档）。
- NG2：不改 kiro 的全局设置读取逻辑，也不试图在 naozhi 侧镜像
  `~/.kiro/settings/cli.json`。
- NG3：不为 claude / codex 合成或估算 effort 值。codex 的 wire 里目前没有
  等价字段（已 grep 确认无 `reasoning_effort`），claude stream-json 同理。
- NG4：不做 effort 的历史留存 / 趋势图。只显示"当前"。

## 3. Alternatives considered

### 方案 A（选定）：normalize 层新增字段 + header 同步渲染

沿用 `turn_duration_ms` 已经走通的整条链路：
`_kiro.dev/metadata` → `EventMetadata` → `Process` atomic → `SessionSnapshot`
→ `/api/sessions` → `renderMainShell` 内联构建 chip。

优点：
- 与 `turn_duration_ms` **同帧、同生命周期、同渲染位置**，是最小的概念增量。
- REST / WS 层零改动（`SessionSnapshot` 整体 marshal，WS 只 kick refetch）。
- 传输链路已被生产验证：turn timer 就在旁边正常渲染，说明这一帧能可靠到达前端。

缺点：新增一个 `processIface` 方法，会打断三个测试 fake（编译期可见，非隐患）。

### 方案 B：只落到 `/debug/vars` 或 doctor，不进 dashboard

优点：diff 更小，完全避开 header 视觉预算之争（见 §5 R3）。

缺点：**不解决用户提出的问题**。用户明确要求"在对话窗体的顶部显示"。且
effort 是 per-session 的运行时状态，expvar 的进程级计数器语义不匹配。

**否决。**

### 方案 C：同时实现 effort 的读与写（`--effort` 传参 + dashboard 选择器）

优点：一次性把隐式通道变成显式配置，操作者不必再去改 kiro 全局设置。

缺点：范围膨胀到需要配置 schema 校验、per-agent/per-backend 覆盖优先级、
ACP 会话中途改档的协议支持调研（`/effort` 是 TUI slash command，ACP 侧是否
有等价 RPC 尚未验证）。与"显示"这件事的风险等级完全不同，且会让本 PR 无法
被独立评审与回滚。

**否决 —— 但记录为后续 RFC 的起点。** 本 RFC 的 §1 实测已经证明
`kiro-cli acp --effort <level>` 这个入口存在（`kiro-cli acp --help` 可见），
为后续实现留好了事实基础。

## 4. Test strategy

| 层 | 测试 | 参照 |
|---|---|---|
| 协议解析 | `parseKiroMetadata` 解出 `effort`；缺失字段→空串不报错；schema drift 跳过 | `internal/cli/cli_test.go:1155-1220` |
| Process 状态 | `Effort()` 零值为 `""`；`applyMetadata` 存取；**空串不回退已存值**；`applyMetadata(nil)` no-op | `cli_test.go:1281-1358`（尤其 :1350-1358 的不回退断言） |
| Snapshot | 有 live process 时填充；无 process 时为零值 | `internal/session/snapshot_normalize_test.go:34-84` |
| API wire tag | **`json.Marshal(SessionSnapshot)` 产出 `"effort":"max"`；空值 omitempty 不出现 key** | 新增 `TestSnapshot_EffortWireTag`。初稿以为 `sessions_shape_test.go:38` 覆盖了这点 —— **不成立**：该测试只断言 required 集合 `{key,state,last_active,session_id}`，无 allowlist。把 tag 写成 `effort_tier` 会编译通过、全部 Go 测试通过、前端静默读到 undefined |
| 事件接缝 | `dispatchProtocolEvent` 把 metadata 帧的 effort 应用到 Process 且不下发 eventCh | 新增；照 `process_live_version_test.go:25` |
| 前端结构契约 | chip builder 存在、class 命名、非空 gating、无值不渲染 | `internal/server/static_git_chip_contract_test.go`、`static_queue_chip_test.go` |
| **前端 wiring（R1b）** | **挂载点 id 唯一出现；`turnCompleted` 分支调用 `setHeaderEffortChip`；`renderMainShell` 末尾重绘** | `static_git_chip_contract_test.go:41`（`if (turnCompleted) invalidateGitState(...)` 那条断言的同构版本） |
| 样式护栏 | 只用 `--nz-*` token，不出现裸 `font-size:Npx` / `#hex` | `static_style_ratchet_test.go:39`、`static_light_theme_parity_test.go` |

**并发/失败路径**：§1 实测显示 kiro **一轮发两帧** metadata（第一帧无
`meteringUsage`），所以必须处理**帧间语义**：

- effort 是**覆盖**语义（新值替换旧值），与 `MeteringUsage` 的**累加**语义相反
  （`process.go:895` 起的 per-unit 累加）。照抄 metering 的心智会写错，须专门
  测中途变档 `xhigh → max` 覆盖成功。
- 空串 guard（`if m.Effort != ""`）有**真实触发场景**：若某版本第二帧省略
  `effort`，无 guard 会把已显示的档位抹空、chip 闪掉。

`Effort()` 必须 lock-free 以满足 `Snapshot` 的 O(1) 契约
（`managed_query.go:127-136`：1Hz × N tabs × M sessions）。

**存 string 而非枚举 int 的理由**：effort 虽是 5 值闭集（可用 `atomic.Int32` +
常量表，更省），但 R4 要求 kiro 未来改字段时 log-and-skip 而非 error ——
naozhi 不该硬编码枚举白名单，存原始 string 才能透传未知新档位。用
`atomic.Pointer[string]`，形态照抄 `Process.liveVersion`（`process.go:284`，
同样是"运行中由事件帧填充的 string"，比 spawn 时一次性设定的 `model` 更贴合）。
**nil pointer → 返回 `""`**（与 `Model()` `process.go:960-965`、`LiveVersion()`
`process.go:996-1001` 一致）—— 这是空串 guard 与前端非空 gating 的共同前提。

**回归证据**：新增测试须在旧代码上失败——`parseKiroMetadata` 的 effort 断言
在未加字段时必然失败（字段不存在，编译不过 / 值为空），满足 Bucket A 的
测试充分性要求。

## 5. Risk & rollback

| # | 风险 | 评估 | 缓解 |
|---|---|---|---|
| R1 | ETag/304 遮蔽 effort 变化 | **已排除，但理由与初稿不同**。前端**从不发送 `If-None-Match`**（`dashboard.js` 全文无该 header），304 路径对 dashboard 根本不激活，`sessionsListETag` 只服务于未来的 API 消费者 | 无需缓解 |
| **R1b** | **（真实缺陷）turn 结束后 header 不重绘，chip 要切走再切回才出现** | **阻断级**。三条独立证据：① `notifyChange`（`router_core.go:1382-1386`）只调回调，**不碰 gen** —— 全部 `gen.Add(1)` 站点都在 session map 增删改路径；② 前端 `fetchSessions` 在 `dashboard.js:487` 按 `version === lastVersion` 短路；③ 即使不短路，`fetchSessions` 末尾只调 `updateHeaderCLI()`，而它（`dashboard.js:11891-11902`）**只重写 `.detail-left`**。`renderMainShell()` 的 5 个调用点（2485/2956/7566/7610/7670）全是 selectSession / rename / 新建，**无一在 turn 完成路径上**。`dashboard.js:11745-11747` 的注释直接印证："storeGen doesn't increment on process state transitions"。**推论：现有 turn-timer chip 同样有此问题**——它也只由 `renderMainShell` 生成 | **必须走挂载点 + turnCompleted 重绘**，见 §5.1。照抄 turn-timer 的内联渲染会继承同一静默失效，恰好是 R3 承诺要避免的"有数据却与无数据视觉同形" |
| R2 | `processIface` 新方法打断三个 fake | 编译期失败，不是运行时隐患 | 同 PR 内补齐 `testutil.go:142`、`router_test.go:247`、`snapshot_normalize_test.go:117` |
| R3 | **header 视觉预算**——R5-2 移过 backend chip、R5-7 移过 ctx-bar，理由是"低信号、争夺注意力" | effort 与被移除的两者不同。ctx-bar 的问题是"<5% 与无数据视觉同形"（图形条编码）；effort 是**离散枚举**（5 档字面量，无歧义）。**与 backend chip 的决定性区别**：R5-2 移除它的理由是 "cliLabel already names the backend — duplicate signal"（`dashboard.js:3149-3152`），而 effort 在 header 里**无任何冗余源**。归无框族（见 §5.1），不引入第三个带框 chip | 只在有值时渲染；沿用 `.model-label` 的 `--nz-fs-sm` 无框样式；除 max/xhigh 轻微强调外不加颜色 |
| R4 | kiro 未来重命名/移除 `effort` 字段 | 现有 log-and-skip 契约已覆盖：解析失败不 error，仅 `slog.Warn` | 无值时 chip 自动消失，无残留 |
| **R4b** | **`effort` 字段类型漂移会连带丢弃整帧**（评审发现，初稿 R4 低估） | 初稿把 `Effort` 解成 `string`。若 kiro 改成对象（它在自己 settings 里**本就是** `output_config.effort` 嵌套对象）/数组/数字，whole-struct `json.Unmarshal` 报 `UnmarshalTypeError` → 走 log-and-skip → **返回零 Event**，于是同帧的 `contextUsagePercentage` / `turnDurationMs` / `meteringUsage` 全部被丢弃。实测确认：Go 解码器其实已把其他字段填好，是代码在 `err != nil` 时扔掉了整个 `raw`。后果不是"chip 消失"，而是**一个装饰性新字段静默清零已上线的成本/上下文/耗时显示** | 改用 `json.RawMessage` + `effortFromRaw()`：仅接受 JSON string，其他形状降级为 `""` 而保住整帧。5 种坏形状的表驱动回归测试，并实测确认改回 `string` 后该测试失败 |
| **R4c** | tier 长度无上界（进程控制的字符串，被 Process 长期持有并 1Hz×N tab 反复 marshal） | `maxEffortRunes = 32` 截断（无省略号 —— tier 是标识符）。同 `maxMeteringUnits` 的既有惯例 |
| R5 | e2e `multibackend.round5.test.js:265-292` 断言 header 无 `.ctx-bar` | 新 chip 用不同 class，不触发该断言 | 不复用 `.ctx-bar*` 那套死 CSS，避免语义混淆 |

### 5.1 渲染方案（针对 R1b）

**不能**照抄 turn-timer 的内联渲染。采用 git-chip 已经解决过同一问题的形态
（`dashboard.js:2723-2735`），且比它更简单 —— effort 已在 `/api/sessions`
响应里，不需要额外端点、不需要缓存层：

1. `renderMainShell` 只建**空挂载点** `<span class="detail-effort" id="header-effort"></span>`
   （CSS `:empty{display:none}` 折叠，与 `.detail-git` / `.detail-runstats` 同构）。
2. `effortTagHtml(effort)` 纯函数：无值 → `''`。
3. `setHeaderEffortChip()` 从 `sessionsData` 读当前选中会话的 effort 并写入挂载点。
4. 两个调用时机：
   - `renderMainShell` 末尾（与 `repaintGitChip()` 并列）—— 覆盖 rename / 重选
     导致的 header 重建，避免 chip 闪掉。
   - **`fetchSessions` 末尾**，紧邻已有的 `if (selectedKey) updateHeaderCLI();`
     （`dashboard.js:667`）—— **这是修复 R1b 的关键**。5s 轮询是 effort 到达
     前端的唯一通道，在这里重绘即可，无需依赖 `renderMainShell`。

**为何不改 `updateHeaderCLI`**：它的职责是"重写 `.detail-left`（cli 名 + 版本）"
（`dashboard.js:11891-11902`）。把 effort 塞进去会让它同时负责两个互不相关的
DOM 区域。独立挂载点 + 独立 setter 是 `.detail-git` / `.detail-runstats` 已经
用了两次的既成模式，零布局风险（不动 `.detail-left` 的 flex 结构）。

**为何不并入 `.model-label`**：前端评审建议把 effort 作为 model 的后缀（理由：
kiro 侧 effort 就挂在 `modelDefaults[<model>]` 下，且净增 0 个视觉元素）。这个
语义论证成立，但落地要把 `modelLabel` 移出 `.detail-left`（否则被
`updateHeaderCLI` 的 `el.innerHTML = label` 擦掉），而 `.detail` 是
`display:flex; gap:8px`，移出后 `.model-label` 的 `margin-left:6px` 会与 gap
叠加成 14px，改变现有间距。**用独立挂载点换取零布局风险**；视觉上仍归无框族
（见下），所以并不会产生"第 7 个带框 chip"的重量问题。

**视觉归族（修正初稿）**：归 `.detail-turn-timer` / `.model-label` 那族
（**无框无底**的行内元信息，`dashboard.html:667` / `:1442`），**不用**
`.git-chip` 的带框形态。初稿说"取 `.git-chip` 的克制形态"是错误归族：
`.detail` 行已有 `.sc-origin` + `.git-chip` 两个带框 chip，第三个会盖过
`.detail-left` 的主标签。附带硬约束：`border-radius` 字面量 ratchet
**已顶格 24/24**（`static_style_ratchet_test.go:24`），带框路线容错为零。

**文案**：保留 kiro 的**英文小写字面量**（`low/medium/high/xhigh/max`），
中文只进 tooltip。理由：这 5 个 token 在 `/effort` 命令、
`~/.kiro/settings/cli.json`、`kiro-cli acp --effort` 里完全一致，翻译成
"低/中/高/极高/最高"会切断操作者从 dashboard 读数映射到 kiro 配置的路径。
同族先例：`modelLabel` 也是原样英文（`dashboard.js:3145-3147`）。

**不用图标**：5 档是**有序量**，emoji / `▲▲▲` 都需图例解码。`.git-chip-icon`
（`⑂`）能用是因为它编码**类型**而非量级。

**max/xhigh 轻微强调**：仅去掉 mute 降级（`--nz-text-mute` → `--nz-text`）
+ `font-weight:600`。**不加颜色** —— 高 effort 不是错误状态，涂色会误读成告警；
且 `--nz-amber` 在本项目已被 `--nz-status-running` 占用为"运行中"语义。

**转义（必须）**：`s.effort` 是 kiro 进程通过 JSON-RPC 通知控制的字符串，
与被 `static_git_chip_contract_test.go:70-108` 专门盯防的 git branch 同级信任度。
正文用 `esc()`，`title` 属性用 `escAttr()`。

**未知档位不白名单 gate**：kiro 若新增第 6 档，硬白名单会让 chip 静默消失，
违背 R4 的 forward-compat 契约。未知值原样显示、只是不加强调 —— 与
`backendChipInfo` 对 orphan backend 的既定处理一致（`dashboard.js:1821-1826`：
"Don't return null — losing the chip silently would hide a real state"）。

**a11y**：沿用 `.detail` 行的既定约定 —— `title` 承载全量信息，**不加
`aria-label`**（`originBadgeHtml` `dashboard.js:1806`、`gitChipHtml` `:2708` 皆如此）。
可见文本 `max` 本身有意义，`aria-label` 会**覆盖**它，造成视觉与听觉内容不一致。

**布局（实现期实测补充）**：`.detail` 是 `justify-content:space-between`
（`dashboard.html:635`），所以挂载点必须带 `margin-right:auto`，否则 tag 会被
甩到 header 水平**正中**、与它所修饰的 model 标签脱开。这是截图验证才发现的
——所有功能断言（存在、文本、强调、消失）在 tag 位于中间时**全部照样通过**。
已加 `effort_tag.test.js` 的 boundingBox 断言防回归，并实测确认移除
`margin-right:auto` 后该断言会失败。

**Rollback**：纯增量，无迁移、无磁盘格式变更、无配置项。`git revert` 单个
commit 即可完全回退；回退后 API 少一个 `omitempty` 字段，前端少一个 chip，
其余行为不变。旧版 naozhi 读新版数据无影响（字段仅在内存与 API 响应里，
不落盘 `sessions.json`）。

## 6. Observability

- effort 随 `/api/sessions` 返回，`naozhi doctor` 与任何 API 消费者自动可见。
- 解析失败沿用 `parseKiroMetadata` 现有的 `slog.Warn("acp: _kiro.dev/metadata
  unmarshal failed")`，不新增日志噪声。
- **不新增 metrics**：effort 是低频（每轮一次）的枚举状态，不是速率或延迟，
  expvar 计数器语义不匹配。如后续需要"各档位占用比例"，应在 runhistory
  层做，而非进程计数器。

## 7. Compatibility & migration

- **API**：`SessionSnapshot` 新增 `effort` 字段，带 `omitempty` —— 纯增量。
  `sessions_shape_test.go` 只强制 *required* 集合，附加字段天然通过。
- **磁盘格式**：无变更。effort 不写入 `sessions.json`，也不进 event log
  （metadata 事件在 `process_readloop.go:689-692` 就被 `return false` 吞掉，
  不入 `eventCh`/`EventLog`）。故重启后 effort 归零，直到下一轮 metadata 到达
  —— 与 `turn_duration_ms` 行为一致，符合"当前状态"语义。
- **配置**：无新增配置项。
- **多节点**：两条远端路径（`node/httpclient.go:155`、`node/reverseconn.go:275`）
  中途都降级成 `[]map[string]any` —— **恰恰因为是无类型 map**，未知字段自然穿过，
  无需 capability 协商。旧版节点不返回该字段 → 前端非空 gating → chip 不显示，
  优雅降级。（注意这里**没有**类型契约保护，是 map 透传。）

## 8. Rollout plan

单 PR 全量落地，无 feature flag。理由：纯增量只读字段 + 条件渲染的 UI chip，
无状态迁移、无破坏面，flag 的维护成本高于其收益（且 flag 需要配套移除计划，
见 dev-workflow 的 anti-pattern 清单）。

1. PR 合入 master。
2. UI-touching 变更，tag 前本地跑 `make release-gate`。
3. tag `vX.Y.Z`（minor bump —— 向后兼容的新功能）。
4. 各 host `naozhi upgrade`。

## 9. 实现清单（依赖序）

1. `internal/cli/event.go` — `EventMetadata.Effort string`（~:121）
2. `internal/cli/protocol_acp.go` — `kiroMetadataParams.Effort`（~:1077）+
   `parseKiroMetadata` 映射（~:1089）
3. `internal/cli/process.go` — `effort atomic.Pointer[string]`（~:246）、
   `Effort()` accessor（~:850）、`applyMetadata` 非空 guard（~:894）
4. `internal/session/managed.go` — `processIface.Effort()`（~:265）+
   `SessionSnapshot.Effort`（~:699）
5. `internal/session/managed_query.go` — `snap.Effort = proc.Effort()`（~:312，
   置于 `proc != nil` 分支内：无进程即无当前 effort）
6. 三个测试 fake 补方法
7. `internal/server/static/dashboard.js` — `effortChipHtml` 构建（~:3167）+
   插入 `main.innerHTML`（~:3208，紧邻 turnTimerHtml）
8. `internal/server/static/dashboard.html` — CSS，仅用 `--nz-*` token
