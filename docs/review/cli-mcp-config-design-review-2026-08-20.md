# 设计评审：cli-mcp-config

- 被评审对象: `docs/rfc/cli-mcp-config.md`（Draft，2026-08-20）
- 阶段: Step 1c 设计阶段评审（编码前）
- 形式: **逐角度书面自评审**。本 session 有"未经用户请求不调用 Agent 工具"的约束，故未走 dev-workflow skill 首选的并行 agent team；按 skill 允许的第二档执行，逐角度书面留证如下。此形式弱于 agent team 评审 —— 已在 §结论 记录该残余风险。
- 结论: **有条件通过** —— 修完 F1–F8 后方可开始编码。

---

## 角度 1 — 架构、命名、抽象层级

**F1（P2，须修）— 必须明确该字段是 router 全局而非 per-backend，并在设计里写清理由。**

`config-local.yaml:58` 记录过一次真实踩坑：顶层 `cli.effort` 会被 `EnabledBackends` 传播到 claude backend，启动时报 "backend does not accept one" 警告后丢弃，所以那份配置刻意把 effort 放 backend 层。`cli.mcp_config` 若被同一传播机制吃到，会重演该告警。

判定：本 RFC 选的链路（`RouterConfig.MCPConfigFile` 单字段，仿 `NaozhiSettingsFile`）是 router 全局的，**不**经 `BackendModels`/`BackendExtraArgs`/`BackendEfforts` 那套 per-backend map，因此不受影响。但设计文档未点明这一点，实施者可能误照 `effort` 的样子放进 per-backend 结构。→ 设计 §8 须显式写"仿 `NaozhiSettingsFile` 的 router 全局单字段，**不**进 per-backend map"。

命名 `MCPConfigFile` / `cli.mcp_config` 与既有 `SettingsFile`/`DebugFile`、`--mcp-config` 对齐，无异议。

## 角度 2 — 测试充分性

**F2（P1，须修）— config 解析层零测试覆盖。**

设计把校验责任放在 `main.go` 的 resolve 函数（仿 `resolveNaozhiSettingsFile`），但实测确认 **`resolveNaozhiSettingsFile` 自己没有任何测试**（`grep resolveNaozhiSettingsFile cmd/naozhi/*_test.go` 空）。照抄这个先例等于照抄它的测试缺口 —— 而本字段的失败模式（配了却静默不生效）恰恰只能在这一层被捕获。

→ 必须新增 `TestResolveMCPConfigFile`，表驱动覆盖：空值 → `""`；相对路径 → `""`；`-` 前缀 → `""`；文件不存在 → `""`；合法绝对路径 → 原路径。这是**优于**先例而非等同先例。

**F3（P2，须修）— shim arg-drift 回归测试写成了条件句。**

设计 §4 写"现有 shim 测试若不覆盖，补一条断言"。G4 是明确的正确性契约（漏传会导致升级后重启所有存活会话），不该留在"若…则…"里。→ 改为无条件要求：先跑既有 `./internal/session/...` 确认是否已捕获，无论结果如何都要有一条显式钉住 `MCPConfigFile` 参与 drift 比较的断言。

## 角度 3 — 性能

无异议。新增开销为 `BuildArgs` 中一个 `if` + `filepath.IsAbs` + `strings.HasPrefix` + 两次 `append`，均无堆分配，调用频率为 per-spawn 与 per-shim-drift-check，量级可忽略。`BuildArgs` 不在事件热路径（热路径是 `ReadEvent`/`ReadEventInto`）。N/A。

## 角度 4 — 安全

**F4（P2，须修）— 只在文档里"要求 0600"，没有任何检测。**

MCP 配置文件可定义 stdio server（`command` + `args`），等价于"谁能写这个文件，谁就能让所有 cc 会话执行任意命令"。设计 §5 只写了"文件须 0600"，属于口头约定，无机制。仓库有硬化先例（`resolveCLIDebugDir` "creates + hardens the directory"）。

→ resolve 函数应 stat 文件并在 mode 含 group/other 写位时 `slog.Warn`（**不**失败 —— 权限判断在多用户场景可能有合理例外，硬失败会把可用性问题引入启动路径）。

**F5（P3，记录即可）— 一个应当被显式记为设计优点的性质。**

naozhi **从不读取**该文件内容，只把路径传给 cc。因此配置里的 `headers`（可能含 bearer token）永不进入 naozhi 进程内存，也就不可能经 naozhi 的任何 API / 日志 / eventlog 泄漏。这是本方案相对"naozhi 解析并转发 MCP 定义"的实质安全优势，值得写进设计而非隐含。

符号链接 / TOCTOU：路径为 operator 配置且由 cc 打开，naozhi 不参与读取，不额外防护 —— 作为已接受风险记录。

## 角度 5 — 可观测性

无异议。§6 的"绝不在看起来启用了的同时实际没生效"与 `resolveNaozhiSettingsFile` 的 F2 处理语义一致；不新增 metric、复用 cc 自报的 `system/init.mcp_servers[].status` 经 eventlog 落盘的既有通路，判断正确 —— 连接状态归 cc 所有，naozhi 另建计数器会产生第二真相源。只打路径不打内容亦正确。

## 角度 6 — 向后兼容与迁移

**F6（P3，须修）— `--mcp-config` 的双形态未说明。**

cc 的 `--mcp-config` 既接受文件路径，也接受内联 JSON 字符串。本设计的 `filepath.IsAbs` 校验会拒掉内联 JSON 形态。这是**有意**的（只支持文件，便于权限控制与 §F5 的不读取性质），但设计未言明，operator 若尝试内联写法会得到静默失效。→ 设计与 `config.example.yaml` 注释须明确"仅接受绝对文件路径，不支持内联 JSON"。

G2（空值时 argv 逐字节不变）与正交性两条契约均已有对应测试，无异议。

## 角度 7 — 失败模式与优雅降级

**F7（P1，须修）— 漏掉了一个可能影响全部会话可用性的失败模式。**

设计覆盖了"文件缺失"（→ MCP 关闭，会话正常）与"MCP server 不可达"（→ cc 报 `failed`，turn 正常），但**未覆盖"文件存在但 JSON 格式错误"**。此时 cc 的行为未知：若 cc 选择拒绝启动，则 MCP 文件里一个逗号打错会导致**每一个会话 spawn 失败**，即从"MCP 不可用"降级为"naozhi 完全不可用"。

这是本设计里唯一的潜在 P1 可用性风险，且成本极低即可证伪。→ §8 验收清单必须加一条：故意写坏 JSON，确认 cc 是否仍能 spawn。若确认会导致 spawn 失败，则 resolve 函数需在启动时做一次 `json.Unmarshal` 预校验（此时 naozhi 会短暂读取内容，与 F5 的性质冲突 —— 需在设计中权衡后明确取舍，建议只校验可解析性、解析结果立即丢弃、绝不落日志）。

## 角度 8 — 文档同步

**F8（P2，须修）— 三处文档需同步，其中一处是既有事实错误。**

1. `docs/design/DESIGN.md:1277` —— **既有内容与实测矛盾**。原文："hooks 隔离 — 已通过 `--setting-sources ""` 解决。插件 Stop hooks 不加载，**MCP/skills 正常**"。本 RFC §1 的实测矩阵证明 `--setting-sources ""` 下 MCP **完全不加载**（`mcp_servers: []`）。该断言须修正，否则后续读者会据此错误结论排查。同页 `:1088` 表格"完整 CC 工具 (需 `--setting-sources ""`)"亦受影响，至少需加注。
2. `CLAUDE.md:292` —— `cli` 字段清单（`backend`/`path`/`model`/`args`/`effort`）须补 `mcp_config`。
3. `config.example.yaml` 的 `cli:` 段 —— 须补注释掉的范例，并写明 F6 的"仅绝对文件路径"与 F4 的 0600 要求。

设计 §8 的触点清单（6 处）只列了代码，未含这三处文档。→ 补为第 7 触点。

---

## 结论

**有条件通过。** F1–F8 全部修入设计文档后方可开始编码；其中 **F2、F7 为 P1**，须在编码前落定（F7 尤其：它决定是否需要在 resolve 阶段做 JSON 预校验，会改变实现形状，不能留到编码后再返工）。

### 签核（2026-08-20，修订后复核）

F1–F8 全部已修入 `docs/rfc/cli-mcp-config.md`：

| 发现 | 处置 | 落点 |
|---|---|---|
| F1 router 全局 vs per-backend | 已修 | §8 触点 3 显式禁止进 per-backend map，附 `config-local.yaml:58` 踩坑引用 |
| F2 config 层零测试 | 已修 | 新增 §4.3 `TestResolveMCPConfigFile`，9 格表驱动 |
| F3 shim 回归写成条件句 | 已修 | 新增 §4.4，改为无条件要求 |
| F4 0600 仅口头约定 | 已修 | §5 改为 stat + `slog.Warn` 机制（不失败）；§4.3 加权限格 |
| F5 "从不读取"性质 | **已修正为更弱但准确的表述** | §7：G5 使该性质不再严格成立，降为"瞬时读信封、不下探 RawMessage、不驻留不记日志" |
| F6 内联 JSON 形态未说明 | 已修 | §7 + §8 触点 7 要求写入 example 注释 |
| F7 坏 JSON 失败模式未覆盖 | **已实测定案，升级为 P1 + 新增 G5** | 新增 §4.1 四格实测矩阵；§2 加 G5；§5 加 P1 行；§8 验收加第 2 项阻断门 |
| F8 三处文档未同步 | 已修 | §8 新增触点 7，含 DESIGN.md:1277 事实错误修正 |

F7 的处置改变了实现形状（新增启动期信封预校验），符合评审要求的"编码前落定"。**准予进入 Step 2 编码。**

一处评审自身的修正：F5 初稿把"naozhi 从不读取该文件"当作安全优点写进设计，而 F7 的缓解措施要求读取。二者冲突，已按"可用性优先、读取面最小化"取舍，并把设计里的表述改准 —— 不保留过强的原始说法。

### 残余风险

- 本次为自评审而非 agent team 并行评审，覆盖面弱于 skill 首选形式。本变更虽命中 Risk Checklist（CLI wrapper / session router 核心路径 + 安全面），但改动形状为"照既有 `SettingsFile` 链路增量复制"，有 6 处逐一比对过的先例可循，自评审的盲区风险因此收窄。若后续 review 阶段（Step 3 diff 评审）发现设计层问题，应回退本步骤重评。

---

# 实现评审（Step 3，diff 层）

- 被评审对象: 本 worktree 相对 `master` 的完整 diff（12 改 + 6 新增；代码 +153/-4，另 4 处文档 + 2 个新测试文件 + 2 篇文档）
- 阶段: dev-workflow Mandatory loop step 3 —— **开 PR 之前**的 diff 评审
- 形式: 同设计阶段 —— 逐角度书面自评审（session 约束：未经用户请求不调用 Agent 工具）。残余风险见 §结论
- 结论: **通过**（D1 已修入代码 + 测试，D2 已修入文档）

## 角度 1 — 架构、命名、抽象层级

链路与设计 §8 逐点一致：`config.CLIConfig.MCPConfig`（YAML）→ `resolveMCPConfigFile`（校验）→ `session.RouterConfig.MCPConfigFile` → `Router.mcpConfigFile`（私有，NewRouter 后不可变）→ `cli.SpawnOptions.MCPConfigFile` → `ClaudeProtocol.BuildArgs` 的一条 `if`。全程一个值、一个方向，无回写、无第二注入点。

F1 关注的 per-backend 传播确认未发生：`grep MCPConfigFile` 在 `BackendModels`/`BackendExtraArgs`/`BackendEfforts` 相关代码里零命中，`router_core.go:764` 的 godoc 已把"故意不进 per-backend map"及其理由（`cli.effort` 的告警踩坑）写死在类型旁。

抽象层级无越界：`internal/cli` 只做 argv 渲染与 argv 注入防护，不认识 YAML；校验全在 `cmd/naozhi`；`internal/config` 只声明字段不做 I/O。与 `SettingsFile` 的分层完全同构。

## 角度 2 — 测试充分性

新增两个测试文件 + 扩一个既有清单，覆盖三类失败：

1. argv 渲染（`internal/cli/protocol_claude_mcpconfig_test.go`，5 个）—— 默认不出现、绝对路径出现且恰好一次、守卫（相对/`-` 前缀）丢弃、与 `SettingsFile` 四组合正交、`--mcp-config` 仍在 `deniedExtraFlags`。
2. 配置解析（`cmd/naozhi/main_mcp_config_test.go`，13 + 1 子测试）—— 含 4 个 `G5:` 关键格与 2 个 `D1:` 格；`resolveMCPConfigFile` **函数级 100% 语句覆盖**（`go tool cover -func`）。
3. 参数漂移一致性（`internal/session/mcpconfig_drift_parity_test.go` 3 个 + `effort_drift_parity_test.go` 的 `required` 扩项）。

**两个 parity 测试做过变异验证**：分别从 `router_lifecycle.go` 和 `router_shim.go` 的 SpawnOptions 字面量里删掉该字段，对应测试各自变红，随后还原。二者均非空转测试。

一处**有意的非对称**记录在案：AST 层面的 `TestSpawnOptionsLiteral_CarriesMCPConfigFile` 之所以必要，是因为删掉结构体字面量里的一个字段既能编译、也能通过所有行为测试 —— 配置会静默失效而无任何测试变红。这是照 `Effort` 的先例。

未覆盖且判定为合理：`internal/config` 层无新测试（该字段只是一个 `yaml` tag，无逻辑）；ACP/codex 忽略该字段无专门测试（由"只有 `ClaudeProtocol.BuildArgs` 引用 `MCPConfigFile`"这一事实保证，`grep` 可验）。

## 角度 3 — 性能

热路径零影响：`BuildArgs` 新增一次字符串比较 + 两个廉价守卫，仅在 spawn 时执行（非 per-event、非 per-request）。文件 I/O 全在启动期一次，**未**进 spawn 路径，也未进任何持锁区间。`Router.mcpConfigFile` 在 `NewRouter` 后只读，故 spawn 与 drift 两处读取都不需要额外同步。无新分配进入 per-event 路径。

## 角度 4 — 安全

- `deniedExtraFlags` **未放宽**，且由 `TestBuildArgs_MCPConfigStillDeniedInExtraArgs` 钉住 —— 走的是 godoc 明示的"专用字段"逃生口，值只可能来自 operator 的 config.yaml，不可能来自 prompt / agent args / IM 消息。
- argv 注入：绝对路径 + 非 `-` 前缀双守卫，且在 cmd 与 BuildArgs **两层**各做一次（defence in depth）。
- 日志泄露：`json.Unmarshal` 的 err **故意不入日志** —— JSON 语法/类型错误消息会引用出错位置附近的原始字节，远程 server 的 `headers` 里可能带 bearer token。信封解析停在 `map[string]json.RawMessage`，不下探；解析结果不驻留。已在代码注释里写明这是有意为之，避免后人"顺手补上 err 字段"。
- 权限：group/other 可写时 `slog.Warn`（不失败）—— 理由已写进代码与 §5。
- 残余：cc 本就带 Bash + `--dangerously-skip-permissions`，故此面是"持久化通道"而非新增"代码执行"面，与 `direct-user-settings.md:128` 同构。

**D1（P2，本轮发现，已修）**—— 可用性/健壮性缺口：原实现直接 `os.ReadFile`。若路径指向 FIFO 或字符设备，`ReadFile` 永不返回，**naozhi 启动挂死**；这比 G5 要防的"spawn 硬失败"更糟，且发生在信封校验之前，G5 的防线根本来不及生效。多 GB 文件则会被整读进内存。
→ 已修：改为先 `os.Stat`，非 regular file 直接拒、超 1 MiB 直接拒（合法信封只有几 KiB），并复用同一个 `FileInfo` 做权限检查，相比原形状**不增加** syscall。新增 `D1: non-regular file rejected` / `D1: oversized file rejected` 两格（目录代表 non-regular 类以保持跨平台可编译 —— `syscall.Mkfifo` 在 CI 的 windows 构建门下不可用）。

## 角度 5 — 可观测性

三条启用/失败路径都能只靠日志定位：成功 `slog.Info("mcp config: enabled", path, servers=N)`（含 server 数量，一眼看出是否读到了预期条目）；每条拒绝路径一条 `slog.Error` 且各自措辞不同（不可 stat / 非常规文件 / 过大 / 不可读 / 非 JSON 对象或缺 `mcpServers` / 缺 `mcpServers` 键 / 相对路径 / `-` 前缀），足以区分成因；权限问题 `slog.Warn`。

关键原则已守住：**绝不出现"看起来启用了但实际没生效"的静默态** —— 置空必伴随 Error 日志。运行期效果由 cc 自己的 `system/init.mcp_servers` 与新发现的 `mcp_server_errors[]` 报告，naozhi 不重复实现。

一处措辞是本轮之前修过的：`{"mcpServers":[]}`（合法 JSON、形状不对）曾被报成 "not valid JSON"，已改为形状中立的表述。该错报是**自己写的测试**发现的。

## 角度 6 — 向后兼容与迁移

零默认行为变更：`cli.mcp_config` 未配置时 `MCPConfigFile == ""`，`BuildArgs` 完全不追加 flag → **argv 与今日逐字节相同**。`yaml:"mcp_config,omitempty"` 新增字段，旧 config 照常加载；新 config 被旧 binary 读到时 YAML 解析忽略未知键（与仓库既有做法一致）。无磁盘格式变更、无迁移、无 wire 协议变更。ACP/codex 后端未受影响。

**一处刻意保留的行为**：operator 首次打开 `cli.mcp_config` 后的第一次重启，所有存活 shim 会被判为 arg-drift 并在下一条消息时重启。这是**正确且必须**的 —— 那些进程确实没有 MCP，不重启就拿不到。G4 的 mirror 防的是**其后每一次**重启的误判，不是这一次。已确认非缺陷。

## 角度 7 — 失败模式与优雅降级

设计的核心不变量（G5）在 diff 层成立：`resolveMCPConfigFile` 的每条失败路径都 `return ""`，没有任何一条会把可疑路径交给 router。因为 cc 在 `--mcp-config` 坏掉时**拒绝启动**（实测：无 `system/init`），这一点是"MCP 不可用"与"naozhi 不可用"之间唯一的闸门。已由 4 个 `G5:` 子测试 + 2 个 `D1:` 子测试钉住。

分层退化链完整：文件坏 → 无 MCP（naozhi 正常）；单个 server 定义坏 → cc 正常启动并经 `mcp_server_errors[]` 上报（naozhi 有意不插手）；OAuth 过期 → 该 server 不可用，会话其余功能不受影响。

**D2（P2，本轮发现，已修）**—— 信封校验只在启动时做一次，之后会过期：naozhi 运行期间 operator 把该文件改坏，G5 的防线已经跑完，下一次 spawn 重新落回 P1 情形。
→ 判定**不做** per-spawn 复检：一是把文件 I/O 塞进 spawn 路径，二是根本关不掉竞态（cc 自己读文件必然发生在 naozhi 校验之后，中间永远有窗口）。已改为流程性缓解 + 显式记录：`config.example.yaml` 加 "RESTART NAOZHI AFTER EDITING THIS FILE" 段并说明原因，§5 风险表加一行。失败是响的（spawn 失败 + cc 自己的报错），非静默降级。

## 角度 8 — 文档同步

四处已同步：`CLAUDE.md:292` 的 `cli` 字段清单加 `mcp_config`；`config.example.yaml` 加带完整 operator 指引的注释块（何时需要 / 格式 / 只支持文件不支持内联 JSON / chmod 600 / 校验理由 / OAuth 不由 naozhi 管 / 改完要重启）；`docs/rfc/README.md` 加索引行；`docs/design/DESIGN.md:1277` **修正一处既有事实错误**（原文称 `--setting-sources ""` 下 "MCP/skills 正常"，实测 MCP 并不正常）并标注 `:1088` 表格行。

按 dev-workflow 约定，本文件即评审留证（`docs/review/`）。本轮 D1/D2 均为可直接落地的修复，无需走 `triage-findings` 分流到 issue / cosmetic-backlog。

---

## 结论（实现评审）

**通过。** 本轮两个发现均已在 worktree 内闭环：

| 发现 | 级别 | 处置 | 落点 |
|---|---|---|---|
| D1 `os.ReadFile` 可能挂死启动（FIFO/设备），且无尺寸上限 | P2 | **已修（代码 + 测试）** | `main_mcp_config.go` 改为 stat-first + regular-file + 1 MiB 上限；新增 2 个 `D1:` 子测试；§5 加风险行 |
| D2 启动期校验会过期，运行期改坏文件重开 P1 窗口 | P2 | **已修（文档；有意不做 per-spawn 复检）** | `config.example.yaml` 加重启要求 + 原因；§5 加风险行 |

设计阶段的 F1–F8 逐条在 diff 中复核落地，无回退、无遗漏。门禁状态：`go build` / `go vet` / `go test ./...`（79 包全绿）通过；改动包覆盖率无下降，新函数 100%。

### 残余风险

- 同设计阶段：本轮为书面自评审而非 agent team 并行评审。缓解同前 —— 改动形状是照 `SettingsFile` 既有链路增量复制，6 处触点逐一比对过先例；且本轮自评审确实产出了两个此前未识别的 P2 发现（其中 D1 需改代码），说明该形式并非空转。
- **未经端到端验证的部分**：Asana OAuth 尚未完成，故"`~/.claude.json` 的 `mcpOAuth` token 在 `--setting-sources ""` 下是否可见"这一问题仍未定案（设计 §7 已记为 open question，正向信号是实测拿到 `needs-auth` 而非 `failed`）。若结论为不可见，回退方案是 §3 备选 E（`npx -y mcp-remote` stdio 包装）——**该分支不改本 PR 的任何代码**，只换 MCP 配置文件内容。
