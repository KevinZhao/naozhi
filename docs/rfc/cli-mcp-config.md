# RFC: `cli.mcp_config` — 给 naozhi spawn 的 CLI 挂载 operator 指定的 MCP server 集

- 状态: Draft
- 日期: 2026-08-20
- 范围: 新增 `cli.mcp_config` 配置项 → `SpawnOptions.MCPConfigFile` → `ClaudeProtocol.BuildArgs` 渲染 `--mcp-config <file>`。使启用了 `naozhi_settings.enabled`（隔离 settings）的部署仍能给 cc 子进程挂 MCP server。

## 1. Background & problem

### 可复现症状

本机 naozhi（`config-local.yaml:11` 开启 `naozhi_settings.enabled: true`）内的 cc 会话**看不到任何 MCP server**，而命令行 cc 能看到 `aws-sentral-mcp` + `playwright`。

根因链（已实测，cc `2.1.235.672`）：

1. `naozhi_settings.enabled: true` → `resolveNaozhiSettingsFile`（`cmd/naozhi/main_claude_settings.go:302`）返回 `~/.naozhi/naozhi-settings.json`
2. → `RouterConfig.NaozhiSettingsFile` → `SpawnOptions.SettingsFile`
3. → `BuildArgs`（`internal/cli/protocol_claude.go:117-118`）走 `--setting-sources "" --settings <file>` 分支
4. `--setting-sources ""` 关掉**所有**外部配置源，其中包含 `~/.claude.json` 的 `mcpServers` 段

实测对照矩阵（同一 cc 版本、同一份 `naozhi-settings.json`，读 `system/init` 事件的 `mcp_servers`）：

| 启动方式 | `mcp_servers` |
|---|---|
| `--setting-sources user`（naozhi 默认路径 / 命令行 cc 等价） | `aws-sentral-mcp`, `playwright` ✅ |
| `--setting-sources "" --settings <naozhi file>`（**本机现状**） | `[]` ❌ |
| 同上 + 在 `naozhi-settings.json` 里写 `mcpServers` 键 | `[]` ❌（settings 文件不认这个键） |
| 同上 + 工作目录放 `.mcp.json` + `enableAllProjectMcpServers` | `[]` ❌ |
| 同上 + **`--mcp-config <file>`** | `asana` → `status: needs-auth` ✅ |

即：`--setting-sources ""` 下**唯一**能把 MCP server 定义带进去的入口是 `--mcp-config`。

### 为什么现在过不去

`--mcp-config` 被 `deniedExtraFlags`（`internal/cli/protocol_claude.go:237`）显式拒绝，注释理由是 "loads attacker-controlled MCP server defs"。`cfg.CLI.Args` → `RouterConfig.ExtraArgs` → `capExtraArgsBytes` → `filterDeniedFlags` 会把它静默剥掉（仅 Warn），所以在 `config-local.yaml` 的 `cli.args` 里直接写 `--mcp-config` **无效**。

该注释同时给出了正解（本 RFC 即按此执行）：

> Callers needing one of these flags must wire it through a dedicated
> SpawnOptions field that BuildArgs renders explicitly, not the catch-all
> ExtraArgs slice.

## 2. Goals & non-goals

### Goals

- G1：让开启隔离 settings 的部署能给 cc 会话挂 MCP server，且 MCP 集由 **operator 的 config.yaml** 单点决定。
- G2：默认关闭。`cli.mcp_config` 空值时，**每一个现存 spawn 的 argv 逐字节不变**。
- G3：路径校验与 `SettingsFile` / `DebugFile` 同范式（绝对路径 + 拒 `-` 前缀），保持 argv 注入面不扩大。
- G4：`router_shim.go` 的 arg-drift 比较必须镜像真实 spawn，避免升级后误判重启存活会话。
- G5（评审 F7 追加，**可用性硬要求**）：一个配错的 MCP 文件**绝不允许**升级为"naozhi 完全不可用"。启动时做信封预校验，任何不通过即置空该字段（退化为"无 MCP"，会话照常）。见 §4.1 实测边界。

### Non-goals

- NG1：**不**从 `deniedExtraFlags` 移除 `--mcp-config`。该拒绝是对的；专用字段绕过它即足够。
- NG2：**不**做 per-agent / per-project 粒度的 MCP 集覆盖。MCP server 定义等于"给 cc 装工具"，是高权限决策，只留 operator 一个注入点（router 全局）。后续若确有需要，另开 RFC 并需重新评审权限模型。
- NG3：**不**动 ACP（kiro）/ codex backend。它们不吃这个 flag，与 `SettingsFile` 现状一致。
- NG4：**不**动 `internal/sysession` Runner 的 `--setting-sources ""`。Runner 无 entry-auth，加载宿主 hook 有实证死循环风险（`runner.go:259-262`），且 AutoTitler 之类的 daemon 不需要 MCP。
- NG5：**不**代管 MCP OAuth token。远程 MCP 的授权仍靠交互式 cc 的 `/mcp` 走浏览器，token 落 `~/.claude.json` 的 `mcpOAuth`；naozhi 只负责挂载 server 定义。

## 3. Alternatives considered

### 方案 A（选定）— 新增 `cli.mcp_config` → `--mcp-config`

照 `SettingsFile` 的现成链路复制一遍：config → main.go 解析校验 → RouterConfig → Router 字段 → SpawnOptions → BuildArgs。

- 优点：与既有范式同构（5 个触点全有先例可循）；opt-in 默认关；保住隔离 settings；MCP 集单点 operator 控制；实测已验证 `--mcp-config` 在 `--setting-sources ""` 下确实生效。
- 缺点：新增一个配置项与一条 argv 分支；MCP server 定义文件成为新的高权限落盘面（须 0600，见 §5）。

### 方案 B — 关掉 `naozhi_settings.enabled`，回退 `--setting-sources user`

零代码。cc 直读 `~/.claude/settings.json` 与 `~/.claude.json`，MCP 立刻可用（矩阵第 1 行已证）。

- 否决理由：会丢掉本机刻意要的隔离收益 —— `naozhi-settings.json` 承载 `modelOverrides`（`claude-fable-5` → `us.` 池）、`enforceAvailableModels`、独立 `effortLevel`，且脱钩后组织侧同步不会覆盖它（`config-local.yaml:8-10` 的注释即为此）。为拿 MCP 而放弃模型映射隔离是净亏。

### 方案 C — 让 `naozhisettings` 在 settings 文件里注入 `mcpServers` 键

- 否决理由：**实测不可行**（矩阵第 3 行）。cc 的 settings schema 不认 `mcpServers`，写进去被忽略。

### 方案 D — 工作目录放 `.mcp.json` + `enableAllProjectMcpServers`

- 否决理由：**实测不可行**（矩阵第 4 行）。project 级 MCP 发现同样被 `--setting-sources ""` 关掉。且 per-workspace 落 `.mcp.json` 会污染用户项目目录，与 "naozhi 不污染 Claude CLI 工件" 的既有约束冲突（见 `docs/rfc/project-stable-session-key.md:79`）。

### 方案 E — 用 `npx mcp-remote` 包成 stdio server

不是 A 的替代而是**补充**：仍需经 `--mcp-config` 挂载，所以 A 是前置。价值在于 token 由 `mcp-remote` 存自己的 `~/.mcp-auth`，绕开 §7 的 token 共享未知。留作 A 落地后若 token 不共享时的 fallback，不在本 RFC 实施范围。

## 4. Test strategy

### 4.1 前置实测：无效 MCP 文件的失败边界（评审 F7，cc `2.1.235.672`）

本节是 G5 的依据。评审提出"文件存在但内容非法时 cc 行为未知"，实测四种输入后边界如下：

| 输入 | cc 行为 | 分类 |
|---|---|---|
| 文件不存在 | `Error: Invalid MCP configuration: MCP config file not found` → **无 `system/init`，进程退出** | **硬失败** |
| JSON 不可解析（尾逗号） | `Error: ... MCP config is not a valid JSON` → **无 `system/init`，进程退出** | **硬失败** |
| 合法 JSON 但缺 `mcpServers` 键（`{}`） | `Error: ... mcpServers: Invalid input: expected record, received undefined` → **无 `system/init`，进程退出** | **硬失败** |
| `mcpServers` 存在但单个 server 定义非法（`type: "bogus"`） | **正常 spawn**，`mcp_servers: []` + 新字段 `mcp_server_errors: [{name, type:"unknown_type", message}]`，turn 正常完成 | 软失败 |

结论 —— 两条都直接决定实现形状：

1. **硬失败是真实 P1**：MCP 文件里一个尾逗号会让**每一个会话 spawn 失败**，即从"MCP 不可用"升级为"naozhi 不可用"。故 §8 的 resolve 必须做**信封预校验**。
2. **预校验只需校验信封，不必校验 server 定义**：cc 对 per-server 问题优雅降级并自报。预校验判据恰好三条 —— 文件可读 / JSON 可解析 / `mcpServers` 为对象。

实现约束（与 §5 F5 的取舍绑定）：预校验解析为 `struct{ MCPServers map[string]json.RawMessage \`json:"mcpServers"\` }` 并要求非 nil，**不下探 RawMessage 内部**，解析结果立即丢弃，绝不进日志。

### 4.2 单元测试

全部放 `internal/cli/cli_test.go`（紧邻既有 `TestClaudeProtocol_BuildArgs_SettingsFile*` 三连），复用其 `settingsArgs` helper 的写法新增一个 `mcpConfigArg` 提取器。

| 测试 | 断言 | 旧代码上 |
|---|---|---|
| `TestClaudeProtocol_BuildArgs_MCPConfigDefault` | `MCPConfigFile` 零值 → argv **不含** `--mcp-config` | 通过（保护 G2 不回归） |
| `TestClaudeProtocol_BuildArgs_MCPConfigAbs` | 绝对路径 → 恰好一次 `--mcp-config <path>` | **失败** |
| `TestClaudeProtocol_BuildArgs_MCPConfigGuards` | 相对路径 / `-`前缀 / `--foo` → **不**发 flag（表驱动，与 `SettingsFileGuards` 同构） | **失败** |
| `TestClaudeProtocol_BuildArgs_MCPConfigIndependentOfSettings` | `MCPConfigFile` 与 `SettingsFile` 两个维度正交：4 种组合下 `--mcp-config` 的有无只由前者决定 | **失败** |
| `TestBuildArgs_MCPConfigStillDeniedInExtraArgs` | `ExtraArgs: ["--mcp-config","/x"]` 仍被 `filterDeniedFlags` 剥掉（锁 NG1） | 通过（防未来误删 denylist 条目） |

### 4.3 config 解析层测试（评审 F2）

设计初稿把校验责任放在 `main.go` 的 resolve 函数并声明"仿 `resolveNaozhiSettingsFile`"，但实测确认**该先例函数自身零测试覆盖**（`grep resolveNaozhiSettingsFile cmd/naozhi/*_test.go` 为空）。照抄先例等于照抄其测试缺口，而本字段的核心失败模式（配了却静默不生效、或配错导致全局 spawn 失败）**只能在这一层被捕获**。故本 RFC 要求**优于**先例：

`TestResolveMCPConfigFile`（新增，`cmd/naozhi/`，表驱动）：

| 输入 | 期望 |
|---|---|
| 空值 | `""`，无日志噪声 |
| 相对路径 | `""` + `slog.Error` |
| `-` 前缀 | `""` + `slog.Error` |
| 文件不存在 | `""` + `slog.Error`（**G5 关键格**：不置空则 cc 硬失败） |
| JSON 不可解析 | `""` + `slog.Error`（**G5 关键格**） |
| 合法 JSON 但缺 `mcpServers` | `""` + `slog.Error`（**G5 关键格**） |
| `mcpServers` 存在但 server 定义非法 | **原路径**（cc 软降级，不该被 naozhi 拦） |
| 合法绝对路径 | 原路径 |
| 文件 mode 含 group/other 写位 | 原路径 + `slog.Warn`（F4，不失败） |

### 4.4 shim arg-drift 回归（评审 F3）

G4 是明确正确性契约（漏传 → 升级后重启所有存活会话），不留条件句。**无条件要求**：先跑 `go test ./internal/session/...` 确认既有 shim 测试是否已捕获漏传；无论结果如何，都要有一条显式断言钉住 `MCPConfigFile` 参与 drift 比较、且设置后不产生 false positive。

手工验证：见 §8。

## 5. Risk & rollback

### 风险

| 风险 | 评级 | 缓解 |
|---|---|---|
| MCP 配置文件成为高权限注入面：谁能写该文件，就能给所有 cc 会话装任意工具（含 stdio server = 任意命令执行） | **P1** | 路径来自 operator config，非 prompt / agent 可控（NG2 锁死无 per-agent 覆盖）。**机制而非口头约定**（评审 F4）：resolve 时 stat 文件，mode 含 group/other 写位则 `slog.Warn` —— 只告警不失败，因多用户场景可能有合理例外，硬失败会把权限判断问题引入启动路径。**注意**：cc 自带 Bash + `--dangerously-skip-permissions`，本就能执行任意命令，故此面是"持久化通道"而非新增"代码执行"面 —— 与 `direct-user-settings.md:128` 记录的同类残余风险同构。 |
| **配错的 MCP 文件把"MCP 不可用"升级为"naozhi 完全不可用"** | **P1** | 评审 F7 提出、§4.1 实测确认为真：文件缺失 / JSON 非法 / 缺 `mcpServers` 键三种情形下 cc **拒绝启动**（无 `system/init`）。缓解即 G5 —— resolve 做信封预校验，任一不通过则置空该字段，退化为"无 MCP"而非阻断 spawn。由 §4.3 的三个"G5 关键格"钉住。 |
| `--mcp-config` 从 denylist 被未来某次改动误删 | P2 | `TestBuildArgs_MCPConfigStillDeniedInExtraArgs` 钉住 |
| 配置了路径但文件不可用 → operator 无线索 | P2 | 同上 G5 路径：置空并发 `slog.Error`，**绝不**在"看起来启用了"的同时实际没生效；BuildArgs 的静默丢弃仅作 defence-in-depth 兜底 |
| 预校验需读取文件内容，与 F5 的"naozhi 从不读取"性质冲突 | P3 | 取舍已定（§4.1）：为 G5 的可用性硬要求让步，但把读取面压到最小 —— 只解析外层信封到 `map[string]json.RawMessage`，**不下探** RawMessage 内部（`headers` 里的 token 始终是未解析字节），解析结果立即丢弃，绝不进日志。见 F5 修订。 |
| 漏传 shim drift → 升级后存活会话被误判重启 | P2 | G4；`SettingsFile` 已有先例注释（`router_shim.go:330-333`）可循 |
| **信封校验只在启动时做一次，之后会过期** —— naozhi 运行期间 operator 把该文件编辑坏，G5 的防线已经跑完了，下一次 spawn 重新落回"cc 拒绝启动"的 P1 情形 | P2 | 有意不做 per-spawn 复检：一是把文件 I/O 塞进 spawn 路径，二是根本关不掉竞态（cc 自己读文件发生在 naozhi 校验之后，中间永远有窗口）。缓解是流程性的：`config.example.yaml` 明确要求"改完这个文件要重启 naozhi"，且失败是响的（spawn 失败 + cc 的 `Error: Invalid MCP configuration`），不是静默降级 |
| 路径指向 FIFO / 字符设备 → `os.ReadFile` 永不返回，启动直接挂死（比 G5 要防的硬失败更糟，且发生在信封校验之前） | P2 | diff 评审 D1：先 `os.Stat` 再读，非 regular file 直接拒；同时用 stat 结果加了 1 MiB 尺寸上限（合法信封只有几 KiB）。两格由 `TestResolveMCPConfigFile` 的 `D1:` 子测试钉住 |
| 第三方 SaaS 数据经 MCP 进入 Bedrock 的合规面 | 非本 RFC 决定 | `naozhi-settings.json` 的 `companyAnnouncements` 载明 cc 仅 cleared for Business Data，ITAR/PII/Customer Data 禁用。挂哪个 MCP 由 operator 逐个判断，本 RFC 只提供机制 |

### Rollback

三级，任选：

1. 配置级（无需重新部署）：删掉 `cli.mcp_config` 一行 + 重启 → 回到 `--mcp-config` 不出现的现状。
2. 文件级：删 MCP 配置文件 → main.go stat 失败 → 置空 → 同上。
3. 代码级：revert 单个 commit。改动纯增量（新字段 + 一条 `if`），无既有分支语义变更，revert 无冲突面。

## 6. Observability

- `cmd/naozhi/main.go` 解析处：启用时 `slog.Info("mcp config: enabled", "path", path)`；信封预校验任一环失败时 `slog.Error(... "staying without mcp config")` 并返回 `""`。语义与 `resolveNaozhiSettingsFile` 一致 —— **绝不**在"看起来启用了"的同时实际没生效。权限位可疑时额外一条 `slog.Warn`（F4）。
- 不新增 metric。MCP 的两条状态通路都由 cc 自报、经 naozhi eventlog 原样落盘，排障读 eventlog 即可，另建计数器会产生第二真相源：
  - `system/init` 的 `mcp_servers[].status` —— 逐 server 连接态（`needs-auth` / `connected` / `failed`）
  - `system/init` 的 `mcp_server_errors[]` —— §4.1 实测发现的**软失败通路**，逐条给出 `{name, type, message}`（如 `unknown_type`）。这是 per-server 定义错误的唯一线索来源，排障时应优先看这里。
- 不打印文件**内容**。预校验虽读取文件（§4.1 取舍），但只 unmarshal 外层信封、不下探 server 定义，且解析结果不进日志 —— 只打路径。

## 7. Compatibility & migration

- 向后兼容：`cli.mcp_config` 缺省为空 → `MCPConfigFile` 零值 → BuildArgs 不发 flag → **argv 逐字节不变**（G2，由 `MCPConfigDefault` 测试钉住）。
- 无落盘格式变更、无 migration、无 config 破坏性改名。
- 与 `SettingsFile` 正交：两个开关可独立开合，4 种组合都合法（由 `MCPConfigIndependentOfSettings` 测试钉住）。
- ACP / codex backend 忽略该字段，与 `SettingsFile` 现状一致。
- **仅接受绝对文件路径，不支持内联 JSON**（评审 F6）：cc 的 `--mcp-config` 两种形态都吃，但本设计的 `filepath.IsAbs` 校验会拒掉内联 JSON。这是**有意**的 —— 只支持文件才能做权限检查（F4）与把内容留在 naozhi 之外（F5）。须在 `config.example.yaml` 注释里写明，否则 operator 尝试内联写法会得到静默失效。
- **naozhi 不保留文件内容**（评审 F5，已按 §4.1 修订）：初稿声称"naozhi 从不读取该文件"，G5 的预校验使这一点不再严格成立。修订后的准确性质是 —— naozhi **只在启动时瞬时读取一次**用于信封校验，只 unmarshal 到 `map[string]json.RawMessage` 层、**不下探** server 定义（故 `headers` 里的 bearer token 始终停留在未解析字节里），解析结果不驻留、不进日志、不经任何 API 暴露。相对"naozhi 解析并转发 MCP 定义"的方案，这仍是实质安全优势，只是强度从"从不读取"降为"瞬时读取信封且不下探"。
- **已知未知（须实测，不在本 RFC 断言）**：远程 MCP 的 OAuth token 存在 `~/.claude.json` 的 `mcpOAuth` 段，而 `--setting-sources ""` 已证会屏蔽同一文件的 `mcpServers` 段。token 是否同样被屏蔽**尚未验证**。正面信号：实测 `--mcp-config` 下 asana 报 `needs-auth` 而非 `failed`，说明 cc 确实去查了 OAuth 存储。若实测证明 token 不共享，走方案 E（`mcp-remote` 自带 `~/.mcp-auth`）。此项写入 §8 验收清单。

## 8. Rollout plan

单 PR，opt-in 默认关，无需分阶段或 flag 摘除计划（配置项本身即长期开关）。

改动触点（7 处：代码 6 + 文档 1，后者含一处既有事实错误的修正）：

1. `internal/cli/wrapper.go` — `SpawnOptions` 加 `MCPConfigFile string` + godoc
2. `internal/cli/protocol_claude.go` — `BuildArgs` 在 `--debug-file` 旁按同范式渲染 `--mcp-config`
3. `internal/session/router_core.go` — `RouterConfig.MCPConfigFile` 字段 + Router 私有字段 + `NewRouter` 赋值。**必须是 router 全局单字段，仿 `NaozhiSettingsFile`；不得进 per-backend map**（评审 F1）—— `BackendModels`/`BackendExtraArgs`/`BackendEfforts` 那套传播机制会把顶层值推给不接受该 flag 的 backend，`config-local.yaml:58` 记录了 `cli.effort` 因此报 "backend does not accept one" 的真实踩坑。
4. `internal/session/router_lifecycle.go` — 真实 spawn 传 `MCPConfigFile`
5. `internal/session/router_shim.go` — **arg-drift 比较镜像**（G4；漏了会在升级后重启所有存活会话）
6. `cmd/naozhi/main.go` + `internal/config/config.go` — `cli.mcp_config` 读取、`ExpandHome`、绝对路径校验 + **信封预校验**（G5，见 §4.1）+ 权限位告警（F4）
7. 文档同步（评审 F8）：
   - **`docs/design/DESIGN.md:1277` —— 修正既有事实错误**。原文断言 `--setting-sources ""` 下"MCP/skills 正常"，本 RFC §1 实测矩阵证明 MCP **完全不加载**（`mcp_servers: []`）。不修则后续读者会据此错误结论排查。同页 `:1088` 表格"完整 CC 工具 (需 `--setting-sources ""`)"须加注。
   - `CLAUDE.md:292` —— `cli` 字段清单补 `mcp_config`
   - `config.example.yaml` 的 `cli:` 段 —— 补注释掉的范例，写明"仅绝对文件路径、不支持内联 JSON"（F6）与 0600 要求（F4）

验收（手工，本机）：

```bash
# 1. argv 渲染 + config 解析 + shim drift
go build ./... && go vet ./... && go test ./...

# 2. G5 可用性硬要求（回归 §4.1 的三个硬失败格）：
#    把 cli.mcp_config 指向 (a) 不存在的文件 (b) 尾逗号 JSON (c) {}
#    每种都必须：naozhi 正常启动、会话正常 spawn、日志有一条 slog.Error。
#    任一情形出现 spawn 失败 = G5 未达成，阻断合入。

# 3. 端到端：naozhi 内的会话能看到 MCP
launchctl kickstart -k gui/$(id -u)/com.naozhi.agent
# dashboard 开一个会话 → 读 eventlog 的 system/init → mcp_servers 非空

# 4. §7 的已知未知：授权后 OAuth token 是否跨 --setting-sources "" 共享
#    交互式 cc 里 /mcp → 目标 server → Authenticate，然后：
claude -p --output-format stream-json --verbose \
  --setting-sources "" --settings ~/.naozhi/naozhi-settings.json \
  --mcp-config <mcp file> --dangerously-skip-permissions "hi" </dev/null | head -1
#    看 mcp_servers[].status 是否 connected（而非仍 needs-auth）
#    若仍 needs-auth → token 不共享 → 转方案 E（mcp-remote 自带 ~/.mcp-auth）
```

> 注：验收命令不要带 `--model claude-haiku-4-5`。实测该别名在隔离 settings 下会被
> `--model` 覆盖 `modelOverrides` 映射，触发 Bedrock 400 后 fallback 到 Sonnet 5，
> 给验收日志引入无关噪声。`system/init` 在模型调用之前发出，故不影响 MCP 判定，
> 但省掉该 flag 更干净。

> **跑第 2/3 项前必须隔离 shim 状态**。这两项都要在本机起第二个 naozhi 进程，而 shim
> 的 `StateDir`（默认 `$HOME/.naozhi/shims`）和 socket 目录（`XDG_RUNTIME_DIR` 否则
> `~/.naozhi/run`）是从 **HOME 派生**的，不跟随 `session.store_path`。后果：第二个实例
> 会 discover 到线上实例的 shim，因 `--settings` 路径不同判定 config drift，日志打
> `shim config drifted, shutting down old shim` 并尝试关掉**线上**会话进程。
> 验收配置必须同时给出：
>
> ```yaml
> session:
>   shim:
>     state_dir: "/tmp/<scratch>/shims"   # 否则会看见并接管线上 shim
> ```
>
> 外加 `XDG_RUNTIME_DIR=/tmp/<scratch>/xdg` 环境变量隔离 socket 目录。

**验收结果（2026-08-20 本机实测）**

- 第 1 项：`go build` / `go vet` / `go test ./...` 全绿（CI 同 run 上 8/9 job pass，仅
  `vuln` 红，为 master 已存在的 stdlib CVE，与本 diff 无关）。
- 第 2 项 **G5 三格全部达成**。按上述隔离配置起临时实例，三种坏文件下 naozhi 均跑满
  超时窗口未退出（`timeout` 返回 124），各自恰好一条 `slog.Error`，且 `drift` 行 0 条：

  | 输入 | 日志 |
  |---|---|
  | 文件不存在 | `mcp config: cannot stat file; staying without mcp config` |
  | 尾逗号 JSON | `mcp config: file is not a JSON object with an "mcpServers" object; ...` |
  | `{}` | `mcp config: file has no "mcpServers" object; ...` |

- 第 3 项：以 naozhi 的完全相同 spawn 形状（`--setting-sources "" --settings
  <naozhi settings> --mcp-config <file>`）实测 `system/init` → server `status: connected`、
  `mcp_server_errors: null`、56 个工具可见。
- 第 4 项（§7 的已知未知）**在本机场景下不再是阻塞项**：落地选的是 Amazon 企业版
  `enterprise-asana-mcp`（本地 stdio），授权走 midway → Stonegate 3LO 换 token，token
  缓存在该 server 自己的进程侧，**不经 cc 的 `~/.claude.json` `mcpOAuth`**，因此与
  `--setting-sources ""` 是否共享 OAuth 无关。远程 http server 的该问题仍未验证。

发布：合入 master 后按 `docs/ops/deployment-strategy.md` 打 tag，各机 `naozhi upgrade`。本机 launchd 实例可先本地构建验证（开发用途），但分发必须走 release pipeline。
