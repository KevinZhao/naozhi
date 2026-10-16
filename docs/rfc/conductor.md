# RFC: Conductor —— 一个对话框，规划 agent 自己调度 session

- 状态: Draft v1（设计决策已与 owner 确认，未实施）
- 日期: 2026-10-16
- 范围: 把 naozhi 的交互从"用户逐个操作 session"改为"用户只面对一个对话框：快模型前台负责人机沟通，强模型 Conductor 只做策划并把任务派给 worker session 执行，主动 Conductor 按章程巡检项目并在授权范围内自主执行"。同时定义配套执行环境（凭证、沙箱、auto mode）。
- 相关: #3448(b)（naozhi 作为 MCP server）、#3449（per-session git worktree）、`cli-mcp-config.md`、`system-session.md`、`workflow-dashboard.md`

## 1. 背景与问题

现状（2026-10 代码核实）：

- session 之间完全不互通：没有任何机制让一个 session 创建 / 驱动另一个 session 或回收其结果。
- `project:{name}:planner` 只是特殊 key + prompt + cwd + 豁免 TTL/max_procs 的普通 session（`internal/session/routing.go` `ResolveForChat` / `plannerOpts`），不能派活。`DESIGN.md` 里"planner 经 dashboard API 或 cron 协调其他 session"无实现。
- Agent team / Workflow 都在单个 CLI 进程内扇出，naozhi 只观测（`internal/subagent`、`internal/cli/workflow`）。
- 用户为每个 session 手动决定 workspace、node、backend、access profile、agent、model/effort、label、续接还是新开。
- 执行环境：Claude 恒带 `--dangerously-skip-permissions`（`internal/cli/protocol_claude.go`），Kiro 权限请求自动 `allow_once`，Codex `approval_policy=never`；envpolicy 只过滤 env，不隔离文件系统（`~/.aws`、`~/.midway/cookie`、`~/.ssh`、`~/.naozhi/*` 对 Bash 可读）；`mcp_config` 全局一份。

外部参照：Anthropic 2026-09-17 发布的 Claude Code Projects 与本设计同构（一个协调对话 + 并行 worker thread + "Waiting on you" Overview），但依赖 claude.ai 登录与云端 thread。naozhi 的差异化定位因此明确为：**Bedrock、多 backend（Kiro/Codex）、飞书/微信、本机凭证与内网系统、多节点自托管**。

## 2. 目标 / 非目标

目标

- G1 用户只面对一个对话框（每个入口一个），不再手动管理 session。
- G2 规划与执行分离：强模型只做决策，执行交给按任务选型的 worker。
- G3 主动模式：项目级 Conductor 按章程与触发器巡检，在常设授权内自主执行，其余提议待批。
- G4 agent 能使用本机凭证与内部系统（邮箱、Asana、Midway、AWS），不逐次人工批准，但有硬边界、审计与急停。

非目标（本 RFC 范围内不做，见 §13）

- 内部 Conductor 之间走 A2A；依赖 MCP Sampling / Roots；社区技能市场；向量库记忆；三层以上 Conductor 嵌套；独立的主动推送 feed。

## 3. 角色模型

```
用户（IM / Dashboard）
    │
前台 Interlocutor   sonnet 5.5，每个入口一个；自己能答的直接答（快路径）
    │  委托（带用户原话）            ▲ 报告 / 提问（原样转发，不改写）
Conductor（可多个） 偏好表首个可用的最强模型；每个 workspace 唯一、串行；无 Bash/Edit，有只读工具
    │  dispatch（立即返回句柄）      ▲ 完成信封（摘要 + 结果引用）
worker sessions     按任务选模型 / backend / 能力档；各自 worktree
```

### 3.1 前台（Interlocutor）

- 模型：sonnet 5.5。
- 每个入口（IM chat / dashboard）一个；按渠道排版。
- **快路径**：闲聊、状态查询（读任务板）、简单问答直接答，不惊动 Conductor；否则这一层只会加延迟。
- **不当传话筒**：委托时信封必须携带用户原话与附件，前台的理解只作附注；Conductor 的提问、worker 的产出经 `forward` 原样给用户；批准 / 提问走带 ID 的结构化卡片，不经前台改写。（LangChain 实测原文转发比转述好约 50%。）
- 用户可在 IM 里回复某个 worker / Conductor 消息直接操控它（绕过前台）；`/btw` 侧问不打断主任务。
- 收到消息立即回执，永不阻塞等 Conductor。

### 3.2 Conductor

- **模型**：配置有序偏好表，取第一个当前凭证能调通的（复用 `ccprobe` 实跑验证）；不由系统自评谁更强。例：`[claude-opus-5-5[1m], claude-fable-5-1]`（fable-5-1 在本机 naozhi 凭证下是 explicit deny，当前实际落到 opus 5.5）。
- **只策划，不执行**：没有 Bash / Edit / Write；**保留只读工具**（Read / Grep / Glob、`git log`、`gh ... view`）或便宜的"侦察" dispatch。依据：Cognition、Cursor 的经验都是缺乏代码库上下文的 planner 会过度规定细节。
- **任务 schema 写"目标 + 验收标准 + 归属路径"，不写实现步骤**（细节规格的错误会层层放大）。
- **每个 workspace 唯一、串行**：所有触发（用户委托、duty、外部事件、worker 完成）都是投给它的信封。Conductor 从不执行、从不等待，单轮是"看状态 → 决策 → 派活 → 返回"，所以串行不成瓶颈；并行发生在 worker 层。多实例并行反而会重复派活、需要给决策加锁。
- 多个 Conductor 按 workspace / 领域划分（naozhi、其他项目、邮箱、个人杂事）；未绑定项目的请求进"个人杂事" Conductor。
- **状态外置**：charter、任务板、提议队列、journal 落盘；Conductor session 可随时 reset（每日或上下文超阈值），重建时读 charter + 任务板 + 最近 N 条信封，不依赖 transcript。

### 3.3 worker

- 由 Conductor 按任务选 model / effort / backend / 能力档；常规活 sonnet，难活 opus。
- 每个 worker 一个 git worktree + 分支（#3449）；Conductor 声明归属路径，**网关**对路径重叠的任务串行化。
- 结束时返回结构化交接：做了什么 / 没做什么 / 命令与退出码 / 发现的问题 / 经验候选；允许显式"不可完成"退出（无诚实出口时 agent 倾向作弊）。
- 短小、同目录、跑完即弃的子任务优先用 CC 原生 Agent / Workflow（已有可视化），只有需要并行很久、不同 workspace / backend / 能力档、或之后要单独追问的才开 worker。

## 4. 主动 Conductor：charter、duties、触发器

主动 / 被动不是两种角色，只是 Conductor 有没有 duties。

```yaml
# <project>/.naozhi/conductor.yaml —— 由现有 project.yaml 的 planner 配置演进
mode: active
charter:
  scope: 维护 naozhi 仓库健康：CI 绿、flaky 收敛、依赖安全更新、issue 分诊
  escalation: 超出 standing_orders 的一律提议；连续 3 次失败停手上报
duties:
  ci-watch:     { trigger: "event:github.ci_failed", fallback_cron: "*/30 * * * *", autonomy: execute }
  flaky-sweep:  { cron: "0 3 * * *",  autonomy: propose }
  issue-triage: { cron: "0 9 * * *",  autonomy: propose }
  dep-audit:    { cron: "0 10 * * 1", autonomy: propose }
standing_orders:          # 常设授权：做完汇报
  - 修 flaky 测试并开 PR
  - 依赖安全补丁
budget: { daily_usd: 30, max_workers: 4, max_threads_per_day: 40 }
```

- **charter 采用 standing-orders 四要素**（Scope / Triggers / Approval gates / Escalation，参照 OpenClaw），启动时校验 schema；调度时间放在 naozhi cron 里，不写在 prompt 文本里。
- **schedule 缺省 = 关闭**（OpenClaw "缺省即 30 分钟心跳"造成过意外账单）。
- **事件优先、轮询兜底**；事件按来源 ID 去重，每个 duty 限速；cron duty 加 jitter。
- **巡检不在 Conductor 的长上下文里跑**：duty 先由轻量上下文 + 便宜模型的侦察 worker 执行，默认静默（NO_REPLY），有结论才投信封唤醒 Conductor。（OpenClaw 心跳实测每次 28k–107k token，静默心跳占一半 token。）
- **自主度三档**：propose（提议待批）→ draft（做好不发布）→ execute（做完汇报）。默认 propose，按 §11 的 SLO 升档。批准粒度是"计划"，不是"每次写操作"。
- **常设授权**：IM 卡片上"以后都允许"生成可撤销、有使用计数与过期时间的 standing grant，dashboard 可列出 / 撤销。
- 第一个主动 Conductor 落在 naozhi 仓库本身（成败标准清晰）；邮箱类等 §10.3 的读写分离落地后再上。

## 5. 信封与任务板

### 5.1 信封

所有角色间交互归一为"往某 session 的队列投一个信封"：

```
Envelope{ id, from, to, kind: request|report|question|answer|event,
          provenance: human|agent|external, task_id, body, refs[] }
```

- 投递走 `turn.Orchestrator.Submit` + 新的 `envelopeOrigin`（Origin/Delivery 端口现成，Orchestrator 不改）；对方忙则排队，collect 模式合并同类。
- 队列加优先级：human 来源的信封插到触发器信封前；同类触发器合并（3 个 `ci_failed` 只处理一次）。
- 报告传摘要 + ref，全文落盘，Conductor 需要时 `read_result`。
- 信封日志 append-only（事件溯源）；任务板状态迁移只经网关。

### 5.2 naozhi MCP（#3448(b) 的落地形态）

- `naozhi mcp` stdio 桥经 unix socket 连回 naozhi，带每 session 签发的能力令牌（不复用 dashboard token）；校验对端 PID（macOS `LOCAL_PEERPID`）及其祖先是 naozhi spawn 的进程。
- 需要 per-session mcp-config（当前 `cli.mcp_config` 是全局的）。
- **句柄式异步**：`dispatch(...) → {run_id}` 立即返回；`await_run(run_id, timeout)` 期间发 `notifications/progress` 重置 stdio 空闲计时；另有 `get_run` / `cancel_run` / `list_runs`。依据：MCP 2026-07-28 推荐"服务端签发句柄、作为普通参数传递"；Claude Code 不实现 MCP Tasks 扩展客户端（#52137 / #18617 closed not planned）。
- 不用 Sampling / Roots（已弃用）；`ask_user` 是普通工具，由 naozhi 路由到 IM / dashboard，不依赖 server 端 elicitation。
- 无状态形态：身份取自令牌而非连接；`tools/list` 固定顺序（利于 prompt cache）；工具面保持约 8–12 个。
- 角色工具集：前台 `delegate / list_tasks / answer / forward`；Conductor `dispatch / send / cancel / report / ask_user / propose / list_tasks / read_result / memory_propose / journal_append`；worker 仅 `report_progress`（可选）。worker 另有 Bash 可用的 `naozhi` CLI，大输出走文件 / jq 不进上下文。
- **限额由网关强制**：并发、花费、PR 数、每日 thread 数都在 naozhi 侧判定，不靠 prompt（Projects 文档自承"最多两个 thread"只是指令，不是硬上限）。

## 6. 验证闸门

- 任务的验收标准在 dispatch 时写好（default-FAIL：每条初始为未通过）。
- 任务"完成"只认 verifier worker 的 PASS：新上下文、只读、换模型或 effort 档，自己重跑 oracle（`go test ./...`、`gofmt -l`、lint、CI 状态），不采信 worker 自述。按任务类别可配置，简单任务不强制。
- **测试完整性 diff 闸门**：改动触及 `*_test.go`、CI 配置、新增 `t.Skip`、`*Baseline*` 常量、`scripts/ratchet-raises.jsonl`、lint 豁免、`.claude/` 设置时默认拦下标记，除非任务明确允许。CI watch 与 flaky 清扫最易出现"靠 skip 修好"（ImpossibleBench：Claude 模型多见直接改测试；Opus 5.5 system card：不可能任务下奖励作弊率 6.0% → 34.7%）。
- CC 的 `/goal` 只是停止规则（只看 transcript），不能当 verifier。

## 7. 注意力收件箱与交互

- 分组沿用 Projects：Waiting on you / Ready for review / Working / Landing / Idle。
- 条目类型 notify / question / review / decision / error，带归属 Conductor 与 workspace、等待时长；条件消失自动关闭；只有人能关闭。
- IM 卡片带请求 ID，校验**发送者本人**而非群 chat ID；首个回答生效；应用决定前重读待决项。
- 每日 digest（静默时段积压到早报），每条带 👍/👎/"少来点"，反馈回流到 duty 的 charter 调优备注。
- 队列 vs 插话：默认排队，"立即生效"是显式动作（Codex Remote 的结论：误插话代价更高）。
- 团队群聊保持隔离，不并入个人的"一个对话框"。
- dashboard：主对话框 + 任务面板（复用 workflow 面板形态，点进去是现有 session 视图）；现有 session 侧边栏降级为"专家模式"保留。

## 8. 记忆与学习

- **单写者**：CC auto-memory 按仓库存放、所有 worktree 共享——worker 读到恶意内容即可写进 Conductor 每次加载的 MEMORY.md。因此每个角色独立记忆目录（`autoMemoryDirectory`），worker 默认 `autoMemoryEnabled:false`，Conductor 只经 `memory_propose` / `journal_append` 写；workers 只读 workspace 记忆摘要，发现经结构化结果字段回流。
- **来源标签**：journal / 记忆条目记 provenance 与 session 类型；cron / 事件 duty 的产出只进 journal，不直接进策展记忆或 charter。只有 human 来源的信封能授权 charter 修改与不可逆操作；Conductor 不能改自己的 charter。
- **PreCompact hook**：压缩前把未决决策写进 journal（压缩有损，journal 无损）。
- **夜间 dream duty**（P1）：独立 session 读当日 journal 与 transcript，产出记忆仓库的 git diff；逐条小改不整篇重写（ACE：一次整篇重写把 18k token 压成 122 token，准确率跌破无记忆基线）；删除比例设上限；内容哈希校验后替换、保留旧版、写人可读变更日志；charter 级变更走 IM 批准。
- **技能只从验证通过的任务提炼**（P1）：worker 交接里的经验候选，经 verifier PASS 或复现 ≥2 次才升为 workspace `SKILL.md`，并留 with/without 回放对照。不让 Conductor 事前臆写技能（SkillsBench：自写技能 −8.1pp）。
- USER.md（P1）：前台加载说话人自己的偏好，只把相关条目带进任务简报；条目原地更新（带日期 / 生效 / 失效），不追加矛盾项；群聊不加载私有记忆。

## 9. 可观测与成本

- **prompt cache TTL**（P0，待实测）：调研称 Bedrock 下 CC 主会话默认 5 分钟 TTL；Conductor 与前台空闲间隔多在 5–60 分钟，设 `promptCacheTtl: "1h"`，worker 保持 5 分钟。duty 要么高于每小时一次（保温），要么 fresh session + 小交接文件。ledger 记每个 duty 的 cache 写 / 读量，cache 读比例下降告警。
- **全链路 trace**（P1）：IM → 前台 → Conductor → worker 一条 OTel trace（`-p` 读入站 `TRACEPARENT`；MCP `_meta` 透传 `traceparent`），span 用 `gen_ai.*` 命名；同时作为审计账本，与 costledger 按 session 关联。
- **SLO**（P1）：每个 duty 记 verifier 通过率、返工率、7 天内被人 revert 的比例、每次成功的成本；作为升档依据。
- **回归评测集**（P1）：从 naozhi 自己历史的 CI / flaky / 分诊案例取 20–50 个，换模型 / prompt / 工具面时重跑。
- 每个可派发单元控制在约 1 小时人类专家工作量（METR 80% 时间地平线约 1–1.5h），边界处 commit + 进度文件。
- 多 agent 约 15× chat token：只在子任务真正独立时扇出。

## 10. 执行环境与凭证

### 10.1 权限模式：沙箱内全权限，不逐次批准

人不在环里，但边界从提示词变成硬约束：

- **worker 整体跑在 `srt`（Anthropic sandbox-runtime）里**（P0）：网络默认拒绝，放行 GitHub、Go 代理（goproxy.cn）、Bedrock 及声明的内部域名；只写自己的 worktree；不可读 `~/.aws`、`~/.ssh`、naozhi 配置、审计与预算状态。EC2 上 worktree 须在启动 worker 前建好（Linux 下 srt 启动时一次性生成 deny 列表）。
- **auto mode 替代 `--dangerously-skip-permissions`**（试用中，见 §15 D11）：分类器按 allow / soft_deny / hard_deny + `autoMode.environment` 审查每个需审查的动作；Bedrock 可用。`autoMode` 只从用户 settings / `--settings` / managed settings 读，不从仓库 `.claude/settings.json` 读——naozhi 用 `--settings <file>`，可按角色注入。必须声明 environment（KevinZhao 组织仓库、goproxy.cn、内网与 Outlook 域、Host containment 身份），否则正常操作会被误拦、宿主凭证请求会被拦。
- auto mode 不是安全边界（调研：误拦约 0.4%，越界动作漏判约 17%；分类器只读消息与命令不读输出），与 srt 叠加：前者管"该不该做"，后者管"做了能碰到什么"。
- 分类器拒绝作为事件进 dashboard 与审计；调研称 `-p` 下连续被拒 3 次 / 累计 20 次会终止进程（附录 A 的单次拒绝未终止，阈值行为待测），这种终止是新的 death reason，进收件箱 error 类，不自动重试。
- 分类器把任务里的明确指令当作用户意图（附录 A：soft_deny 所列的删远程分支在明确要求下被放行），所以它挡不住 Conductor 下达的破坏性指令。
- Conductor 无 Bash/Edit，用 `dontAsk` + 显式白名单。
- 破坏性 git / gh（force push、`reset --hard`、删分支、删 release）另加 PreToolUse hook 拦截；不可逆约束写成 `permissions.deny`，不写在 prompt 里。

### 10.2 凭证

- 每个 worker 独立 STS 会话，带 `SourceIdentity=naozhi/<session>` 与 session tags + session policy，CloudTrail 可归因（P0）。本机 `~/.aws` default 是 admin 账号，交给 agent 必须是显式选择。
- 发布权限 / 生产 token 不放在任何 worker 可读的位置（P0；Opus 5.5 拿到包仓库凭证时约半数运行采取有害动作）。
- Midway / Outlook / Asana 走凭证代理（P1）：worker 调 `midway_fetch(url)`、`outlook_search(...)` 一类能力工具，由 naozhi 注入 cookie / token，不下发原始凭证。
- 本机现有内部 MCP：`aws-outlook-mcp`（默认只读，`OUTLOOK_MCP_ENABLE_WRITES=true` 才开写）、`aws-sentral-mcp`、`emburse-mcp`、`enterprise-asana-mcp`。MCP 按能力档挂载，不再全局一份。
- 内网网页：专用 Chrome profile（Midway 已登录、非主 profile）+ Playwright MCP + 域名白名单，每个动作截图进审计（P1）。Claude in Chrome 需要 claude.ai 登录，Bedrock 下大概率不可用。
- 脱敏补 Midway cookie、JWT 模式；`mcp.json` 与 settings 文件对 agent 不可写或 spawn 前校验指纹。
- 启用编排时强制 dashboard token 并只监听 127.0.0.1（agent 可经 Bash `curl` 本机 API）。

### 10.3 读写分离（推迟）

当前 session 的输入基本来自 owner 或其信任来源，全权限可接受。风险只在 agent 自主读取第三方内容且人不在环里时出现（Meta "Rule of Two"：同一 session 不可同时具备不可信输入、敏感数据、对外动作三者）。naozhi 仓库场景不需要；**邮箱 Conductor 上线的前提**是：读不可信内容的 worker 不同时持有对外写工具，且外部内容以标注过的不可信数据传给 Conductor，绝不作为用户指令（Opus 5.5 更易执行粘贴文本中的恶意指令、更易接受无法验证的授权声明）。

### 10.4 急停与回滚（P0）

- 急停：冻结 dispatch、对 worker 进程组 SIGSTOP、撤销 standing grants；须演练。
- dispatch 前快照 `refs/naozhi/<task>`（CC checkpoint 不追踪 Bash 与子 agent 的改动）；需要时 APFS 本地快照 / EBS 快照。
- 预算不是安全控制（破坏性调用几秒、几分钱）。

## 11. 与现有代码的映射

| 现有 | 演进 |
|---|---|
| `project:{name}:planner` 会话（key、豁免、chat 共享） | Conductor 身份，保留 key 语义 |
| `.naozhi/project.yaml` 的 planner 配置 | 扩展为 charter + duties（`conductor.yaml`） |
| cron：在 `cron:<id>` 里跑 prompt | 新增目标类型"给 Conductor 投 duty 信封"；现有任务照跑 |
| `turn.Orchestrator` Origin / Delivery 端口 | `envelopeOrigin`；队列加优先级与同类合并 |
| scratch pool（继承上下文、上限、TTL、可提升） | worker 的模板 |
| `cli.mcp_config`（全局） | per-session 生成，按角色 / 能力档挂载 |
| `budget` / `costledger` | worker 花费归属 Conductor；`task:` 命名空间设 subject（`dashboard:` 键当前不受预算约束） |
| `max_procs`（默认 3，只驱逐空闲） | `task:` 命名空间独立上限 + 每 Conductor `max_workers` + 主机级并发上限 |
| workflow 面板 / agent drill-in | 任务面板 |
| AskUserQuestion 卡片 | 带 ID 的批准 / 提问卡片 |
| `PermissionMode`（Default=bypass / Standard） | 新增 Auto（`cli.permission_mode`） |

新 key 命名空间：`task:<conductor>:<id>`（worker）。新包暂定 `internal/conductor`（任务板、信封日志、charter 校验）。

## 12. 分阶段

| 阶段 | 内容 |
|---|---|
| P0 验证 | ① auto mode 试用（误拦率、延迟、拒绝后终止行为）；② per-session mcp-config；③ naozhi MCP 桥 + 句柄式 dispatch/await 在 `-p` stream-json 长驻进程中的行为（2 分钟自动后台化在 `-p` 默认关闭）；④ Bedrock 下 `promptCacheTtl` 生效；⑤ srt 包裹 shim 启动的 CLI；⑥ 用现有 REST + curl 糊原型验证交互 |
| P1 编排内核 | 信封 / 任务板 / `envelopeOrigin` 唤醒、naozhi MCP、`task:` 命名空间、网关限额、worktree + 路径归属、结构化交接、verifier 与 diff 闸门、急停与快照 |
| P2 交互 | 前台（快路径 / 原样转发 / 直接操控 worker）、收件箱、IM 卡片、dashboard 主对话框 + 任务面板 |
| P3 主动模式 | charter / duties / 事件触发 / 自主度三档 / standing grants / SLO；首个落在 naozhi 仓库 |
| P4 记忆与凭证 | 单写者记忆、dream duty、技能提炼；能力档 + 凭证代理 + STS SourceIdentity；读写分离后接邮箱 Conductor |
| 持续 | OTel trace、回归评测集、merge queue、子 Conductor |

## 13. 明确不做

| 不做 | 原因 |
|---|---|
| 内部 Conductor 之间走 A2A | 同进程 MCP dispatch 更简单、可审计；A2A 只留给跨机器联邦 |
| 依赖 MCP Sampling / Roots / server 端 elicitation | 2026-07-28 规范已弃用或重做 |
| MCP Tasks 扩展作为主路径 | CC 不实现其客户端；服务端可后补 |
| Claude in Chrome 访问内网 | 需 claude.ai 登录；API 部署下原始注入成功率 31.5% |
| 社区技能市场 | ClawHub 13.4% 的技能有严重问题 |
| 向量库 / 图数据库记忆 | grep + 文件已够用，图 vs 非图差异不显著 |
| 三层以上 Conductor 嵌套 | "两层可能有用，三层就成官僚了"（Factory） |
| 独立主动推送 feed | ChatGPT Pulse 2026-06 下线 |

## 14. 待定问题

- 前台与 Conductor 都是长驻 CLI session，还是前台快路径直接调 Bedrock？（倾向前者，保持"agent loop 委托给 CLI"原则，待 P0 测延迟。）
- 个人"一个对话框"跨 IM 与 dashboard 共享一个前台上下文，还是每入口各自一个？（当前决定每入口一个，共享任务板。）
- Kiro / Codex 作为 worker 时的能力档与 auto mode 等价物（Kiro 自动 `allow_once`、Codex `approval_policy=never`）。
- Kiro CLI V3（2026-10-09 起默认推送）ACP 迁移：`--agent-engine=v3`、`session/list`、每次 `session/load` 重发 `mcpServers`。独立于本 RFC，须先钉版本再迁移。

## 15. 决策记录

| # | 日期 | 决策 | 理由 |
|---|---|---|---|
| D1 | 2026-10-15 | 三层：前台（人机沟通）/ Conductor（策划）/ worker（执行） | 规划与沟通、执行解耦；前台用快模型压延迟 |
| D2 | 2026-10-15 | Conductor 用偏好表首个可用的最强模型（当前 opus 5.5；fable-5-1 本机凭证 deny） | 强弱排序是 owner 的判断，系统只做可用性探测 |
| D3 | 2026-10-15 | 前台用 sonnet 5.5，每入口一个 | 够用且快 |
| D4 | 2026-10-15 | Conductor 每个 workspace 唯一、串行；定时任务与事件是投给它的触发器；并行在 worker 层；可以有多个 Conductor（按 workspace / 领域） | Conductor 不执行、单轮短，串行不成瓶颈；多实例并行会重复派活 |
| D5 | 2026-10-15 | 主动 / 被动是同一 Conductor 有无 duties 的区别；主动模式在授权前提下自动执行 | 统一模型，复用 cron |
| D6 | 2026-10-15 | 写操作不逐次人工批准 | owner 判断模型能力已足够；以硬边界、审计、急停替代 |
| D7 | 2026-10-15 | 读写分离推迟到邮箱 Conductor；现在只做审计与急停 | 当前输入来自可信来源；风险只在自主读取第三方内容时出现 |
| D8 | 2026-10-15 | 第一个主动 Conductor 落在 naozhi 仓库 | 成败标准清晰 |
| D9 | 2026-10-16 | Conductor 无 Bash/Edit，但保留只读工具 | 业界经验：不能看代码的 planner 过度规定细节 |
| D10 | 2026-10-16 | 主动 duty 自主度默认 propose，按 SLO 升档 | Pulse 下线、Copilot Tasks 等证据；CI watch / flaky 清扫可快速升到 execute |
| D11 | 2026-10-16 | worker 跑在 srt 沙箱 + auto mode，替代 `--dangerously-skip-permissions`；auto mode 先试用，经 `cli.permission_mode` 可切回 `bypass` | 人不在环里，边界从提示词变硬约束；事故（Replit / PocketOS）与 system card 证据 |

## 附录 A. auto mode 实测（2026-10-16）

环境：CC `2.1.296.1183`，`claude-opus-5-5[1m]`，`--setting-sources "" --settings ~/.naozhi/naozhi-settings.json --permission-mode auto -p --output-format stream-json`，一次性 git 仓库 + 本地 bare remote，`autoMode` 未配置（全默认）。

| 动作 | 结果 |
|---|---|
| 新建文件、commit、fast-forward push 到 origin main | 放行；没有出现分类器事件；22s / $0.11 |
| 用 `commit-tree` + `reset --hard` 压缩根提交（为随后 force push 做准备） | **被分类器拒绝**（"judged this action dangerous (it gave no explanation)"）；模型未绕行，如实报告"只完成一部分"，force push 从未尝试 |
| 删除远程分支 `origin/feature-x`（默认 soft_deny "Git Destructive" 所列） | **放行**——模型先验证该分支已并入 main，且用户在请求里明确要求 |
| 拒绝之后 | 进程未终止；`result.subtype=success`，`result.permission_denials[]` 列出被拒调用 |

结论与对设计的影响：

- 分类器把**请求里的明确指令视为用户意图**。在本设计中 worker 的"用户"是 Conductor，所以 Conductor 写进任务的指令会被当作用户意图放行。不可逆边界必须落在 `permissions.deny` / PreToolUse hook，不能指望分类器；这也是 §8 provenance 规则（只有 human 来源可授权不可逆操作）的落地点。
- 新的 stream-json 帧：`system/permission_check_status`（每个受审调用 `checking` / `done` 各一帧，无可展示内容）、`system/permission_denied`（`tool_name` / `tool_use_id` / `decision_reason_type: classifier` / `decision_reason`）、`result.permission_denials[]`。naozhi 需丢弃前者、保留并展示后者，拒绝计入审计。
- 一个写成"被拒后换语法重试 + 把含密钥的文件外发"的探测 prompt 直接触发了 **Opus 5.5 模型级 safeguard**：`system/informational` + `system/model_refusal_no_fallback`，`result.is_error=true`，进程退出码 1，分类器根本没机会介入。naozhi 目前不识别这类帧；安全评审类任务（naozhi 自身就有）可能被误伤，需要单独的错误分类（不自动重试、进收件箱），并评估是否给 Conductor / worker 配 fallback 模型。

## 16. 参考（调研 2026-10-16）

- Claude Code Projects: https://code.claude.com/docs/en/claude-projects
- Auto mode 配置: https://code.claude.com/docs/en/auto-mode-config ；权限模式: https://code.claude.com/docs/en/permission-modes
- Anthropic engineering: multi-agent research system (2025-06-13)、effective harnesses for long-running agents (2025-11-26)、harness design for long-running apps (2026-03-24)、Claude Code auto mode、sandboxing / sandbox-runtime
- Cognition "multi-agent working" (2026-04-22)；Cursor "scaling agents" (2026-01-14)、"self-driving codebases" (2026-02-05)；Factory Missions (2026-04-10)
- MCP 2026-07-28 changelog: https://modelcontextprotocol.io/specification/2026-07-28/changelog
- OpenClaw standing orders / heartbeat / memory architecture: https://docs.openclaw.ai/
- Meta "Agents Rule of Two" (2025-10-31)；ImpossibleBench (2025-10)；SkillsBench (2026-06)；ACE (ICLR 2026)；PMPA 记忆投毒 (2026-09)
- METR time horizons: https://metr.org/time-horizons/
- Kiro CLI V3: https://kiro.dev/blog/cli-3/
