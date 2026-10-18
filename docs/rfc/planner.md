# RFC: Planner —— 一个对话框，最懂你也最懂系统的规划 agent

- 状态: Draft v5（设计决策已与 owner 确认，未实施）
- 日期: 2026-10-18（v1 名 conductor.md；v2 改名 Planner；v3 加入监护；v4 收敛为两类 session；v5 重写核心模型：三个角色、两种流动、两份知识，去掉 Task 与"驾驶者"，Planner 定位为幕僚 + 顾问 + 知识维护者，并吸收四路评审的事实订正）
- 范围: 把 naozhi 的交互从"用户逐个操作 session"改为"用户只面对一个对话框：执行 session 和用户一起做事，Planner 理解用户与整个系统、替用户管注意力、在用户前面把事情准备好、在执行跑偏时提出修改意见，并把大任务派给执行 session 完成"。同时定义权限与凭证、多节点调度与现有功能的迁移。
- 相关: #3448(b)（naozhi 作为 MCP server）、#3449（per-session git worktree）、#3686（`cli.permission_mode`）、`passthrough-mode.md`、`cli-mcp-config.md`、`system-session.md`、`workflow-dashboard.md`、`node-pairing.md`

## 0. 术语

| 术语 | 含义 |
|---|---|
| **你 / owner** | 最终的授权来源。v1 只支持单 owner（§17） |
| **Planner** | 每个 workspace 一个的规划 agent；持有"懂你""懂系统"两份知识；承担幕僚、顾问、知识维护者三种职责（§4）。现有 project planner（`project:{name}:planner`）是它的雏形，迁移后合并为同一概念 |
| **执行 session** | 中立的执行者（一个 CLI 进程加上下文）。谁发消息就替谁干活。俗称：你在和它对话时叫"话题线程"，由 Planner 派出时叫"worker"——只是俗称，不是两种类型 |
| **消息** | 角色之间一切主动的沟通，带"代表谁"（§3.3） |
| **事件流** | 网关确定性记录的客观事实（§3.4） |
| **懂你 / 懂系统** | Planner 的两份知识（§3.5、§3.6） |
| **托管** | 执行 session 上的开关：是否由 Planner 替你盯着（§5.4） |
| **网关** | naozhi 进程本身（Go，不用模型）：路由、事件记录、权限执行、急停 |
| **一次性助手** | 单次 `claude -p` 调用，不持有会话：停顿判读、新消息归属提示、摘要 |

## 1. 背景与问题

现状（2026-10 代码核实）：

- session 之间完全不互通：没有任何机制让一个 session 创建 / 驱动另一个 session 或回收其结果。
- `project:{name}:planner` 只是特殊 key + prompt + cwd + 豁免 TTL / max_procs 的普通 session（`internal/session/routing.go` `ResolveForChat` / `plannerOpts`），不能派活。
- Agent team / Workflow 都在单个 CLI 进程内扇出，naozhi 只观测（`internal/subagent`、`internal/cli/workflow`）。
- 用户为每个 session 手动决定 workspace、node、backend、access profile、agent、model / effort、label、续接还是新开。
- 执行环境：Claude 恒带 `--dangerously-skip-permissions`，Kiro 权限请求自动 `allow_once`，Codex `approval_policy=never`；envpolicy 只过滤 env，不隔离文件系统；`mcp_config` 与 naozhi-settings 全局各一份。

owner 使用实测（附录 B）：dashboard-only、单机；一个话题一个 session、多 session 并行；主节奏"讨论 → 改吧 → 当场改 → 推送"；大任务只占 2% 的请求却带来 90% 以上的成本与大部分"继续"；跨 session 发消息 31 次——**owner 已在手工充当 Planner**。"拆开并行跑"已由 CC 在会话内完成，Planner 的价值不在扇出，而在跨会话的理解与判断、在你前面做事、替你管理注意力。

外部参照：Claude Code Projects（2026-09-17）与本设计同构（一个协调对话 + 并行 thread + "Waiting on you"），但依赖 claude.ai 登录与云端 thread。naozhi 的差异化定位：**Bedrock、多 backend（Kiro / Codex）、飞书 / 微信、本机凭证与内网系统、多节点自托管**。

## 2. 目标 / 非目标

目标

- G1 用户只面对一个对话框，用自然语言交互；斜杠命令等专业操作降到专家视图（D16）。
- G2 Planner 最懂你、最懂系统：替你管注意力，在你前面把事情准备好，在执行跑偏时提出修改意见（D39）。
- G3 主动模式：项目级 Planner 按章程与触发器巡检，在 Now / Next 边界与授权内自主推进，其余提议待批。
- G4 agent 能使用本机凭证与内部系统（邮箱、Asana、Midway、AWS），你发起的动作不逐次人工批准，但有硬边界、审计与急停。
- G5 远程分发：从任何入口把任务派到醒着或锁屏状态的 Mac 上执行（D13）。

非目标（见 §16）：Planner 之间走 A2A；依赖 MCP Sampling / Roots；社区技能市场；向量库记忆；三层以上 Planner 嵌套；独立主动推送 feed；使用量打点（D15）；成本优化（D17）；派生凭证（D42）。

## 3. 核心模型（D38）

**三个角色、两种流动、两份知识**。其余名词都只是它们的视图或字段。

```
                 懂你（用户模型）            懂系统（系统模型）
                       ▲ 读写                     ▲ 由事件流派生
   你 ◄──── 消息 ────► Planner ◄──── 事件流 ──── 所有执行 session
   │                      │ 消息（建议 / 代办 / 提问 / 叫停 / 派活）
   └──── 消息 ────► 执行 session ◄┘
```

| 类别 | 是什么 | 一句话定义 |
|---|---|---|
| 角色 | 你 | 最终的授权来源 |
| 角色 | Planner | 每个 workspace 一个，持有两份知识，承担三种职责（§4） |
| 角色 | 执行 session | 中立的执行者；带契约、血缘、产物、托管开关（§3.2） |
| 流动 | 消息 | 一切主动的沟通：谁发的、代表谁、发给谁、哪一类（§3.3） |
| 流动 | 事件流 | 一切客观发生的事实，由网关确定性记录（§3.4） |
| 知识 | 懂你 | 偏好、决定与先例、授权、关注点、节奏、边界（§3.5） |
| 知识 | 懂系统 | 每个 session 在做什么、彼此关系、环境健康、项目长期知识、外部产物（§3.6） |

**模型里刻意没有的东西**：

- **没有 Task**（D37）。CC / Codex 后端都没有这个对象，naozhi 仓库已经用 GitHub issue 当任务系统，CC 内部还有自己的 Todo。工作单元就是**执行 session + 它的血缘**；外部 tracker（issue、Asana）作为可选链接。以后若出现"一个目标同时铺开大量并行 session、需要汇总进度"的真实需求，再考虑加轻量分组。
- **没有"驾驶者"**（D36）。session 中立；权限看每条消息"代表谁"，在动作发生时判定（§3.3、§7）。

**旧名词去向**：任务板 / 全局视图 → 懂系统的展示；授权、lessons、需人决定清单、Now / Next → 懂你；提议、监护指令 → 消息种类；收件箱、早报 → 需要你处理的消息与事件按优先级排出的视图；understanding.md → 懂系统的"项目长期知识"。

### 3.1 你

- 唯一能修改"懂你"中授权、边界、Now / Next / Not-now 的角色（UI 确认）。
- 你在执行 session 里说"把 X 加进 Now"，session 只能发起确认卡片。

### 3.2 执行 session（D29、D36、D37）

中立的执行者。字段：

| 字段 | 含义 |
|---|---|
| `key` | 只表示来源命名空间（`dashboard:` / IM / `task:` / `cron:`），**不编码谁在用它**；段数与现有 4 段解析器一致（仿 scratch 补齐） |
| `workspace` / `node` / `backend` / `model` / `effort` | 启动规格 |
| `mounts` | 启动时挂载的能力（MCP 清单、透传的 env、可读路径），由 session 的用途决定（§7） |
| `contract`（可选） | 目标、验收标准、`origin`（根来源，写入后不可改）、`focus_ref`、自主度、`blocked_by`、`workflow` 策略 |
| `lineage` | `parent`（由谁派出）、`prev`（接续自哪个 session；现有 `prev_session_ids` 是雏形） |
| `artifacts` | 产物链接：PR、分支、报告、issue |
| `custody` | 托管开关与监护契约（§5.4） |
| `taint` | 是否读进过外部不可信内容（由网关按工具事件确定性标记，§7.4） |
| `state` | 生命周期状态（§5.2） |

### 3.3 消息（D36）

```
Message{ id, from, on_behalf_of, origin, chain[], to, kind, session?, body, refs[], deadline? }
```

- `from`：直接发送方（你 / Planner / 网关 / duty / 外部）。
- `on_behalf_of`：这条消息代表谁的意志。你发的代表你；Planner 发的代表"在授权范围内的你"；duty 或外部事件引出的代表它的来源（会被降级）。
- `origin`：根来源，沿链传递、只降不升（§6.3）。
- **授权在动作时判定**：PreToolUse hook 读取"当前这一轮由哪条消息触发、代表谁"，按这个身份的权限、授权、污染程度放行或拦截（§7.2）。
- **混合轮按最低权限**：CC 会在 tool 边界把排队的消息并进当前轮；一轮里只要混入非你本人的消息，这一轮按其中最低的身份执行。因此你正在对话的 session，Planner 不往里插话（§4.4）。

**消息种类**（统一一张表；收件箱映射见 §10）：

| kind | 典型发送 → 接收 | 唤醒对方 | 进收件箱 |
|---|---|---|---|
| `request` | 你 / session / Planner → Planner；Planner → 执行 session | 是 | 否 |
| `accept` / `decline` | Planner ↔ Planner | 是 | decline 时是 |
| `need_input` | Planner / session → 你 | — | question |
| `progress` | session → Planner | 否（只更新懂系统） | 否 |
| `result` / `failed` | session → Planner | 是 | 里程碑时 notify；failed 时 error |
| `query` | Planner → Planner | 是 | 否 |
| `instruction` | Planner → 执行 session（继续 / 纠正 / 回答 / 暂停 / 停止） | 是 | 否（session 内可见并带来源标记） |
| `advice` | Planner → 你（旁注）或 → 执行 session（建议） | 视对象 | 给你时 review |
| `proposal` | Planner → 你 | — | decision |
| `cancel` | 你 / Planner → 执行 session | 是 | 否 |
| `event` | 网关 → Planner（事件流的唤醒通知） | 是 | 视严重度 |

### 3.4 事件流

网关确定性记录，不由模型生成；"懂系统"由它派生。

| 事件 | 来源 |
|---|---|
| 回合开始 / 结束、结果帧 | `Sender.AfterTurn`、`SendResult` |
| workflow 阶段 / 完成 / 失败 | `WorkflowBoard.Subscribe / Running / Result` |
| 进程死亡、watchdog、resume 失败 | death reason、shim |
| 权限拒绝、模型级拒答 | `system/permission_denied`、`system/model_refusal_no_fallback` |
| **状态变更（Activity）** | 从工具事件派生：push、merge、发版、打 tag、aws 写操作、发邮件、写 Asana；以及 git / GitHub 事件（PR 打开 / 合并 / review / CI 结果） |
| 读入外部内容 | WebFetch / WebSearch、非 collaborator 的 issue / PR、邮件正文、fork 日志（用于 `taint`） |
| 环境 | Midway 将过期 / 已过期、地区 400、DNS、磁盘、限流 429、master CI 状态 |

"所有状态变更都可见"这个不变量由 Activity 落实，不依赖 session 自己登记；你在终端或 GitHub 上直接做的操作，经 git / GitHub 事件同样进入事件流。

### 3.5 懂你（用户模型，D41）

| 区 | 内容 | 谁写 |
|---|---|---|
| 偏好 | 语言（中文输出）、PR / 提交习惯、汇报方式、模型偏好 | 你明说的直接生效；从你的修改与反应中归纳的作为候选，满足升格条件后写入 |
| 决定与先例 | 你在什么情况下做过什么决定（"继续"、选了哪个方案、驳回了什么） | 网关从你的批准 / 驳回 / 回复中记录 |
| 授权 | 结构化 Grant `{能力, 范围, 次数, 过期, 来源卡片}`；charter 的常设授权引用 Grant id | 只有你（UI 确认） |
| 关注点 | Now / Next / Not-now（D14） | 只有你 |
| 节奏 | 活跃时段、回复间隔、静默时段、打扰预算 | 由事件流统计，你可改 |
| 边界 | 需人决定清单、约束（"别碰模块 X"），约束编译成 hook / 路径 deny | 只有你 |

- **建立在现有记忆体系上**：owner 的 auto-memory 里已有"只在 blocker 上请示""中文输出""naozhi 侧只留两个模型"这类偏好与边界，本来就是用户模型。迁移时导入为初始内容，并保留出处。
- **可见、可改**：每条带出处与日期；长期未被印证的条目降权并请你确认。
- **外部内容不能写入"懂你"**：issue、邮件、网页、worker 输出里的文字只能作为"懂系统"里的观察。
- 规模与整理：沿用 §8 的带教规则（每轮加载 ≤50 条、增量合并、冲突交人、过期复查）。

### 3.6 懂系统（系统模型）

| 区 | 内容 | 来源 |
|---|---|---|
| 在途 session | 每个执行 session 的目标、所处阶段、最近输出摘要、产物、健康（错误、停滞）、血缘 | 事件流派生 + Planner 摘要 |
| 关系 | 冲突（同路径 / 同 PR / 结论矛盾）、依赖、重复 | 事件流派生（路径与 PR）+ Planner 批注（结论矛盾） |
| 环境 | 凭证健康、网络 / 地区、磁盘、限流、master CI | 事件流 |
| 项目长期知识 | 决策与理由、技术债与坑、反模式黑名单、近期变化（原 understanding.md） | Planner 提议，夜间整理合并；你可改 |
| 外部 | 开放的 issue、PR、release | git / GitHub 事件 |

- 在途 session、关系、环境、外部四区由事件流**确定性**派生，Planner 只加批注，不编造状态。
- 项目长期知识只记从代码推不出来的东西（LLM 生成的 context 文件让成功率降约 3%，arXiv 2602.11988；23% 的 AI 配置文件引用已不存在的代码，arXiv 2606.09090）。每条 `claim / evidence[] / anchors[] / verified_at:<commit> / provenance / confidence`；锚点在 `verified_at` 之后被改或符号不存在 → 由确定性脚本标"未复核"。
- 你问"现在都在干什么""之前怎么定的"，Planner 回答时附出处。

### 3.7 持久化

| 状态 | 位置 | 写者 | 进 git |
|---|---|---|---|
| 消息日志、事件流 | `~/.naozhi/planner/<workspace>/` append-only | 网关 | 否 |
| 懂你（偏好、先例、授权、关注点、边界） | `~/.naozhi/planner/<workspace>/user.*`；权威副本在仓库外 | 你 / 网关 | 否 |
| 懂系统：项目长期知识 | `~/.naozhi/planner/<workspace>/knowledge/`，可选导出到仓库 | Planner 提议，网关写入 | 可选 |
| charter / duties | `~/.naozhi/planner/<workspace>/planner.yaml`（权威）；仓库内 `.naozhi/planner.yaml` 只作初始导入 | 只有你 | 否 |
| session 元数据（契约、血缘、产物、托管） | 现有 `sessions.json` 扩字段 | 网关 | 否 |
| 闹钟、路径锁、收件箱状态 | `~/.naozhi/planner/<workspace>/` | 网关 | 否 |

charter 与"懂你"的权威副本放在仓库外：Planner 派出的 session 改的就是这个仓库，任何一次合并都不能改写自己的边界（评审发现）。没有仓库的 workspace（邮箱、个人杂事）同样放在 `~/.naozhi/planner/<name>/`。

## 4. Planner：三种职责（D39）

### 4.1 模型、工具与单例

- **模型**：owner 排序的偏好表，取第一个当前凭证能调通的（`ccprobe` 实跑验证）。当前 owner 判断 opus 5.5 优于 fable 5.1，偏好表为 `[claude-opus-5-5[1m], …]`；Planner 与执行 session 同模型时以 effort 区分（Planner xhigh，D23）。弱 planner 的损失远大于弱执行者（PEAR，EACL 2026 Findings；arXiv 2603.26458）。
- **工具**：无 Edit / Write；Bash 只放行白名单只读命令（`git log / show / diff`、`gh … view / list`、`naozhi task …`），`git fetch` 由网关做；web 搜索；naozhi MCP（§6.2）。只读工具只用于核实具体说法，不用于"读一眼就判定已修好"。
- **单例与串行**：每个 workspace 一个；所有触发都是投给它的消息；单轮"看状态 → 决策 → 派活 / 建议 → 返回"，单轮设上限（如 3 分钟），从不等待。
- **状态外置**：两份知识与日志都在磁盘；Planner 进程可随时重启，重建时读 charter + 懂你摘要 + 懂系统摘要 + 最近消息（≤ 约 30k token）。进程常驻还是冷启动由网关按事件密度决定（D32）。

### 4.2 幕僚：替你省注意力

- **系统性故障只处理一次**（D44）：Midway 过期、地区 400、断网、磁盘满会同时打倒多个 session（附录 B：F 类成批出现）。Planner 识别同一根因 → 暂停受影响的 session → 只问你一次 → 恢复后统一继续。
- **等你的事排好序**：哪些 session 在等你、各等了多久、哪件最该先处理。
- **打扰预算**：每天有限的立即推送次数；超出的进早报；你专注某个 session 时，其他非紧急消息先攒着。
- **离开摘要**：你回来时，收件箱顶部是"离开期间发生了什么"（从事件流确定性渲染）。

### 4.3 顾问：想在你前面，也敢指出问题

**（a）预判：在你开口之前把事情准备好**

依据"懂你"的先例和"懂系统"的现状推测你接下来会需要什么：

| 先例或信号 | 提前做什么 |
|---|---|
| 你每次推送前都跑全量 `go test` | 你说"推吧"之前，verifier 已经跑完 |
| PR 合并后你通常发版 | 准备好 changelog 草稿与发版检查项，等你一句话 |
| 你早上先看 CI 和新 issue | 早报里已分好诊并附建议 |
| Midway 还有 2 小时过期，托管任务今晚要跑 | 现在提醒你 mwinit |
| 上周你问过某机型何时在伦敦上线 | 留意公告，一出来就告诉你 |
| 你刚结束一个架构讨论 | 把结论整理好，下次接着聊时拿出来 |

边界：默认"准备好、等你确认"，只有满足"显而易见"五条门槛（§4.5）才直接做完；每次说明理由（"因为你过去 20 次推送前都会跑"）；你连续几次无视某类预判，就停止这一类。

**（b）过程评审：worker 跑偏时提出修改意见**

只在关键节点看：刚给出计划、第一份 diff、推送或开 PR 前、verifier 失败后、长时间无进展时。看这几类问题：

- 修的是症状不是根因；
- 偏离你已知的偏好或决定（懂你）；
- 违反反模式（靠 skip 修 flaky、抬基线）；
- 与另一个 session 的改动或结论冲突（懂系统）；
- 原地打转、范围蔓延、悄悄弱化测试；
- 交接里"没做什么"与"已完成"矛盾。

意见去向（D40）：

| 情况 | 去向 | 形式 |
|---|---|---|
| 你正在和这个 session 对话 | 旁注给你，不插进 session | `advice`："这里在改症状，根因可能在 X，因为……"由你决定是否转达 |
| session 托管中，或由 Planner 派出 | 直接发给 session | 带理由的 `advice`；session 可以反驳（它可能更了解细节），分歧记录在懂系统，同一处反复分歧升级给你 |
| 严重：破坏性操作、篡改测试、安全风险 | 先叫停再给你卡片 | `instruction: 暂停` + error 卡片 |

与 verifier 的分工：verifier（新上下文、可用便宜模型）做机械检查——测试、gofmt、lint、CI、diff 闸门、default-FAIL 验收清单，回答"做到没、弄坏没"；Planner 做方案层评审，回答"做的事对不对、方法对不对"；大 diff 或高风险另派强模型、干净上下文的评审 session，Planner 只读结论。

**（c）复盘：事情结束后沉淀经验**

把"你这次怎么决定的、哪些预判被采纳、哪些意见被证明是对的"写回"懂你"的先例区；下次的预判和评审更准。

### 4.4 知识维护者

- 维护 §3.5、§3.6 两份知识；你可以随时问、随时改。
- 外部来源的内容不进"懂你"；事件流派生的部分不由模型改写。

### 4.5 介入等级与判断

| 等级 | 做什么 | 例 |
|---|---|---|
| 0 旁观 | 维护认知，不产出 | 记录每个 session 卡在哪 |
| 1 标注 | 附上信息，合适时让你看到 | "这个 PR 和另一个 session 改同一文件" |
| 2 建议 | 提出动作，由你决定 | "先合 A 再继续 B？" |
| 3 代办 | 做掉显而易见的下一步 | 停下时说"继续"、出错重试、rebase、重跑 CI、用已知事实回答澄清 |
| 4 承包 | 被委托的结果端到端负责 | "处理完所有 review issue" |
| 5 护栏 | 发现出问题先停下再告诉你 | 失控循环、破坏性操作、系统性故障 |

判断式：**介入能省下的注意力 + 能避免的风险 > 做错的代价 + 打扰你的代价** 才介入。可逆、有把握、在授权内 → 代办；不可逆或有不止一个合理选项 → 建议；价值低 → 标注进早报；危险 → 护栏（唯一不经请求就主动打断的介入）。

**"显而易见"五条门槛**（代办必须全部满足，缺一条降为建议）：

1. 有先例：你在同类情况下做过同样的决定（附录 B：你说"继续"后 79% 下一轮有产出）。
2. 只有一个合理的下一步。
3. 可逆。
4. 与 session 契约的目标和验收一致。
5. session 自己提过这一步（"接下来我会做 X"，然后停住）。

**介入配置**：写在 charter 里，按场景区分允许的等级、打扰预算、静默时段、推送渠道、先例来源。

| 场景 | 代表谁 | 介入重心 | 特别处理 |
|---|---|---|---|
| 单人开发 / SA 工作（owner 默认） | 你 | 代办与承包为主，替你盯长任务 | 你在对话时只旁注 |
| 团队群聊 | charter 所有者，同时尊重每个请求者 | 标注与汇总；协调冲突请求 | 权限按发起请求的人算；不拿 A 的授权做 B 的事 |
| 值班 / 运维 | 值班人 | 关联告警、执行 runbook | 打扰门槛极高；只有 runbook 预批准步骤可代办；回滚只建议 |
| 调研与知识工作 | 你 | 标注为主：共享上下文、发现结论矛盾、提醒遗留问题 | 几乎不代办 |
| 个人事务（邮件、日程、报销、Asana） | 你 | 批量、起草、盯截止时间 | 输入多为外部内容；默认起草不发送 |
| 长跑批 / 基础设施 | 你 | 监控进度与资源、失败重试 | 停掉忘关的资源属护栏级 |
| 多人共用 bot | 每个使用者各自一个 Planner | 严格隔离 | 不跨用户可见或代办 |

### 4.6 激活：什么时候思考（D31）

事件驱动，不做心跳轮询（避免噪声与为忙而忙；人审 agent PR 平均 42.9h，审查时间才是瓶颈）。

| 唤醒来源 | 例 | 时机 |
|---|---|---|
| 被动 | session 交来的活、session 结果、事件流异常、别的 Planner 的请求 | 立即 |
| duty | CI 巡检、flaky 清扫、issue 分诊 | cron / 外部事件；侦察先跑，有发现才唤醒 |
| 空闲拉活 | 从 Now / Next 取下一件 | 容量空出来时（PR 合并、你审完一批） |
| 自排闹钟 | "30 分钟后看 CI" | Planner 设定的时刻 |
| 预判信号 | 凭证将过期、你按习惯即将需要某样东西 | 事件流触发 |

拉活门槛：你待审的 Planner 产出 PR ≤ K（默认 3）、在途未满上限、所取的活落在 Now / Next 里。

### 4.7 核心决策：新工作交给谁（D33）

新工作到来时，Planner 决定交给谁；决定与理由写入懂系统，你可以改判。

| 选择 | 适用 | 动作 |
|---|---|---|
| 通知已有 session | 同一目标或同一产物，且该 session 满足复活条件（§5.2） | `request` / `advice` 发给它；你正在对话的 session 改为旁注给你 |
| 排在已有之后 | 不同目标但同路径 / 同 PR | 新 session 的契约写 `blocked_by` |
| 取代 | 新要求推翻旧的（"别做 A 了，改做 B"） | 旧 session `parked(superseded)`，处置其产物，新 session 接手 |
| 另开 | 独立目标、不冲突、需要不同挂载 / 节点 / backend，或已有 session 不满足复活条件、带有 `taint` | 新开执行 session，附结构化交接 |

"通知"在复活条件不满足时自动退化为"另开 + 交接"。

### 4.8 判断：打分与说不

> 优先级 ≈ 价值 × 与 Now / Next 的契合度 × 置信度 ÷（执行成本 + 你的评审成本）

- 说不的硬规则：与开放 PR / issue / 在途 session 重复；在 Not-now 里或 N 天内被否过且无新证据；写不出验收标准的只派调查不派修复；你待审 PR 超过 K 个就停止开新 PR；连续 3 次失败停手上报。
- "什么都不做"是合法输出；SLO 计入你拒绝与 revert 的比例。
- Now / Next / Not-now 是网关执行的边界（D14）：Planner 自发的工作必须挂到 Now / Next（`focus_ref`），否则只能提议；落在 Not-now 的一律拒绝，除非来自你本人；你直接提出的请求不受此限，但 Planner 提示"要不要加进 Now / Next"。

## 5. 执行 session

### 5.1 选型、交接与执行形态（D27）

- 由发起者（你，或 Planner 按任务）选 model / effort / backend / 挂载 / 节点；Planner 派出时常规活 sonnet，难活 opus。
- Planner 派出的 session 各用一个 git worktree + 分支（#3449）；路径重叠由 Planner 决定排后（§4.7），网关只对同一 PR / 分支做硬互斥。
- 结束时返回结构化交接：做了什么 / 没做什么 / 命令与退出码 / 发现的问题 / 经验候选；允许显式"不可完成"退出（无诚实出口时 agent 倾向作弊）。

| 形态 | 适合 | 谁决定 |
|---|---|---|
| session 内直接做 | 交互式、小、依赖上下文 | session |
| CC Workflow（会话内扇出） | 同一 workspace 内同质可并行的批量，汇总成一个产物 | session 自行决定，受契约 `workflow: auto / allowed / forbidden` 约束；你说"用 ultracode / workflow"直接生效 |
| 多个执行 session | 每个子项有独立产物（各自 PR）、需单独追问、跨 workspace / 节点 / 凭证、寿命超过一个会话 | Planner |

- 交给 Planner 的判据（任一）：预计多个独立 PR、跨 workspace、你要离开且希望它推进到底。
- 大任务交给 Planner 后默认自主推进到完成，按里程碑推送，只在 blocker 与需人决定清单上找你；委托任务的自主度等于委托范围，与 duty 档位无关。

### 5.2 生命周期（D18）

| 状态 | 进程 | 上下文 | 进入条件 |
|---|---|---|---|
| running | 活 | 在用 | 正在跑一轮 |
| warm | 活 | 缓存有效期内 | 等 verifier 或下一步，不超过缓存 TTL |
| awaiting | 释放 | 可 `--resume` | 绑定产物，等 review / CI / 你 |
| dormant | 释放 | 可 `--resume` | 产物仍开着但无订阅 |
| parked | 释放 | 可 `--resume` | 被抢占（`preempted`）或被取代（`superseded`），带 `resume_on` |
| archived | 无 | 不再恢复 | 产物关闭；只留交接与 transcript 引用；worktree 删除 |

- 寿命跟产物走：PR session 到 PR 合并 / 关闭 + 24h；报告类到被采纳或空闲 7 天；侦察 / verifier 完成即归档；你 pin 的可延长（上限 30 天）；没改过任何状态、空闲 2 小时的问答 session 自动折叠。
- 复活条件（全部满足才复活，否则另开 + 交接）：同一产物；上次上下文 ≤ 约 100k token；休眠 ≤72h；CLI / backend 版本不变；无 `taint`。
- 进入 dormant / parked 前必须 commit + push WIP；远端分支是真相源。
- 外部事件唤醒：只认有写权限者的 review 与 CI 失败；同类合并；忽略自己的 push 与重复绿灯；连续 3 次修复失败或分支出现人工 commit 即停止自动唤醒并交回 Planner。
- 缓存 TTL：你正在对话的 session、Planner、等 CI 的 PR session 用 `promptCacheTtl: "1h"`（Bedrock 已实测生效，`ephemeral_1h_input_tokens=15522`），其余 5 分钟。
- 上限：每 Planner `max_workers`、`task:` 命名空间配额（现有豁免总池 20 已被 cron / project / sys 分满，需新增配额）、主机级上限；进程树实测约 640MB。

### 5.3 人为打断沿用 CC 原生逻辑（D30）

- 你在 session 运行中发的消息直通 CC 自带的 command queue（tool 边界 mid-turn drain、`priority` 字段，已实测 `priority: next` 在同一回合内被吸收）；"停下"走 CC 的 interrupt control request，立即生效、不弹卡片；"丢弃上下文 / 删除"才确认。
- **事实订正**：passthrough 不是默认模式，默认是 `collect`（`internal/config/defaults.go:27`），本机当前即 collect；模式是全局一份。需要改为**按 session / 按来源**设置：你直接对话的执行 session 走 passthrough，Planner 的收件队列由 naozhi 定语义（优先级、同类合并）。Kiro / Codex 无 CC command queue，降级为 collect。
- 撤回排队中的消息用 interrupt 的 `cancel_queued`；已进 CC 队列的 Planner 消息不能靠 naozhi 撤回，所以 Planner 消息由网关在回合边界投递。

### 5.4 托管：替你盯着（D25、D26、D43）

托管是 session 上的开关：启动 workflow 时自动打开、你说"你盯着"时打开、Planner 派出的 session 默认打开；可一键关闭。**"你在不在旁边"不是单独的状态**，只是托管里的规则：你正在看（焦点 + 页面可见）或最近 N 分钟发过消息时，托管只提示、不插话。

依据附录 B：编排类会话里约 85% 的催促可由机器代劳；对话类会话里 C（真 blocker）+ G（完成后改需求）合计 65%，本就需要你。所以托管默认只作用于有契约或有 workflow 在跑的 session。

**三层检测**：L0 网关规则（不用模型）；L1 停顿判读（一次性助手，sonnet low，只输出分类枚举，输入里去掉 tool_result；附录 B 的 305 条标注作评测集）；L2 Planner 判断。

| 停顿原因 | 处理者 | 规则 |
|---|---|---|
| E 做了一部分就停、没提问 | L0 | 仅当有契约或 workflow 已结束、结尾不是问句、后台无任务在跑 → "继续，还剩这几条验收项：…" |
| F 出错 | L0 | 超时 / DNS / 5xx：1、2、4、8 分钟退避；地区 400：先探测出口再续，计入"同类错误 5 次"；显式 deny、429：不重试直接上报；中途截断：先查磁盘与进程再续；`model_refusal_no_fallback`：不重试、不换措辞重派，交你 |
| A 问已授权的下一步 | L1 | 下一步能匹配契约或计划中的某一步才自动批准 |
| D 等后台 workflow | L0 | 不唤醒、不催；里程碑推送；2 小时无里程碑推心跳；静默 >60 分钟且最后一条是 API Error → 报警 |
| B 给选项等选择 | Planner | 在授权内按"显而易见"门槛选推荐项并记录理由；落在需人决定清单的一律问你 |
| C 真 blocker | 你 | 同一 session 的多个问题合并成一张卡片一次问完；不受阻的部分继续 |
| G 完成后你改需求 | 你 | 不自动处理；语言漂移加确定性检查兜底 |

**需人决定清单**（默认，可增删）：发版或打 tag；改分支保护；修改 epic 验收标准或 `docs/` 下的设计文档；合并来源不明的 PR；动 Now / Next / Not-now；重启或升级 naozhi 服务本身。**落地为动作级 PreToolUse hook**，对所有非你本人发起的回合生效（含 L0 的自动"继续"与 L1 的自动批准），不只约束 Planner 的选择；你亲口说的照做。

**防循环**（按血缘链累计，另开 session 不能绕过）：

- 连续 3 次自动"继续"无进展 → 停下找你；进展只认确定性信号：新 commit / PR 更新 / workflow 进入新阶段 / 验收项变绿（报告类任务以产物更新为准）。
- 同一血缘链最多自动续 8 次（owner 实际最长连续催促）。
- 同类错误连续 5 次 → 停止并通知。
- 同一条指令发两次无效 → 升级；session 自己关掉的 PR 记为失败。

### 5.5 撞车（D35）

| 层 | 情形 | 处理 |
|---|---|---|
| 1 | 你对运行中的 session 又说一句 | CC 原生逻辑（§5.3） |
| 2 | 新事情与手上的事不相干 | 新开 session 并行；容量满按优先级排队 |
| 3 | 新事情与在途事情冲突 | Planner 的核心决策（§4.7） |
| 4 | 资源不够（节点、配额、你的注意力） | 按优先级抢占 |

优先级（高 → 低）：你说"先处理这个" → 你的普通请求、master CI 变红 → 在途 session 的 blocker 与异常 → duty 发现 → 空闲拉活。抢占只停车（`parked`），不杀；你正在对话的 session 不被自动抢占；Planner 同类消息合并（3 个 `ci_failed` 只处理一次）；Bedrock 配额紧张时你发起的 session 优先。

## 6. 消息投递与 naozhi MCP

### 6.1 投递

- 执行 session 的输入：你的消息走 passthrough（§5.3）；Planner / 网关的消息经新的 `envelopeOrigin` 走 `turn.Orchestrator.Submit`，在回合边界投递。
- Planner 的收件队列：naozhi 定语义——优先级（你 > 异常 > 结果 > duty > 拉活）与同类合并。**事实订正**：现有队列是 FIFO 无优先级，`PriorityNow` 直接绕过队列（`internal/turn/queue.go`、`orchestrator.go:34`），加优先级需要改 Orchestrator。
- 消息日志 append-only；报告传摘要 + ref，全文落盘，Planner 需要时 `read_result`。

### 6.2 naozhi MCP（#3448(b)）

- `naozhi mcp` stdio 桥经 unix socket 连回 naozhi；能力令牌经继承的 fd 或 0600 文件传递（不走 argv / env，防同 uid `ps` 读取），绑定首个连接的 PID。
- **句柄式、不阻塞**：`dispatch(...) → {session}` 立即返回；完成以 `result` 消息唤醒 Planner。**删除 `await_run`**：CC 的 `MCP_TOOL_TIMEOUT` 是硬墙钟，progress 通知不续命，且阻塞期间你插的话要等 tool 返回。
- 不用 Sampling / Roots（已弃用）；`ask_user` 是普通工具，由 naozhi 路由到 dashboard / IM。
- 工具按角色（统一命名）：

| 角色 | 工具 |
|---|---|
| 执行 session | `system_status`（读懂系统摘要）、`handoff_to_planner`、`report_progress`、`report_result`（结构化交接与"不可完成"） |
| Planner | `dispatch`、`send`（request / advice / instruction）、`cancel`、`park`、`propose`、`ask_user`、`set_alarm`、`session_status`（上下文规模、版本、空闲时长）、`read_result`、`request_planner`、`knowledge_propose`、`journal_append` |

- `tools/list` 固定顺序（利于 prompt cache）；限额（并发、PR 数、每日派发数、跳数）由网关强制，不靠 prompt。
- 原型期可先用 Bash 白名单下的 `naozhi task …` CLI 调本机 REST，再换 MCP 桥。

### 6.3 多 Planner 协作（D19）

Planner 之间不直接对话，只互投 `request`，由对方决定接不接；各自只对自己的 workspace 负责。

- 状态：`request{goal, acceptance, why, deadline, origin, chain[]}` → `accept` / `decline{reason}`（终态，只能改写重投或升级给你）/ `need_input` → `progress` → `result` / `failed`；`query` 只读不建工作。
- 归属：每个 Planner 在 charter 声明 `accepts:`；发起方不能在对方 workspace 派 session；lead 由发起方指定，网关只校验 `accepts:`；目标 workspace 没有 Planner 时退回给你。
- provenance 只降不升：`origin` 写入后不可改，有效信任取整条链最低值；外部原文只作带不可信标签的 ref，不进 `goal`。
- 防环：最多 2 跳；目标已在 `chain` 中即拒绝；按（origin, target, 意图哈希）去重；每对 Planner 限速，被弹回 3 次熔断；每个请求必须有 deadline。
- 否决的方案：直接对话、黑板自愿认领、contract-net 竞标、handoff 移交对话、同步调用。

## 7. 权限与凭证（D42）

### 7.1 原则

- **你发起的动作不逐次人工批准**（D6 的限定表述）；Planner / duty 发起的自主工作按自主度档批准"计划"；需人决定清单是唯一的逐项例外。
- 约束由网关、hook、settings 的 deny / allow、MCP 挂载、沙箱执行，不写在 prompt 里。
- **不做派生凭证**（D42）：当前场景不需要 STS / GitHub App 等派生身份。

### 7.2 授权判定：按消息代表谁

- session 启动时挂载**它的用途需要的能力**（§7.3）；启动后能力集合固定（CC 的 settings / MCP / 沙箱在 spawn 时定死）。
- 每个动作由 PreToolUse hook 按当前轮的 `on_behalf_of` 判定：你 → 挂载范围内全部放行（除硬 deny）；Planner → 只放行授权（Grant）覆盖的动作类，需人决定清单内的拦下转卡片；外部来源 → 降级（不放行对外写、云写、发版）。
- 混合轮按最低身份（§3.3）。
- 需要更多能力时（例如 Planner 派出的 session 要发版），由你在卡片上授予 Grant，或另开一个挂载更大的 session——不在运行中给进程"升档"。

### 7.3 薄 Broker：凭证清单 + 挂载策略

初期的 Broker 不签发 token、不起常驻服务，只有两样：

1. **凭证清单**：机器上有哪些秘密、在哪、属于哪类（本机盘点见下表，只记名字与类型）。
2. **挂载策略**：按 session 用途生成 mcp-config（`--mcp-config` + `--strict-mcp-config`）、env 透传（envpolicy）、settings 的路径 deny、沙箱规则。

| 类别 | 本机现状（2026-10-18 盘点） | 挂载策略要点 |
|---|---|---|
| AWS | `~/.aws/credentials` 2 段长期静态密钥；SSO 缓存；1 个 `credential_process`；awsCredentialExport 走 Isengard；shell rc export 了 `AWS_BEARER_TOKEN_BEDROCK` | Planner 派出的 session 不挂 admin profile；CLI 自身取 Bedrock 凭证的路径必须保留 |
| Midway | `~/.midway/cookie` + client cert 配置 | 只经内部系统 MCP 使用；外部来源轮次不放行写 |
| SSH | `id_rsa`、`id_ed25519`、cert、活的 ssh-agent | `SSH_AUTH_SOCK` 只透传给需要 git over ssh 的 session |
| GitHub | gh token 在钥匙串（owner 身份） | 见 §7.5 的剩余风险 |
| API key / 应用密钥 | `mcp.json` 的 Exa key；naozhi `cookie_secret`、dashboard token | session 不可读 `~/.naozhi/*`；含密钥的备份已清理（2026-10-18：删除 `mcp.json.bak-*`，`config-local.yaml.bak-单机化前` 的节点 token 已打码） |
| 其他 agent CLI | `~/.codex/auth.json`、`~/.kiro`、`~/.claude/.amzn` | 只给对应 backend 的 session |
| 将来 | 邮箱（优先 OAuth / Midway 支撑的 `aws-outlook-mcp`，不存密码；若需密码只进钥匙串）、其他内部系统 | 读邮件的 session 自动 `taint`；"发信"是单独能力 |

**只有人能做的事永远留给人**：mwinit / FIDO、MFA、首次授权、轮换——对应 C 类 blocker，收件箱只提示"需要你去做 X"。

### 7.4 污染标记

- 网关按工具事件确定性地给 session 打 `taint`：WebFetch / WebSearch、非 collaborator 的 issue / PR、邮件正文、fork 日志、第三方 MCP 返回。
- 带 `taint` 的轮次发起的派活只能是"提议"，能力封顶读仓库 + 改分支；外部原文只作 ref，不进契约的目标与验收。naozhi 仓库是公开的（4 个 fork），issue-triage 读到的外部 issue 就是这类输入；只有 collaborator 提的 issue 能进入 execute 档 duty。
- 读写分离（D7）的完整版仍推迟到邮箱 Planner；污染标记是它的前置。

### 7.5 剩余风险（不做派生凭证的代价）

Planner 派出的 session 仍用 owner 的 gh token，GitHub 侧的 T2 / T3 区分做不到，只能靠 hook 拦截与仓库设置。四路评审核实的事实：仓库公开、master 合并批准数为 0、release 非 immutable、自升级签名公钥 `trustedSigKeys` 为空（`internal/selfupdate/signature.go:23`）、自动更新默认开启。可选缓解（待 owner 决定，§17）：开启 immutable releases 与 push protection、填签名公钥、Planner 运行期间自动更新改为只通知、hook 拦截 `gh release *` / tag 推送 / 分支保护 API / 重启升级 naozhi、安全关键路径（`.github/**`、`internal/planner/**`、hook 与 envpolicy、selfupdate、`scripts/`、`tools/`、`go.mod`）进 CODEOWNERS 且 Planner 自发工作只能提议。

### 7.6 执行环境

- auto mode 替代 `--dangerously-skip-permissions`（#3686 试用，可切回 bypass；附录 A）：须在 CLI 加载的 settings 里声明 `autoMode.environment`。auto mode 不是安全边界，且把请求里的明确指令当作用户意图（附录 A：soft_deny 所列的删远程分支在明确要求下被放行），挡不住 Planner 下达的指令——不可逆约束必须落在 deny / hook。
- 沙箱：**第一步用 CC 原生 sandbox（只管 Bash）+ settings 的 Read deny + 按 session 挂 MCP**。整进程 srt 推迟：评审核实它会弄坏 CLI 取 Bedrock 凭证、写 transcript、MCP 子进程读 Midway cookie，Linux 上 cgroup 收养校验 exe 也通不过。
- 分类器拒绝、连续拒绝导致的终止、模型级拒答是三种新的错误类别，进收件箱 error，不自动重试。
- 急停分层：所有 hook 读 kill 文件一律 deny → 冻结 dispatch / cron / 闹钟 → 关闭 Planner 名下 PR 的 auto-merge → 按 ppid 遍历进程树（shim 用 Setsid，进程组 SIGSTOP 打不全）→ SIGKILL 兜底；须演练。
- 快照：派出前 `refs/naozhi/<session>`；CC checkpoint 不追踪 Bash 与子 agent 的改动。
- 脱敏：所有出口（IM、GitHub 公开写入、产物、日志、trace）统一过脱敏器，补 JWT（`eyJ` 且两个点）、Midway cookie、JSON 形态的 STS `SessionToken`（`IQoJb3JpZ2lu` 前缀）、12 位账号 ID 与内网域名；向公开仓库写入前检查。

## 8. 带教：你的纠正如何沉淀（D20）

"懂你"里偏好、先例、边界的写入规则：

- 信号强度：强 = 显式纠正、带理由的拒绝、7 天内被 revert；中 = 对草稿的修改（从 diff 归纳偏好）；弱 = 批准、无理由拒绝、👍/👎。
- 按类型落地：约束 → 编译成 hook / 路径 deny；偏好 → 进 prompt（限定 scope）；事实 → 带代码锚点、用前校验、28 天未用删除；流程 → 只从验证通过的工作提炼成 `SKILL.md`（SkillsBench：事前自写技能 −8.1pp）。
- 条目字段：`rule(≤2 句), kind, scope, strength(must|should|hint), source, origin, evidence[], created, last_confirmed, last_applied, review_by, helpful/harmful, status`。
- 整理：只做增量（ADD / UPDATE / MERGE / RETIRE），隐式经验独立出现 ≥3 次才升格；不可信来源结构上排除；冲突只标记交你；should / hint 30 天未用转 dormant，must 到 `review_by`（默认 90 天）请你确认；夜间整理逐条小改、不整篇重写（ACE：整篇重写把 18k token 压成 122 token，准确率跌破无记忆基线）。
- 规模：每轮加载 ≤50 条（IFScale：100 条 94.6%、250 条 67.9%）；约束在 hook 执行不占预算；超预算强制整理。
- 记忆隔离：CC auto-memory 按仓库共享，Planner 派出的 session 默认 `autoMemoryEnabled:false`，Planner 只经 `knowledge_propose` / `journal_append` 写；PreCompact hook 压缩前把未决决策写进 journal。

**自主度升降档**（duty × 动作类别分别计，升档由你确认；阈值按"三法则"推算，是建议值）：propose → draft：≥20 次提议、批准率 ≥90%、实质修改 ≤10%；draft → execute：连续 30 次 verifier PASS 且无实质拒绝、7 天 revert 为 0；自动降一档：任一 execute 被 revert、触发 diff 闸门、最近 10 次被拒 ≥2 次。每天一次的 duty 攒够样本要几个月，初期由你人工升档。

## 9. 主动模式：charter 与 duties

主动 / 被动只是 Planner 有没有 duties（D5）。

```yaml
# ~/.naozhi/planner/naozhi/planner.yaml（权威副本，仓库外）
charter:
  scope: 维护 naozhi 仓库健康：CI 绿、flaky 收敛、依赖安全更新、issue 分诊
  escalation: 超出授权的一律提议；连续 3 次失败停手上报
intervention_profile: solo-dev        # §4.5 介入配置
accepts:
  - { from: "planner:*", kinds: [request, query], max_autonomy: draft }
duties:
  ci-watch:     { trigger: "event:github.ci_failed", fallback_cron: "*/30 * * * *", autonomy: execute, priority: high }
  flaky-sweep:  { cron: "0 3 * * *",  autonomy: propose }
  issue-triage: { cron: "0 9 * * *",  autonomy: propose }
  dep-audit:    { cron: "0 10 * * 1", autonomy: propose }
grants: [g-flaky-pr, g-dep-patch]      # 引用"懂你"中的结构化授权
limits: { max_workers: 4, max_dispatches_per_day: 40 }
```

- charter 只保留四要素（scope / triggers / approval gates / escalation），只有你能改；学到的规则进"懂你"（§8），不进 charter。
- schedule 缺省 = 关闭；调度时间放在 naozhi cron；事件优先、轮询兜底；事件按来源 ID 去重，duty 限速，cron 加 jitter。
- 巡检由轻量侦察 session 执行，默认静默，有结论才唤醒 Planner。
- 自主度 propose → draft → execute，默认 propose（D10）。
- 第一个主动 Planner 落在 naozhi 仓库（D8）；Mac 没有公网 webhook 入口，单机阶段 CI / issue 事件靠 `gh` 轮询兜底。

## 10. 交互

- **主视图**：一个输入框；执行 session 以卡片出现在主对话里；输入框常显"→ 发往 X"。默认发给当前焦点 session；焦点 session 正在运行、而新消息不像短跟进时，先用一次性助手判断归属（"这像是新话题，要另开吗？"）；有"新话题"快捷键。
- **新话题的 workspace**：继承焦点 session → 从文本推断并以 chip 显示、可改 → 默认 scratch 目录。
- **收件箱**：分组 Waiting on you / Ready for review / Working / Landing / Idle / Resolved（一周无活动转入）；条目 notify / question / review / decision / error，带归属 Planner、workspace、等待时长；**只有 notify 会在条件消失时自动关闭，其余只有你能关**。早报与离开摘要是收件箱的分组，不是另一个视图。
- **卡片**：带请求 ID，校验发送者本人；首个回答生效；应用决定前重读待决项。
- **可解释**：每条代办 / 注入消息带原因码与计数（"⚙ 自动继续 · E · 第 2/8 次 · 剩余验收项…"）；每个 session 显示托管 chip；Planner 卡片显示"被什么唤醒、做了什么决定、理由"；全局暂停开关。
- **推送渠道**：dashboard 目前只注册了 service worker，没有 Notification / Web Push，IM 未接（`platforms: {}`）。Mac 用浏览器 Notification；手机渠道待定（飞书 bot 单聊 / PWA Web Push，§17）。
- **专家视图**（D16）：斜杠命令、会话侧边栏、新建会话对话框、tuning 弹层、`/cd` 等保留为**视图**，操作对象仍是执行 session，一键可达、长期保留。
- 团队群聊保持隔离，不并入个人对话框；IM 上无"焦点"，以所在话题为归属。

## 11. 多节点（D13）

- 阶段一单机 Mac；终态 **EC2 作为控制面**（消息与事件日志、两份知识、网关、cron duty、Planner），**Mac 作为持凭证的执行节点**；锁屏时照常运行，睡眠时派往它的工作进入 `blocked(waiting_node)`；Mac 跑活期间 `caffeinate -i -w <pid>`。
- 现状：反连 `/ws-node` 只能 primary → 节点；`send` 线路上没有 backend 字段（dashboard 给远程节点选的 backend 不生效）；`send` 不幂等、无 turn id。
- 节点能力清单（新 RPC `fetch_manifest`）：`always_on`、power、backends、repos（按 remote）、凭证类别标签与新鲜度、capacity、本地 policy。
- 放置：`requires`（硬）/ `prefers`（软）/ `pin`；只有离线节点满足时 `blocked(waiting_node)` + `max_wait`；凭证过期视为不满足。
- `dispatch-v1`：`dispatch{run_id, session, repo, backend, model, effort, mounts, message, deadline, lost_after}`，节点先落盘再启动，同 `run_id` 幂等；`run_update{run_id, seq, state, handoff}` + `run_ack`，节点发件箱持久化重放；`list_runs` 对账；`cancel_run`；**`steer{run_id, text, priority}` / `interrupt`**，供你直接对话 Mac 上的 session。`side_effects` 由节点按挂载推导，不信发起方。
- 离线：断线 >60s suspect → 超过 `lost_after`（Mac 2h、EC2 15m）unknown → 只读的可换节点重跑，有副作用的进收件箱由你决定；重启后托管中的 session 一律 suspect，先对账不自动续。
- 安全：Mac 端 `upstream.mode: dispatch_only` 关闭裸 `send` / `update_config` / takeover；节点本地 policy 由节点说了算；凭证永不过线；dispatch_only 上线前 EC2 不连 Mac。

## 12. 可观测

- naozhi 侧 span 为主：入口 → 执行 session → Planner → 派出的 session，用 `gen_ai.*` 命名，同时作审计账本。**事实订正**：CC 读 `TRACEPARENT` 只在进程级，长驻进程做不到按回合传播。
- 每个 duty 的 SLO：verifier 通过率、返工率、7 天内被 revert 的比例。
- 回归评测集：naozhi 历史 CI / flaky / 分诊案例 20–50 个，加附录 B 的 305 条停顿标注，换模型 / prompt / 工具面时重跑。
- 成本账本照常记录，但不作为门槛（D17）。

## 13. 与现有代码的映射（评审核实，基于 `968447cd`）

| 现有 | 演进 | 核实要点 |
|---|---|---|
| `project:{name}:planner` | Planner 身份 | 保留 key 语义 |
| `.naozhi/project.yaml` planner_model / planner_prompt | 导入到仓库外的 `planner.yaml` | — |
| cron | 新增目标"给 Planner 投 duty 消息" | cron 不走 Orchestrator，自有端口 |
| `turn.Orchestrator` Origin 端口 | `envelopeOrigin` / `supervisorOrigin` | Origin 端口够用；**优先级需改 Orchestrator**（队列 FIFO） |
| queue mode（全局 `collect`） | 按 session / 来源设置 | 默认 collect，非 passthrough |
| `ArgvSpawnOptions` + `driftCompareArgs` | 每 session 启动规格进 `shim.SpawnOverlay`，drift 从 overlay 读 | **不改会在重启时误杀所有存活 shim**（`spawn_drift.go:47` 用全局值重建 argv） |
| scratch pool、`ReleaseIdleProcess` | 执行 session 模板 | `ReleaseIdleProcess` 只对 exempt key，且只有 cron 调用 |
| `prev_session_ids`、`resolveResumeID` | 血缘 `prev` | `resolveResumeID` 失败静默开新 session，需改为显式交接 |
| 豁免配额（cron 12 + project 5 + sys 3 = 20） | 新增 `task:` 配额 | 总池已分满 |
| budget `SubjectForKey` | `task:` 归属到 Planner | `task:` 现会落到 `""` |
| `cli.mcp_config`、naozhi-settings（全局） | 按 session 生成 | CLI 支持 per-process `--settings` / `--mcp-config`；argv 白名单需配合 |
| `Sender.AfterTurn`、`WorkflowBoard`、`SendResult`、death reason、eventlog | 事件流 | 均已存在 |
| `group_scope: thread` + `thread_auto_open` | IM 话题 = 执行 session | 只有 Slack、飞书支持；不开 auto_open 时顶层进群级 session |
| `PermissionMode`（Default / Standard） | 新增 Auto（#3686） | 仍在分支上 |
| node / upstream `send` | `send-backend` → `fetch_manifest` → `dispatch-v1` | `send` 无 backend、不幂等 |
| 4 段 key 解析器（`routing.go:162`、`routes.go:310` 等约 10 处前缀判断） | `task:` 补齐为 4 段 | 3 段会被拒收 |

新包暂定 `internal/planner`（消息与事件日志、两份知识、charter / grant 校验、放置、托管）。

## 14. 现有功能的去留与迁移（D15、D16）

| 功能 | 新模型角色 | 去留 |
|---|---|---|
| `/new` `/clear` `/stop` `/urgent` | 从自然语言识别同类意图；打断走 CC 原生 | 命令降到专家视图 |
| `/cd` `/pwd` `/model` `/effort` `/backend` | 选型由发起者决定 | 专家视图 |
| `/project` | 绑定入口到 Planner | 并入 Planner |
| `agent_commands`（`/review` `/research`） | 执行 session 的挂载模板 | 转为模板，命令退役 |
| `/cron` 与 cron 面板 | duty 触发器 | 合并，两种目标并存 |
| 会话侧边栏 / 项目分组 / 收藏 | 专家视图；收藏 → Planner 列表 | 长期保留 |
| scratch 追问抽屉 | "追问此 session" | 底层池复用，抽屉 UI 退役 |
| workflow 面板 / agent-team 下钻 | 懂系统的展示 | 保留并升级 |
| 接管终端会话 | 把终端会话收编为执行 session | 保留（单独 provenance 标记） |
| 文件视图 / cc 资产浏览器 / 记忆弹层 | 产物与知识查看 | 保留 / 合并 |
| 多节点 | 放置 | 保留并升级 |
| 成本 / 预算、语音、附件、webhook、sysession | 照常；sysession 与侦察 session 同构 | 保留 |

迁移：M1 并存（`planner.enabled` 默认关，按 workspace opt-in，旧 key 语义不变，`naozhi planner migrate` 默认 dry-run 导入 planner 配置与 auto-memory 中的偏好）→ M2 一个对话框视图与专家视图切换（已有安装默认保留侧边栏）→ M3 cron 合入 duty、scratch 入口改为"追问此 session" → M4 Planner 模式成为默认后退役 scratch 抽屉 UI、`/review` `/research` 绑定、planner 专属 routing 分支。

## 15. 分阶段

| 阶段 | 内容 |
|---|---|
| P0 验证 | ① PreToolUse hook 能否拿到当前轮的 `on_behalf_of` 及判定延迟；② 每 session 启动规格的重启存活测试（drift）；③ queue mode 按 session 设置；④ L1 停顿判读用附录 B 305 条离线评测；⑤ auto mode 试用（#3686）；⑥ CC 原生 sandbox + Read deny 下 CLI 取凭证、写 transcript、MCP 正常；⑦ 推送渠道打通 |
| P1 编排内核 | 消息与事件日志、`envelopeOrigin`、`task:` 命名空间与配额、执行 session 契约 / 血缘 / 生命周期、托管 L0 + L1 与里程碑推送、需人决定清单 hook、防循环、急停与快照 |
| P2 Planner 与知识 | 懂系统（确定性派生 + 批注）、懂你（导入 auto-memory、Grant、Now / Next）、Planner 幕僚与顾问职责（预判、过程评审）、核心决策、naozhi MCP、worktree、verifier 与 diff 闸门 |
| P3 交互 | 一个对话框、归属提示、收件箱与离开摘要、卡片、可解释、专家视图 |
| P4 主动模式 | charter / duties / 事件触发 / 自主度 / SLO；首个落在 naozhi 仓库 |
| P5 带教 | 带教回路、夜间整理、技能提炼、过期检测 |
| P6 多节点 | EC2 控制面 + Mac `dispatch_only`、`fetch_manifest`、`dispatch-v1`、离线状态机 |

评审建议的另一种排法（先只做托管 MVP、不建 Planner，1–2 周可 dogfood）列为待定（§17）。

## 16. 明确不做

| 不做 | 原因 |
|---|---|
| Task 对象 | D37：后端没有、GitHub issue 已是任务系统；session + 血缘足够 |
| "驾驶者"属性与驾驶者切换 | D36：权限按消息代表谁判定 |
| 派生凭证（STS / GitHub App 等） | D42：当前场景不需要；剩余风险见 §7.5 |
| Planner 之间走 A2A | 同进程消息更简单、可审计 |
| 依赖 MCP Sampling / Roots / server 端 elicitation、`await_run` | 已弃用或在 CC 下不可行 |
| 整进程 srt（初期） | 与 CLI 取凭证、transcript、MCP 子进程冲突 |
| Claude in Chrome 访问内网 | 需 claude.ai 登录 |
| 社区技能市场、向量库记忆 | 风险高 / grep + 文件够用 |
| 三层以上 Planner 嵌套、超过 2 跳 | 官僚化与转发死循环 |
| 独立主动推送 feed、心跳轮询 | Pulse 下线；噪声 |
| 使用量打点后再决定去留 | D15 |

## 17. 待定问题

- **路线**：按 §15 推进，还是评审建议的"先托管 MVP、不建 Planner"。
- **仓库侧安全缓解**（§7.5 列表）：做哪些、何时做。
- **手机推送渠道**：飞书 bot 单聊，还是 PWA Web Push（需 HTTPS，iOS 需添加到主屏）。
- 本机卫生项：shell rc 里的 `AWS_BEARER_TOKEN_BEDROCK`、`~/.aws/credentials` 的静态密钥、SSH 私钥 passphrase。
- 多用户：v1 声明只支持单 owner；团队群聊场景的授权来源细则留到需要时。
- 新消息归属提示的误判率阈值（P3 用真实数据测）。
- Kiro / Codex 作为执行 session 时的 hook / auto mode 等价物与 command queue 降级。
- Kiro CLI V3 ACP 迁移（独立待办，先钉版本）。
- 跨节点批准是否需 owner 私钥签名。
- Go 定时器跨 macOS 睡眠、熔断重连延迟的真机测试。

## 18. 决策记录

被后续决策取代的条目以删除线标注，并注明取代者。

| # | 日期 | 决策 | 理由 |
|---|---|---|---|
| D1 | 10-15 | ~~三层：前台 / Planner / worker~~（→ D28、D38） | — |
| D2 | 10-15 | Planner 用 owner 排序的偏好表中第一个可用模型（当前 opus 5.5） | 强弱排序是 owner 的判断；强 planner 有证据支持 |
| D3 | 10-15 | ~~前台用 sonnet 5.5，每入口一个~~（→ D28） | — |
| D4 | 10-15 | Planner 每个 workspace 唯一、串行；触发都是投给它的消息；并行在执行 session 层 | 单轮短，串行不成瓶颈 |
| D5 | 10-15 | 主动 / 被动只是有无 duties | 统一模型，复用 cron |
| D6 | 10-15 | 你发起的动作不逐次人工批准（限定表述见 §7.1） | 以硬边界、审计、急停替代 |
| D7 | 10-15 | 读写分离推迟到邮箱 Planner；污染标记先行（§7.4） | 当前输入多来自可信来源 |
| D8 | 10-15 | 第一个主动 Planner 落在 naozhi 仓库 | 成败标准清晰 |
| D9 | 10-16 | Planner 无 Edit / Write；Bash 只放行白名单只读命令 | 不能看代码的 planner 过度规定细节 |
| D10 | 10-16 | 主动 duty 自主度默认 propose | Pulse 下线等证据 |
| D11 | 10-16 | auto mode 替代 `--dangerously-skip-permissions`，可切回 bypass；沙箱初期用 CC 原生 sandbox（整进程 srt 推迟） | 边界从提示词变硬约束；srt 冲突经评审核实 |
| D12 | 10-16 | Conductor 改名 Planner，与 project planner 合并 | 避免语义混淆 |
| D13 | 10-16 | 先单机 Mac；终态 EC2 控制面 + Mac `dispatch_only` 执行节点 | 常开控制面 + 凭证不离开本机 |
| D14 | 10-16 | Now / Next / Not-now 由你维护，并作为 Planner 自发工作的网关边界 | 判断质量与可控性都来自这里 |
| D15 | 10-16 | 不做使用量打点 | owner 已确认斜杠命令很少用 |
| D16 | 10-16 | 主路径自然语言；专业操作降到专家视图 | 降低门槛 |
| D17 | 10-16 | 成本不作为设计约束 | owner 决定 |
| D18 | 10-16 | 执行 session 生命周期分层回收，寿命跟产物走，按复活条件决定复用或另开 | 进程与冷上下文都昂贵 |
| D19 | 10-16 | 多 Planner 只经 `request` 协作：`accepts:`、provenance 只降不升、2 跳上限 | 防责任分散与转发死循环 |
| D20 | 10-16 | 带教规则进"懂你"、与 charter 分离、每轮 ≤50 条 | 防膨胀与指令遵循退化 |
| D21 | 10-16 | ~~前台用 opus 5.5 low / medium~~（→ D28） | — |
| D22 | 10-16 | 你对话的执行 session 可直接改状态；~~状态变更登记任务板~~ → 由事件流的 Activity 落实（D38）；粘性路由 | owner 主节奏"讨论 → 改吧 → 当场改 → 推送" |
| D23 | 10-16 | 模型按角色动态配置（会话 > workspace > 角色默认，热更新）；同模型时以 effort 区分 | "调 effort 往往比换模型更有效" |
| D24 | 10-16 | 协同原则：规则先行、消息可见、授权不扩张；~~在场 / 离场接管~~ → 托管规则（D43） | — |
| D25 | 10-16 | 托管：三层检测 + 按停顿原因处理 + 需人决定清单；清单落地为动作级 hook（v5） | 编排会话约 85% 的催促可代劳；评审发现 L1 可被注入 |
| D26 | 10-16 | 防循环 3 / 8 / 5，按血缘链累计（v5） | owner 数据；防另开绕过 |
| D27 | 10-16 | 执行形态（session 内 / CC Workflow / 多 session）的判据与决定者 | — |
| D28 | 10-17 | 只有两类 session（执行 session、Planner），无独立前台 | 前台没有不可替代的职责 |
| D29 | 10-17 | 话题线程与 worker 合并为执行 session；~~按驾驶者区分~~（→ D36） | 同一 runtime |
| D30 | 10-17 | 人为打断沿用 CC 原生 command queue / interrupt；queue mode 改为按 session 设置（v5 事实订正） | CC 已实现；默认实为 collect |
| D31 | 10-17 | Planner 事件驱动 + 空闲拉活（K=3）+ 自排闹钟 + 预判信号；不做心跳 | 避免噪声 |
| D32 | 10-17 | Planner 进程按事件密度自适应常驻 / 冷启动 | 状态全外置 |
| D33 | 10-17 | "通知已有 / 排后 / 取代 / 另开"是 Planner 的核心决策，可改判 | owner 指出 |
| D34 | 10-17 | ~~权限跟着驾驶者走、能力档 T1–T6~~（→ D36、D42） | — |
| D35 | 10-17 | 撞车四层处理；抢占只停车不杀；你对话中的 session 不被自动抢占 | — |
| D36 | 10-18 | 去掉"驾驶者"：session 中立；每条消息带 `on_behalf_of`；授权在动作时由 hook 判定；混合轮按最低身份 | owner：对 worker 来说谁开车不重要；免去切换与重启 |
| D37 | 10-18 | 不引入 Task；工作单元 = 执行 session + 血缘；契约挂在 session 上；提议在收件箱；外部 tracker 作链接 | 后端没有 Task；GitHub issue 已是任务系统 |
| D38 | 10-18 | 核心模型：三个角色（你 / Planner / 执行 session）、两种流动（消息 / 事件流）、两份知识（懂你 / 懂系统）；其余名词都是视图或字段 | owner 反馈旧模型名词过多、易混淆 |
| D39 | 10-18 | Planner 三种职责：幕僚（省注意力）、顾问（预判 + 过程评审 + 复盘）、知识维护者；介入 0–5 级 + 判断式 + "显而易见"五条门槛 + 按场景的介入配置 | owner：Planner 应是主动的 consultant，最懂你也最懂系统 |
| D40 | 10-18 | 顾问意见去向：你在对话中 → 旁注给你；托管中或 Planner 派出 → 直接给 session、允许反驳；严重 → 先叫停再报告 | 不打断你的对话，也不放过跑偏 |
| D41 | 10-18 | "懂你"建立在现有 auto-memory 上，可见可改，外部内容不能写入；"懂系统"由事件流确定性派生，Planner 只加批注 | 可信与可审计 |
| D42 | 10-18 | 不做派生凭证；Broker 初期 = 凭证清单 + 挂载策略 | owner：当前场景不需要 |
| D43 | 10-18 | 托管是 session 上的独立开关；"你在不在旁边"只是托管里的规则 | 离开不改变谁说了算 |
| D44 | 10-18 | 系统性故障合并处理是 Planner 的核心职责 | 附录 B：F 类错误成批出现 |

## 附录 A. auto mode 实测（2026-10-16）

环境：CC `2.1.296.1183`，`claude-opus-5-5[1m]`，`--setting-sources "" --settings ~/.naozhi/naozhi-settings.json --permission-mode auto -p --output-format stream-json`，一次性 git 仓库 + 本地 bare remote，`autoMode` 未配置。

| 动作 | 结果 |
|---|---|
| 新建文件、commit、fast-forward push | 放行；无分类器事件；22s / $0.11 |
| `commit-tree` + `reset --hard` 压缩根提交（为 force push 做准备） | **被分类器拒绝**；模型未绕行，如实报告只完成一部分 |
| 删除已并入 main 的远程分支（默认 soft_deny 所列） | **放行**——用户在请求里明确要求 |
| 拒绝之后 | 进程未终止；`result.permission_denials[]` 列出被拒调用 |

- 分类器把**请求里的明确指令视为用户意图**；Planner 写进任务的指令同样会被当作用户意图。不可逆边界必须落在 deny / hook。
- 新帧：`system/permission_check_status`（#3686 已丢弃）、`system/permission_denied`、`result.permission_denials[]`。
- 写成"被拒后换语法重试 + 外发含密钥文件"的探测 prompt 直接触发 **Opus 5.5 模型级 safeguard**（`system/model_refusal_no_fallback`，退出码 1）；naozhi 需单独的错误分类。

后续评审实测（2026-10-17，临时目录）：`promptCacheTtl: "1h"` 经 `--settings` 在 Bedrock 生效；工具执行中写入 `priority: next` 消息在同一回合被吸收；`--settings` 里的 SessionStart / UserPromptSubmit hook 在 `--setting-sources ""` 下触发。

## 附录 B. owner 使用数据（2026-10-16，本机只读分析）

范围：8/31–10/16 由 naozhi 管理的 118 个 session、owner 亲手输入的 715 条消息（逐条人工分类，单人标注）；session-runs 覆盖 60 天。

**请求构成**：跟进 / 继续 / 批准 34%（99 条 ≤12 字，无主语）；问答 / 讨论 / 设计 26%；状态查询 9%；外部系统操作 9%；小改动 6%；调研 5%；本机运维 5%；PR / 发版 5%；大任务 2%（却带来 90% 以上的成本与大部分"继续"）。

**习惯**：一个话题一个 session、每天新开约 2 个、47% 的活跃小时同时 ≥2 个；改状态的回合里 49% 是依赖上下文的跟进，60% 在 5 分钟内完成；回复中位间隔 3.9 分钟（对话类）/ 24 分钟（编排类）；opus 5.5 占 57% 回合，sonnet 只来自自动 fallback；跨 session 发消息 31 次。

**停顿原因**（305 条"继续 / 进展 / 按你的建议"之前的上一轮）：

| 停顿原因 | 编排类（128） | 对话类（177） |
|---|---|---|
| A 问已授权的下一步 | 7% | 5% |
| B 给选项等选择 | 30% | 17% |
| C 真 blocker | 2% | 31% |
| D 后台 workflow 在跑，owner 来问 | 21% | 9% |
| E 做了一部分就停、没提问 | 17% | 0 |
| F 出错 / 断线 / 进程被杀 | 20% | 4% |
| G 声称完成后 owner 改需求 | 2% | 34% |

- E 类平均 106 分钟后才被发现；F 类中地区 400 有 9 次、断网 / DNS 8 次、中途截断 8 次；D 类 77% 是纯状态查询，workflow 多数最终跑完。
- 编排会话 B 类 38 条里 27 条回复是"继续 / 按你的建议"，11 条是"要不要发版"。
- 纯放行后下一轮 79% 产生 commit 或 PR；"继续 → 无产出"连续 3 次只出现 1 次；最长连续催促 8 次。
- 局限：单人标注；编排会话样本只有 4 个。

## 附录 C. 参考（调研 2026-10-16 至 10-18）

- Claude Code Projects、Routines、auto mode、权限模式、prompt caching、agent teams、remote control（code.claude.com/docs）
- Anthropic engineering：multi-agent research system（2025-06-13）、effective harnesses（2025-11-26）、harness design（2026-03-24）、managed agents（2026-04-08）、auto mode（2026-03-25）、sandbox-runtime
- Cognition multi-agents working（2026-04-22）；Cursor scaling agents（2026-01-14）、self-driving codebases（2026-02-05）；Factory Missions（2026-04-10）
- PEAR（EACL 2026 Findings）；Can AI Models Direct Each Other?（arXiv 2603.26458）；Review Beats Planning（arXiv 2603.03406）；AGENTS.md 研究（arXiv 2602.11988、2601.20404、2607.27250）；AI 配置文件过期（arXiv 2606.09090）
- MAST（arXiv 2503.13657）；Prompt Infection（arXiv 2410.07283）；Agents of Chaos（arXiv 2602.20021）；Intelligent AI Delegation（arXiv 2602.11865）；A2A 规范
- IFScale（arXiv 2507.11538）；ACE（ICLR 2026）；IHEval（NAACL 2025）；CIPHER（NeurIPS 2024）；SkillsBench（2026-06）；Copilot Memory、Devin Knowledge、Jules、Cursor rules
- Google SRE AI 自主度分级；Anthropic Measuring agent autonomy（2026-02-18）；Meta Rule of Two；ImpossibleBench；METR time horizons
- Nomad placement / disconnect；MCP 2026-07-28 changelog；OpenClaw standing orders / heartbeat / memory；Kiro CLI V3
- 模型选择（platform.claude.com choosing-a-model）；多轮零碎需求掉分 39%（arXiv 2505.06120）；LangChain supervisor 原样转发（2025-06-11）

## 附录 D. 外部参照：Meta Muse（调研 2026-10-18）

Muse 是 Meta Superintelligence Labs 的个人 agent（代号 Hatch，2026-09-08 美国上线，App / muse.ai / WhatsApp），底层是 Muse 模型家族（Muse Spark 2026-04-08；Muse Wonder 2026-10-12，主打 Active Memory / Active Focus）。产品形态与本 RFC 高度相似：一条长的主对话 + side chat、Goals / Ideas / Feed / Library、后台按计划或事件继续工作、"主动消息的门槛必须很高"。Meta 承认产品上"深受 OpenClaw 启发"。

**已核实的架构**（官方安全博文 https://research.meta.ai/blog/security-and-safety-for-ai-agents-our-approach-with-muse ，2026-09-08）：每用户一台云端 VM；agent 跑在容器里、**看不到真实凭证**（`authd` 发替身 token）；**Sentinel** 是独立于 Muse 的另一个 agent，是 connector 动作与出网流量的唯一授权方，Muse 只能提议，审批经确定性 UI 直达用户而不经对话，授权分一次 / 会话 / 任务 / 限时 / 永久；eBPF 污点追踪，读过用户数据的进程失去自动放行；浏览器子 agent 只看无障碍树。

**泄露材料（多源印证、Meta 未确认）**：OpenClaw 式工作区文件（USER / MEMORY / PROACTIVE_PREFERENCES / SUBAGENTS_MONITORING 等）；记忆以 Markdown 为真相源、Postgres 作索引，`memory.claims` 记证据、置信度与 `supersedes_claim_id`；每小时 / 每天 / 夜间的自我改进任务（Ideas 打分、"做梦"复盘、技能复审会撤回可度量无效的改动）；主动推送每条只带一件事、注明来源，普通事项每天最多一次且等安静时段；"答应的提醒必须是单独的定时任务"；"送到用户手上才算完成"；"重复不可逆操作前先确认上次是否已生效"。

**上线后的问题**（已核实报道）：监控任务约 15 分钟后静默停止且吞错误；一句话就把 6.8GB 根文件系统打包发出；Muse 对自身机制给出错误解释引发公关事件；Ideas 被批评借建议之名索取更多数据。

**可借鉴（候选，未决定）与对应章节**：

| Muse 做法 | 对 naozhi 的含义 | 章节 |
|---|---|---|
| 推送管线独立、三级（日报 / 有时效 / 紧急），每条一件事并注明来源 | 打扰预算按三级实现，每条附"为什么推、依据哪条事件或先例" | §4.2、§10 |
| PROACTIVE_PREFERENCES：想听 / 永远别提 / 什么时候 / 什么形式；"一次忽略 ≠ 永久退订" | 作为"懂你"里注意力偏好的格式 | §3.5 |
| 承诺与酌情推送分开 | Planner 答应你的事落成定时任务，不受打扰预算挤压 | §4.6 |
| 记忆 = 结论 + 证据 + 取代关系，可追溯 | "懂你"条目带原始决策引用，纠正用取代而非覆盖 | §3.5、§8 |
| 反思离线批量做、经合成与闸门、可度量无效即撤回 | 夜间整理产出经闸门再进"懂你" | §8 |
| 授权方独立于 agent（Sentinel），审批走确定性 UI | 与 §7.2 一致：Planner 只提议，放行由 hook / 卡片决定 | §7.2 |
| "送达才算完成"、重复不可逆操作前先查是否已生效 | 加入过程评审与 verifier 检查项 | §4.3、§5.4 |
| 监控监控本身 | watcher / cron / 托管自身的存活与产出也进事件流 | §3.4、§3.6 |
| 解释自身行为只引用记录 | Planner 回答"为什么告诉我这个"必须引用事件或先例 | §4.4 |
| 避开：借"建议"索取更多权限 | 预判以已有上下文为限；扩权作为决策直接问你 | §4.3 |
