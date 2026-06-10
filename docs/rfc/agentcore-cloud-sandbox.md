# AgentCore 云上沙箱执行基底（控制面/数据面分离 + run-once 作业模型）

> **状态**: 设计提案 **v2（Phase 0 实测通过，2026-06-10）** — V1-V4+V8 全过，详见 `agentcore-cloud-sandbox-validation.md`
> **作者**: naozhi team
> **创建**: 2026-06-09
> **修订**: 2026-06-10 v1.1 — 新增 §4.3 结果交付模型（A1，已核实流式边界）；§12 重构为三层决策清单（吸收原 Open Questions）；新增 V8 验证项；Phase 1 范围收敛至 ≤60min 无本地 MCP 任务
> **修订**: 2026-06-10 v1.2 — 双 agent review 修订：runtimeSessionId 改为 = run_id（消除 sticky 残留态污染，§4.1）；断流≠job死 双跑漏洞封堵（§6.2 + maxLifetime 钳制）；run record 排除 secrets（§5.1）；新增 B10 workspace 决策与围栏（§4.4）；§6 升格小节；A3 拆包核实完成（依赖轻、可行）
> **修订**: 2026-06-10 v1.3 — **重定性**：AgentCore 从"第三类 backend"改为**执行基底（placement / infra 层）**，与 backend flavor 正交——microVM 里跑的还是 claude CLI、回来的还是 claude stream-json，不新增 flavor、不写 protocol_agentcore.go，改为 Runner/Transport 抽象（§4.2 重写）；新增 **§7 界面设计**（placement 选择器 / run 徽标三态 / run 详情 / replay / 人工确认队列 / 成本指示）；原 §7-§12 顺延为 §8-§13
> **修订**: 2026-06-10 v2 — Phase 0 实测通过（V1 完整 turn 含 Bash 工具 / V2 spawn→init 0.6-0.7s / V3 内存 ~250MB 无 MCP / V4 三态判定 + Stop 即焚实证 / V8 30.2min hold 流全程不断含 6 个 5min 静默段）；吸收 6 个实现层发现（F1 agentcoreRunner 必须 aws-sdk-go-v2 hold 流、F2 job prompt 须禁后台任务、F3 runtimeSessionId ≥33 字符、F4 容器非 root、F5 Phase 1 建专用执行角色、**F6 平台按 SSE 流静默判 idle——bootstrap keepalive 是硬前提，§4.1 已修订**），详见 validation 报告 §7
> **依赖 / 前置**:
> - `internal/cli/backend/profile.go`（多 backend 注册表已就位，参见 `multi-backend.md`）
> - `internal/cli/protocol.go` `Protocol` 接口
> - `docs/rfc/multi-backend.md` v2（backend.Profile 抽象 + Dashboard §8 差异化规约）
> - AWS Bedrock AgentCore Runtime（2026-06 GA；microVM-per-session，consumption-based 计费）
> **关联代码**: `internal/cli/backend/` · `internal/cli/protocol.go` · `internal/cli/wrapper.go` · `internal/cron/` · `internal/server/`（eventlog 回流）
> **可行性验证**: ✅ `docs/rfc/agentcore-cloud-sandbox-validation.md`（Phase 0 实测报告，2026-06-10）

---

## 0. TL;DR

把 AWS Bedrock AgentCore Runtime 接成 naozhi 的**执行基底（execution substrate）**——**不是第三类 backend**（v1.3 重定性）。microVM 里跑的还是 claude CLI，回来的还是 claude 的 stream-json 事件流；变的只是"在哪跑"。模型是两条正交的轴：

```
轴1（引擎/flavor）:   claude | kiro | (未来 gemini...)     ← backend.Profile 管这条
轴2（位置/placement）: local 进程 | agentcore microVM       ← 本 RFC 管这条
一个 job = flavor × placement
```

用于**一次性、隔离、弹性**的作业负载——首发载体是 **cron / sysession**。

核心设计是**控制面/数据面分离**：

- **naozhi = 有状态控制面**（单一事实来源）：持有租户的 CC 配置、skills、`CLAUDE.md`、MCP 配置、secrets、run record。
- **AgentCore Runtime = 无状态数据面**（纯算力）：base 镜像里只烤进 CC 二进制 + 通用环境；**自身不持久化任何租户数据**；session 结束 microVM 焚毁、内存擦除。

每次运行时，naozhi 把租户的配置/skills/prompt 打成 **payload 注入**到 microVM，CC 在沙箱里跑一遍，事件流回传 naozhi。

关键简化：**run-once 作业模型** —— 沙箱里跑过的 session **不 resume，跑一遍就完事**。这消掉了"transcript 无损重建"这个最棘手的问题，云端彻底无状态。Debug 需要时走 **replay（重放）而非 resume**：用 naozhi 留存的同一份输入 payload 重新注入一个全新 microVM 跑一遍——重放能力几乎免费，因为控制面本就握着全部输入。

**不替代本机装机模式**。naozhi 产品定位是"操作本机真实系统的装机工具"（不用沙箱）；本 RFC 是 placement 轴上**并存的第二个取值**，专门补本机模式给不了的能力：多租户隔离、弹性并发、阅后即焚、结构性消除 cron 会话泄漏。用户视角："还是那个 claude agent，只是这次跑在云上沙箱里"。

---

## 1. 背景与动机

### 1.1 naozhi 当前的进程模型

naozhi 把整个 agent loop（工具、上下文、推理）委托给长生命周期 claude CLI 子进程，自己只做路由。会话模型是 `session key ({channel}:{chatType}:{id}) → 进程`，本机 spawn，stream-json over stdin/stdout，崩溃/回收后 `--resume`。设计原则：**不引入外部组件、状态全文件化**。

### 1.2 痛点：本机模式给不了的三件事

1. **cron 会话泄漏**（已知问题）：cron job 跑完 exempt 会话永不回收，每个 ~1.6GB（claude+MCP 子树），实测 6 个占 9.68GB。根因是 Exempt + TTL 跳过 + finalize 不碰会话。本机模式下只能靠 Reset + 重注册 stub 这类手工回收打补丁。
2. **无多租户隔离**：所有会话共享一台机器的文件系统和进程空间。一旦多个陌生用户共用一个 naozhi 实例（SaaS 形态），本机模式毫无隔离可言。
3. **无弹性并发**：硬编码最大并发进程数（默认 3），满载驱逐最久空闲会话；无法 0→数千 session 弹性扩展。

### 1.3 为什么是 AgentCore Runtime

AgentCore Runtime 是少数为「有状态、长生命周期、I/O 密集」agent 进程设计的 serverless 运行时，它的 **microVM-per-session** 模型和 naozhi 的「session → 长驻进程」模型几乎同构：

| AgentCore Runtime 实测边界（2026-06 核实） | 对 naozhi 的含义 |
|---|---|
| 每 session 一个专属 microVM，CPU/内存/文件系统完全隔离，结束销毁 + 内存擦除 | 隔离强度从"进程级"升到"microVM 级" |
| 单 microVM 生命周期最长 **8h**，空闲超时默认 15min（可调 60s–8h，`idleRuntimeSessionTimeout` / `maxLifetime`） | run-once 作业下"跑完即焚"正合适，idle timeout 反而帮忙兜底 |
| **容器部署（ECR）= 任意框架/任意二进制** | claude CLI 这种非 Python 二进制**只能走容器路径**（direct-code-zip 是 Python 专用），硬约束 |
| `InvokeAgentRuntime`（agent 推理）+ `InvokeAgentRuntimeCommand`（同 microVM 确定性 shell） | 注入通道 + 环境准备通道 |
| payload 最大 **100MB**；同步请求 15min / 流式 60min / 异步 8h | 注入全量 config/skills 够用；本 RFC 选定流式（§4.3 A1-a），Phase 1 围栏 <60min，异步模式已排除 |
| 单 session 最高 **2vCPU / 8GB** | claude+MCP 单会话 ~1.6GB，8GB 够用但不宽裕，需监控峰值 |
| **消费量计费**：CPU 仅按实际占用秒计（I/O wait 免费），内存按峰值/秒计；$0.0895/vCPU-hr + $0.00945/GB-hr，1 秒起 | 对"大量会话、低 CPU、长等待"的 agent 负载是最优定价 |
| VPC/PrivateLink；session storage（S3-backed，跨 stop/resume 持久化文件系统，**preview**） | 本 RFC **不依赖** session storage（run-once 无需跨死亡持久化） |

---

## 2. 设计哲学：控制面 / 数据面分离

```
┌─────────────────────── 控制面（有状态，单一事实来源）─────────────────────┐
│  naozhi 网关（本机 / EC2 常驻）                                            │
│  - IM 接入 / session 路由 / dashboard / cron 调度                          │
│  - 持有：租户 CC 配置、skills、CLAUDE.md、MCP 配置、secrets                 │
│  - 持有：run record（输入 payload 快照 + 输出事件流）                       │
└───────────────────────────────────┬───────────────────────────────────────┘
                                     │ InvokeAgentRuntime(sessionId, payload)
                                     │ payload = config + skills + claude_md
                                     │           + mcp_config + secrets + prompt
                                     ▼
┌─────────────────────── 数据面（无状态，纯算力）─────────────────────────────┐
│  AgentCore Runtime microVM（每 session 隔离）                              │
│  - base 镜像（ECR）：CC 二进制 + Node + 通用工具链 + bootstrap handler      │
│  - 运行时物化注入的配置/skills 到 ~/.claude/ 和项目目录                      │
│  - spawn 长驻 claude CLI，喂 prompt，stream-json 输出流式回传               │
│  - session 结束 → microVM 焚毁 → 注入的一切归零（阅后即焚）                  │
└─────────────────────────────────────────────────────────────────────────┘
```

**云端零持久化租户数据**：naozhi 当唯一事实来源，既拿到 microVM 的隔离/弹性/阅后即焚，又不违背"状态文件化"的底色。

### 2.1 "烤进镜像" vs "运行时注入" —— 必须切开

| | 内容 | 时机 | 放哪 |
|---|---|---|---|
| **烤进 base 镜像**（一次） | CC 二进制、Node 运行时、通用工具链（git/python/常用 CLI）、bootstrap handler | `docker build` → ECR | 镜像层，所有租户共享 |
| **运行时注入**（每 microVM 生命周期一次） | 租户 `~/.claude/settings.json`、`agents/*.md`、`skills/*`、`CLAUDE.md`、MCP 配置、secrets、本轮 prompt | `InvokeAgentRuntime` 调用 | microVM 临时文件系统，结束焚毁 |

**红线**：CC 绝不"每次冷启动 npm install"——那会让冷启动慢到不可用。CC 装进镜像一次，运行时只注入"个性化的那层"。

---

## 3. run-once 作业模型

### 3.1 从 session 降级成 job

沙箱里跑过的 session **不 resume，跑一遍就完事**。这把模型从"会话"塌缩成"作业（job）"：

| | 有 resume 的 session（已否决） | run-once job（本 RFC） |
|---|---|---|
| 云端状态 | 要跨 microVM 死亡桥接上下文 | **零持久化**，microVM 死 = 作业结束 |
| 注入 | 冷启动注入 + 重注入 + transcript 回灌 | **只注入一次**，不存在重注入 |
| 生命周期 | 三态（冷/温/重注入） | **运行中 → 主动退出/Stop → 焚毁**（不靠 idle timeout 自然死，见 §4.1） |
| naozhi 侧 | session router 要管 resume 链 | 当成 **fire-and-forget 调用** |
| 延迟张力 | 重注入态回吐 naozhi 低延迟卖点 | **不成立**——job 只跑一次，延迟只剩"冷启动一次" |

### 3.2 语义边界：(A) 一个 turn vs (B) 一个 microVM 生命周期

- **(A) 一个 turn 就完事**：注入 → 喂一条 prompt → 拿完整输出 → 焚毁。纯单次。
- **(B) microVM 活着时可多 turn，但不跨死亡**：sticky session 在一个 microVM 内来回几轮，microVM 一死会话永久结束、不 resume。

**决策**：按 **(A) job 模型**实现。注意 (B) **不是免费附赠**（v1.2 修正）：(A) 的隔离性要求 `runtimeSessionId = run_id` 每 job 唯一（§4.1），而 (B) 依赖 sessionId 复用触发 sticky——二者互斥。若未来交互式场景真需要 (B)，须作为新决策单独设计 sessionId 复用策略，且要正视残留态污染问题。**不为 (B) 预留任何机制。**

### 3.3 适配的 naozhi 负载

run-once 特别适合本来就一次性的负载：

1. **cron 任务** ✅ 天生 fire-and-forget；microVM 焚毁 → **结构性消除会话泄漏**（云端根本不存在那个 bug）；可并发数千互不干扰。**首发载体。**
2. **system session / 后台任务** ✅ 无人值守、跑完即弃。
3. **`/sandbox <一次性任务>`** ✅ 隔离跑不可信代码/脚本。
4. **scratch drawer 追问抽屉** ✅ "临时、不污染主对话"，跑完即弃语义吻合。

**不适合**：日常交互式 IM 多轮长对话（连续追问几小时）——这类走 `placement: local`。再次印证：agentcore 是 placement 轴的补充取值，不替代主干。

---

## 4. 运行时注入机制

AgentCore Runtime 容器入口是自定义 HTTP server（AgentCore SDK 的 `/invocations` + `/ping`）。naozhi 调 `InvokeAgentRuntime` 时带的 **payload（≤100MB）就是注入通道**。

### 4.1 调用流程

```
naozhi (控制面)
  │  InvokeAgentRuntime(
  │      runtimeSessionId = <run_id>,   ← 每 job 唯一，绝不复用（见下方说明）
  │      payload = { config, skills, claude_md, mcp_config, secrets, prompt })
  ▼
microVM bootstrap handler
  1. 物化 payload：config/skills → ~/.claude/ 和项目目录
  2. （可选）InvokeAgentRuntimeCommand 做注入后/spawn 前环境准备（写文件、临时依赖）
  3. spawn 长驻 claude CLI 子进程  ← 与 naozhi 本机做的逻辑一致
  4. 喂 prompt 进 stdin（stream-json），stdout 流式回传 naozhi
  ▼
session 结束 → microVM 焚毁 → 注入的一切归零
```

bootstrap handler 本质是**把 naozhi 本机的"spawn + 配置 ~/.claude + stream-json 转发"逻辑搬一份到 microVM 里跑**。naozhi 侧的 `agentcoreRunner`（§4.2）负责打包 payload + hold 流；事件解析复用现有 `protocol_claude.go`。

**`runtimeSessionId = run_id`（每 job 唯一）是 job 模型的硬性要求**，不是实现细节：

- AgentCore 按 runtimeSessionId 做 microVM stickiness。若复用 naozhi session key，同一 cron task 两次相邻 run（间隔 < idle timeout）会**粘到同一个未焚毁的 microVM**——第二次 run 不触发注入、继承上次残留文件系统，"阅后即焚/全新隔离"全部失效，§5 replay "进全新 microVM"也被打破。
- 代价：§3.2 的 (B) sticky 多轮**不再是"免费附赠"**——(A) 要求 sessionId 唯一化、(B) 要求复用，二者互斥。若未来真要 (B)，需引入"交互会话专用的 sessionId 复用策略"，是新决策不是顺手开关。
- 配套：job 完成后 bootstrap 主动退出（或 naozhi 调 `StopRuntimeSession`），不等 idle timeout 自然焚毁——既消灭残留态窗口，也省 idle 期间的内存计费。
- **idle timeout ≠ 安全兜底**（Phase 0 实测修订，原"60s 兜底"作废）：平台把 **SSE 输出流静默**也判定为 idle——60s 配置实测误杀了一个正在 `sleep 300` 工具调用中的活 job（validation F6）。两层应对：① bootstrap 在 job 全程发 15s 间隔 keepalive 事件保持流非静默；② `idleRuntimeSessionTimeout` 配 300s（容忍 keepalive 偶发丢失），残留态窗口靠主动退出 + Stop 消灭，不靠短 idle。

### 4.2 naozhi 侧：placement 抽象（Runner/Transport），不新增 flavor（v1.3 重写）

**v1.3 重定性的核心代码含义**：AgentCore 不是新引擎，是新位置。microVM 里跑的是 claude CLI，回来的是 claude stream-json——`protocol_claude.go` 的事件解析**原样复用**，一行不改。因此：

- ❌ ~~`protocol_agentcore.go`~~（不存在"agentcore 方言"，不写）
- ❌ ~~`profile_agentcore.go` / `flavor: agentcore`~~（不注册新 flavor；否则将来 kiro 上云就要 `agentcore-kiro`，flavor 变 flavor×placement 叉积，协议解析全是重复）
- ✅ 在 **transport seam** 上新增 placement 实现。A3 拆包核实已发现：spawn/协议解析核心对 config/session/eventlog 零硬依赖，最重纠缠点恰是 `shim.Manager`（transport 边界）——**那个 seam 就是 placement 轴在代码里的天然落点**：

```
Runner 接口（新抽象，local 为现状缺省）
├─ localRunner     = 现有 exec 子进程 + shim transport（行为不变，纯重构提取）
└─ agentcoreRunner = InvokeAgentRuntime + payload 注入 + hold event-stream
                     （StopRuntimeSession 作终止原语）

两者之上：同一个 Protocol（protocol_claude.go）做事件解析
```

| 关注点 | localRunner | agentcoreRunner |
|---|---|---|
| spawn | exec 子进程 | `InvokeAgentRuntime`（payload 即注入） |
| 传输 | stdin/stdout（shim） | HTTP event-stream（A1-a hold 流） |
| 终止 | kill 进程 | `StopRuntimeSession` |
| workspace | 本机文件系统 | §4.4 B10 注入 / clone-on-boot |
| **协议/事件解析** | **protocol_claude.go（同一份）** | **protocol_claude.go（同一份）** |

**配置语义**：cron task / 会话配置新增 `placement: local | sandbox`（默认 `local`），agent/flavor 字段照旧。与 `multi-backend.md` 的关系从"在它之上加第三个 profile"改为"与它正交的新维度"，两个 RFC 各管一条轴。

**交付物**：
- `Runner` 接口提取 + `localRunner`（纯重构，行为不变，可独立 PR 先行）；
- `agentcoreRunner`（AWS SDK 调用 + payload 打包 + 流解析进现有 Protocol）；
- 容器侧 bootstrap handler（独立产物，Go 编译进镜像，复用拆包后的 spawn/协议包——它在云端那侧做的事不受本次重定性影响）。

### 4.3 结果交付模型（决策 A1：naozhi hold 流式连接）

`InvokeAgentRuntime` 本身就是**流式 HTTP 长连接**（text/event-stream，边产出边回传；同步上限 15min、流式上限 60min、异步模式 8h，2026-06 核实）。结果怎么回到 naozhi 有三个候选：

| 选项 | 机制 | 适用 | 代价 |
|---|---|---|---|
| **A1-a 同步 hold 流**（✅ 选定主路径） | naozhi 发起 invoke 后全程 hold event-stream 连接，边收边落盘 events.ndjson | 任务 **<60min**（绝大多数 cron/sysession） | 每个 in-flight job 占一个长连接 goroutine；连接断 = 流中断（→判 failed 可重放，§6.1/6.2 兜住） |
| A1-b agent 反向 push（逃生通道，Phase 4 之后再议） | microVM 内 handler 主动把事件流 push 回 naozhi | **>60min** 长任务 | **要求 naozhi 有 microVM 可达的入口**——与"naozhi 本机部署、无公网 IP"的定位冲突，还要新开鉴权通道 |
| A1-c async + 轮询（❌ 排除） | agent 立即返回"已开始"，`/ping` 返 `HealthyBusy` 保活，naozhi 之后 re-invoke 同 session 取结果 | 长任务 | 要求 microVM 全程存活 + re-invoke 取结果，与 run-once 语义打架，且 idle 期间内存持续计费 |

**选定 A1-a 的决定性理由**：数据走"naozhi 主动发起的那条连接"回来，**microVM 不需要反向可达 naozhi**——naozhi 保持纯调用方，零新增入口面。这一个决策同时消解了两个下游问题：

1. **网络模型（原决策 A4）**：不需要 VPC 打通、不需要 naozhi 暴露公网端点；
2. **事件回流协议（原 OQ3）**：退化为"naozhi 收到流后自己写 eventlog"，不需要 reverse-node WS 或任何新通道。

**Phase 1 范围随之收敛**：只支持 **<60min** 的 job（cron 实测都在分钟级，足够）。>60min 任务先在 naozhi 侧调度时拒绝并提示，A1-b 等真实需求出现再立项。

### 4.4 workspace 获取与产物回传（决策 B10，v1.2 新增）

首发载体 cron 的真实负载大多是 **git/仓库密集型**（PR review、auto-merge、代码修复），而 payload 100MB 装不下代码仓——"项目目录里的代码从哪来"必须显式设计，不能含混在"物化到项目目录"里：

| 选项 | 机制 | 代价 |
|---|---|---|
| **B10-a clone-on-boot**（✅ Phase 1.5 目标档） | payload 带 repo URL + 只读 deploy key / 短时效 token，bootstrap 在 spawn CC 前 `git clone --depth=N` | 冷启动加 clone 时间（浅克隆可控）；需 git 凭证下发（走 §5.1 secrets 引用机制） |
| B10-b payload 内嵌 workspace | 小型工作目录直接打进 payload | 仅适合 <100MB 的非 git 任务（脚本、文档处理） |
| B10-c 共享存储挂载 | S3/EFS 预置 workspace | 引入云端持久化状态，**违背零持久化原则**，排除 |

**产物回传走 git 而非文件流**：cron 任务的产出本来就是 push 分支/提 PR（git 远端即"产物存储"），事件流里带结论文本。不设计额外的"文件回传通道"——需要回传任意文件的任务不在本 RFC 范围。

**Phase 1 围栏再收敛**：首批 cron 任务限定 **无 workspace 或 B10-b 小内嵌**（如"调研报告、API 巡检、纯 prompt 任务"）；B10-a（git 任务）在 Phase 1.5 单独验证 clone 延迟 + 凭证下发后放开。

---

## 5. Debug 重放（replay ≠ resume）

| | resume（续跑，已否决） | replay（重放，本 RFC） |
|---|---|---|
| 目的 | 在原上下文上**继续** | **复现**一次跑过的执行 |
| 需要的状态 | CC 会话 transcript（难无损重建） | **当初的输入 payload**（naozhi 本就持有） |
| 怎么做 | JSONL 灌回去再 `--resume` | **同一份 payload 重新 `InvokeAgentRuntime` 进全新 microVM** |

**重放要存的不是"会话状态"，而是"作业的输入"** —— 而输入恰好就是 naozhi 注入的那份 payload。所以重放几乎免费，且不需要 session storage、不需要 transcript 回灌、不破坏"云端零持久化"。

### 5.1 run record（存在 naozhi 侧，文件化）

```
run record
├─ run_id
├─ input/
│  ├─ payload.json        # config + mcp_config + prompt + skills 版本引用；secrets 仅存引用名，绝不落值
│  ├─ skills_snapshot/    # 或只存 skills 的 content-hash（见 §5.2）
│  └─ claude_md
├─ output/
│  └─ events.ndjson       # CC 回传的完整 stream-json 事件流
└─ meta: { runtime_arn, image_version, started, ended, exit_status, cost }
```

**secrets 红线（v1.2 明确）**：run record 的 `payload.json` **排除 secrets 明文，只存引用名**（如 `secrets: ["github_token"]`）。否则 §6.6 "secrets 只在 microVM 内存活、随焚毁清除"被自己推翻——明文永久落在 naozhi 磁盘上。replay 时由控制面按引用**重新解析当前值**注入：这也顺带解决了 secrets 轮换后旧 payload 失效的问题（replay 复现的是"任务输入"，不是"当时的凭证"）。

### 5.2 内容寻址优化

skills/config 用 **content-hash 存**：多次 run 引用同一份 skill 只存一份，payload 里只放 hash。好处：(1) run record 不膨胀；(2) 重放时按 hash 还原快照，保证"用的就是当时那个版本的 skill"，哪怕 skill 后来改了。（参考 ecc `content-hash-cache-pattern`。）

---

## 6. run-once 引入的新问题（必须正视）

去掉 resume 不是没代价，而是把复杂度从"上下文桥接"换成"作业交付保证"：

### 6.1 结果交付与 partial-result 三态判定

job 模型下没有"重连续跑"——naozhi 必须把**输出流式落盘**（events.ndjson 边收边写），不等结束一次性拿。即使 microVM 中途死，已产出部分仍在。复用 eventlog writer 的中途失败防护（参见已知问题中"eventlog 毒化 writer"一类关注点）。

完成判定不是二元的（v1.2 细化为三态），三态对"是否产生副作用、能否安全重放"含义不同：

| 终态 | 判定依据 | 副作用可能性 | replay 安全性 |
|---|---|---|---|
| **success** | 收到 CC stream-json `result` 事件且非 error | 已完成（预期内） | 无需 replay |
| **failed-clean** | 收到 `result` 但 `is_error`，或流干净结束（EOF）但无 `result`（handler 异常退出） | CC 自己报错/早退，副作用**大概率未发生或不完整** | 较安全，仍建议有副作用任务先核对 |
| **failed-transport** | 传输层断流（连接 reset/超时），未见流尾 | **未知**——microVM 内 job 可能还在跑甚至已完成 | **不安全，必须走 §6.2 终止确认后才能 replay** |

### 6.2 双跑漏洞：断流 ≠ job 死（v1.2 封堵）

A1-a 的天然漏洞：naozhi 侧连接断了，**microVM 内 CC 可能继续跑完并产生副作用**（比如 cron 提了 PR）。naozhi 判 failed-transport 直接 replay → 同一任务跑两遍。`run_id` 去重只防 naozhi 自身重复发起，**感知不到云端实际完成状态**。封堵三件套：

1. **断流先终止再重放**：failed-transport 后必须先调 `StopRuntimeSession` 确保 microVM 已死，确认成功才允许 replay 进入队列；
2. **maxLifetime 钳制**：Phase 1 强制 `maxLifetime ≤ 60min`（= A1-a 流式上限）——杜绝"流被平台掐断后 job 还能再跑数小时"的窗口；
3. **有副作用任务的幂等围栏前移**（原 B7 增强档提前到 Phase 1）：cron 任务声明 `side_effects: true` 的，failed-transport 后**不自动 replay**，进 dashboard 人工确认队列（先查 PR 是否已提，再决定）。

### 6.3 无 checkpoint：长任务中断整跑

run-once 下中断要从头来。对绝大多数 naozhi 任务（分钟级）无所谓，且 §6.2 已把上限钳到 60min，最坏损失有界。文档写明边界即可。

### 6.4 可观测性回流

云端 job 的事件流要**回流到 naozhi eventlog**，dashboard 才能统一展示。**A1-a 选定后此问题大幅简化**（§4.3）：事件流本来就从 naozhi hold 的那条连接回来，"回流"退化为本地写入——agentcore Protocol 收到流后同时写 events.ndjson（run record）和 naozhi eventlog，无需任何新协议。

### 6.5 naozhi 自身重启时的 in-flight 孤儿流（v1.2 新增）

naozhi 的部署习惯是 `systemctl restart`（零中断热重启尚未实现）。重启瞬间所有 hold 中的连接孤儿化：run record 半截、云端 job 还在跑。设计要求：

- **in-flight run 状态落盘**：发起 invoke 前先把 `{run_id, runtime_session_id, started}` 写入 pending 目录（文件化，符合主干原则）；
- **启动 reconcile**：naozhi 启动时扫 pending，对每条孤儿 run 调 `StopRuntimeSession` 终止 + 标记 `orphaned`，按 §6.2 规则决定是否 replay（有副作用的进人工队列）。

### 6.6 MCP 二进制不在镜像里

租户 MCP 配置若引用本地二进制，microVM 里没有就跑不起来。两条路：(a) 纯远程 MCP（HTTP）直接注入配置；(b) 用 **AgentCore Gateway** 把多个 MCP 聚合成一个端点，注入的 MCP 配置只指向 Gateway（见 §8）。Phase 1 选 (a)（B6 回避档）。

### 6.7 CC 在 microVM 里连 Bedrock 与凭证边界

走 Runtime IAM 执行角色拿 AWS 凭证（平台提供）；注入 config 带好 Bedrock 设置（既有 Bedrock 约束依然适用：如 Computer Use 需 InvokeModel 非 Converse）。secrets 只在 microVM 内存活、随焚毁清除（注意 §5.1 红线：run record 侧不落明文）。

**多租户凭证边界（v1.2 补充）**：microVM 隔离是**计算层**隔离，不是**凭证层**隔离——所有租户的 microVM 共享同一 Runtime 执行角色。单租户阶段无问题；多租户 SaaS 阶段需 per-tenant 执行角色或 scoped 凭证下发，与 B5 的 Identity 增强档同期设计（见 §12 A4 状态修订）。另：payload 含敏感内容时需确认 CloudTrail data events 的捕获范围，必要时对 InvokeAgentRuntime 关闭 data event 记录。

---

## 7. 界面设计（v1.3 新增）

> 原则：**placement 是会话/任务的一个属性，不是新的页面**。不为云端 run 建独立视图——复用现有 cron 面板、会话卡片、事件流渲染，只加"在哪跑"的标识与少量动作。事件流回流 eventlog 后（§6.4），云端 run 的消息渲染与本机会话**零差异**，界面增量集中在四处。

### 7.1 发送入口：placement 选择器

**cron 任务编辑表单**（首发，Phase 1）：
- 新增「运行位置」字段：`本机`（默认）/ `云沙箱 ☁️`，紧挨现有 agent/backend 选择控件，沿用既有表单控件样式；
- 选「云沙箱」时联动显示围栏提示与校验（Phase 1 围栏，§12）：超时上限 60min、不支持本地 MCP、无 workspace/小内嵌——**校验在表单提交时同步反馈**（如任务引用了本地 MCP 二进制，直接标红"该 MCP 在云沙箱不可用"），不让用户保存一个注定失败的任务；
- 同字段位置预留 `side_effects` 开关（默认关）：声明任务有外部副作用（提 PR 等），决定 failed-transport 后进人工队列（§6.2）。

**quick session / scratch drawer**（Phase 3 后，可选）：输入框旁加一个 ☁️ toggle，开 = 本次任务进云沙箱 run-once；默认关。**IM 渠道（飞书）不加语法**——`/sandbox <prompt>` 指令留到真实需求出现再议，避免给 IM dispatch 加解析面。

### 7.2 运行标识：placement 徽标 + 三态终态

dashboard 已有 IM-origin badge 和 backend pill 两层标识（`dashboard.js` originBadgeInfo / backend pill 模式），placement 作为**第三个独立标识**加入，不与 backend pill 合并（轴正交，标识也正交）：

- **运行中**：会话卡片 / cron run 行加 `☁️ 沙箱` 徽标 + 现有"运行中"动画；
- **终态三色**（对齐 §6.1 三态判定，这是界面上必须暴露的核心状态机）：

| 终态 | 徽标 | 用户语义 |
|---|---|---|
| success | ☁️ 绿 | 云端跑完，结果可看 |
| failed-clean | ☁️ 黄 | 任务自己报错/早退，可直接重放 |
| failed-transport | ☁️ 红 + ⚠ | 断流，云端状态未知——**禁止一键重放**，引导进 §7.4 确认流程 |

- cron 面板 run-history 行（复用现有 cron-run-history UI，即原 C14）：每行加 placement 徽标；header cron-badge 的 attention 计数把 `failed-transport` 与 `orphaned` 计入待处理。

### 7.3 run 详情与 replay

点开云端 run（复用现有会话详情/事件流渲染）：

- **事件流**：与本机会话完全相同的消息渲染（eventlog 回流的直接收益，无新组件）；
- **run 元信息条**（新增，详情页顶部一行）：`run_id · 镜像版本 · 时长 · 内存峰值 · 成本估算`（数据源 = run record meta，§5.1）；
- **「重放」按钮**：success / failed-clean 状态下可用；点击 = 同一份输入快照（content-hash 还原，§5.2）注入全新 microVM，新 run 以 `replay_of: <原run_id>` 链到原 run，详情页显示来源链；failed-transport 状态下按钮禁用，tooltip 引导到确认队列；
- **输入快照查看**（debug 场景）：折叠面板展示注入的 payload 清单（config/skills hash/CLAUDE.md/prompt；secrets 只显示引用名——与 §5.1 红线一致，界面也绝不渲染 secrets 值）。

### 7.4 人工确认队列（§6.2 副作用围栏的 UI 落点）

`side_effects: true` 的任务断流后进确认队列，这是双跑封堵三件套里唯一需要人参与的环节：

- **入口**：cron 面板内一个待办分区（非独立页面），header cron-badge attention 计数联动；
- **每条卡片**：任务名 + 断流时间点 + 已收到的部分事件流（看它断在哪一步）+ 副作用线索辅助（如任务声明了 PR 类副作用，卡片给出"去检查 PR 列表"的外链）；
- **两个动作**：`确认已完成`（标记 closed，不重放）/ `确认未完成，重放`（先自动 `StopRuntimeSession` 确认终止——§6.2 第 1 条在 UI 动作里内嵌执行——成功后才入 replay 队列）；
- **orphaned run**（naozhi 重启 reconcile 产物，§6.5）同入此队列，同样两个动作。

### 7.5 成本可见性（轻量，非账单系统）

- run 详情元信息条里的单次成本估算（§7.3）；
- cron 面板 placement=sandbox 的任务行，月累计成本估算小字（数据源 = run record meta 求和，纯前端聚合）；
- 不做预算告警 UI（B9 增强档的事，到时再议）。

### 7.6 各 Phase 界面交付切分

| Phase | 界面交付 |
|---|---|
| Phase 1 | §7.1 cron 表单 placement 字段 + 围栏校验；§7.2 运行中/终态徽标（最小可用：能选、能看出在哪跑、能看出结果） |
| Phase 2 | §7.3 run 详情元信息条 + 输入快照查看；§7.5 成本小字（依赖 run record 落地） |
| Phase 3 | §7.3 replay 按钮 + replay_of 链；§7.4 人工确认队列（依赖 replay/幂等机制落地） |
| Phase 3 后 | §7.1 quick session / scratch drawer 的 ☁️ toggle（可选） |

**测试规约**：沿用 dashboard 既有契约测试模式（per-file 不用 union，吸取 cron_view.js 拆分教训）；三态徽标与确认队列动作必须有 DOM 级断言覆盖——它们是 §6.2 安全机制的 UI 面，不是装饰。

---

## 8. 与既定决策/红线的关系

| 既定红线 | 本 RFC 的处理 |
|---|---|
| 产品定位 = 操作本机真实系统的装机工具（不用沙箱） | **不替代**，placement 轴的并存取值；`placement: local` 保持纯净文件化 |
| 状态全文件化、不引入外部组件 | 控制面（naozhi）保持文件化；外部依赖（AgentCore）只在 sandbox placement 这条线引入 |
| sysession Runner 保持 `--setting-sources ""`（防 AutoTitler 死循环） | 注入的 config 沿用现有 env policy；microVM 内 spawn 沿用同样的隔离开关 |
| 低延迟卖点（stream-json 长驻 4-5x） | run-once 下延迟只剩"冷启动一次"，cron 类负载对此不敏感（后台任务无人等首字节）；交互式负载仍走本机 |
| 已知 Bedrock 约束（Channels 不支持 / CU 需 InvokeModel） | microVM 内 CC 走 Bedrock 时全部适用；CU 类需求在沙箱里反而更合适 |

---

## 9. 可选增强：Gateway / Identity（仅 sandbox placement 线）

> 这两个组件违背"零外部依赖、状态文件化"，**只在 sandbox placement 这条线**作为配套设施（infra 配套挂 infra 轴下，定位自洽），**不进本机主干**。

- **Gateway** ⭐：把多个 MCP server 聚合成一个 MCP 端点 + 语义工具检索（`x_amz_bedrock_agentcore_search`）+ 出口凭证注入。价值：(1) 解决 naozhi 每会话每 backend 重连所有 MCP 的 M×N 问题；(2) 语义检索让 CC 先搜"需要什么工具"再调，省 token（呼应上下文优化主题）；(3) 多租户下游凭证按入口身份注入隔离。**也顺手解决 §6.6 的 MCP 二进制问题**。
- **Identity** ⭐：入口鉴权接 Cognito/Entra/Okta；出口托管下游凭证（GitHub/Stripe…）。经 Runtime/Gateway 调用时**免费**。价值：替换手搓的 dashboard token/CSRF（踩过坑的薄弱环节）+ 多租户凭证隔离。
- **Memory** ❌：与 CC `--resume` + `CLAUDE.md`/`MEMORY.md` 强冗余，且按 event 计费。不用。
- **Browser Tool / Code Interpreter** ⚠️：前者与"本机 driver 操作真实浏览器"决策方向相反；后者等于本 RFC 的沙箱执行本身，非额外功能。

---

## 10. 成本量级（Runtime，naozhi 实测负载）

```
每秒成本 = (实际占用 vCPU × $0.0895 + 峰值内存 GB × $0.00945) / 3600
```

- **短 sysession job（活跃 5min，CPU 30%，内存 1.6GB）**：≈ $0.0035/次
- **cron 任务（30min，CPU 平均 0.5vCPU，内存 1.6GB）**：≈ $0.030/次
- **idle 残余**：CPU≈0 不计费，内存 1.6GB × $0.00945/hr ≈ $0.0151/hr；本 RFC 下 job 完成即主动退出/Stop（§4.1），idle 残余仅在异常路径出现，60s 兜底超时把浪费压到 ≈$0.0003（**对比本机 9.68GB 常驻无人管**）

盈亏平衡：高利用率/长连接 → 本机 EC2 更便宜（固定成本摊薄）；稀疏/突发/大量空闲（cron、SaaS、偶发群聊）→ AgentCore 按用量省得多。

---

## 11. Phase 0 可行性验证（待跑，升 v2 前置）

run-once 简化后，原"延迟 + resume 无损重建"两大问题，**resume 那个划掉**。验证项按归属分两组（v1.2 修正：V5/V7 对应机制在 Phase 2/3 才实现，不应阻塞升 v2）：

**Phase 0 可行性验证（升 v2 前置）**：

| # | 验证点 | 通过标准 |
|---|---|---|
| V1 | CC 能否在 microVM 里以 stream-json 长驻并完成一个完整 turn | 注入 config → spawn → 跑通一个代表性 cron 任务，输出完整 |
| V2 | 冷启动端到端延迟（注入 → spawn → 首字节 / 完成） | 后台 job 场景下可接受（cron 无人等首字节，容忍度高于交互式） |
| V3 | 单会话内存峰值 | 安全低于 8GB（claude+MCP 代表性配置实测） |
| V4 | 结果完整性 + partial-result | 输出流式落盘；microVM 中途焚毁能按 §6.1 三态可靠判定 |
| V8 | **A1-a hold 流的连接稳定性**：30min 级 cron 任务全程 hold event-stream 不断流 | 长连接在 NAT/代理/AWS SDK 默认超时下稳定收完整流；断流时 §6.1/§6.2 判定与终止确认生效 |

**后续 Phase 验收项（随对应 Phase 交付，不阻塞 v2）**：

| # | 验证点 | 归属 | 通过标准 |
|---|---|---|---|
| V5 | run record 内容寻址存储 | Phase 2 | 重放时按 hash 还原快照、payload 不膨胀 |
| V6 | 事件流回流 naozhi eventlog | Phase 2 | dashboard 能统一展示云端 cron run |
| V7 | 幂等/去重 + 断流终止确认 | Phase 3 | `run_id` 去重 + failed-transport 先 Stop 后 replay，有副作用任务进人工队列（§6.2） |

验证脚本与原始输出汇总到 `docs/rfc/agentcore-cloud-sandbox-validation.md`（参考 `multi-backend-validation.md` 模式）。

---

## 12. 实施路线（建议）

聚焦 **cron 这一个场景**作为 sandbox placement 的第一个落地点（既是天生 job、又顺手结构性治好会话泄漏）：

1. **Phase 0**：跑 §11 V1-V4 + V8，产出 validation 报告，升本 RFC 到 v2。
2. **Phase 1**：`Runner` 接口提取 + `localRunner`（纯重构 PR 先行）→ base 镜像（CC + 通用环境 + bootstrap handler）+ `agentcoreRunner`（§4.2，复用 protocol_claude.go）+ cron 表单 placement 字段与围栏校验、运行徽标（§7.6），跑通单个 cron job run-once。**范围围栏**：仅 <60min 任务且 `maxLifetime ≤ 60min`（§6.2 钳制）；仅无 workspace / B10-b 小内嵌任务（§4.4）；仅纯远程 MCP 或无 MCP（B6 回避档）；secrets 走 payload 注入但 run record 只存引用（B5 回避档 + §5.1 红线）；in-flight run 落盘 + 启动 reconcile（§6.5）。超界任务调度时拒绝并提示走 `placement: local`。
3. **Phase 1.5**：B10-a clone-on-boot（git 任务放开）：验证浅克隆延迟 + 只读凭证下发。
4. **Phase 2**：run record（内容寻址）+ 事件流回流 eventlog + dashboard 展示：run 详情元信息条、输入快照查看、成本小字（§7.6；V5/V6 验收）。
5. **Phase 3**：replay（重放）+ 幂等去重 + 断流终止确认全流程，含 replay 按钮 + 人工确认队列 UI（§7.4/§7.6；V7 验收）。
6. **Phase 4（可选）**：Gateway 接 MCP + Identity 多租户鉴权 + per-tenant 凭证隔离（仅 agentcore 线）。

---

## 13. 决策清单（三层分级）

> 按"定错了要推倒重来"的程度分级。A 层是架构阻塞性决策（必须 Phase 0 前拍板）；B 层影响复杂度/成本/合规但不推倒架构（Phase 1 选"回避档"渐进）；C 层是实现细节（Phase 0 跑完再定）。原 Open Questions（OQ1-5）已吸收进对应条目。

### A 层：架构阻塞性决策

| # | 决策 | 状态 | 结论 / 待办 |
|---|---|---|---|
| **A1** | 结果交付模型：hold 流 vs 反向 push vs async 轮询 | ✅ **已决** | **A1-a naozhi hold 流式连接**（§4.3）。决定性理由：microVM 无需反向可达 naozhi，契合"无公网 IP"定位；同时消解 A4 和原 OQ3。A1-b 留作 >60min 逃生通道，A1-c 排除 |
| **A2** | run-once 语义：纯单 turn (A) vs sticky 多轮 (B)（原 OQ1） | ✅ **已决** | **先纯 (A) job 模型**（§3.2）。cron/sysession 都是单次；(B) 是 sticky 的免费附赠，等交互式需求出现再开，不为其引入任何机制 |
| **A3** | bootstrap handler 语言：Go 复用主干 vs 轻量脚本（原 OQ2） | ✅ **已决（拆包核实通过，2026-06-10）** | **Go**。代码核实结论：spawn/协议解析核心（wrapper/process/protocol_*）对 config/session/eventlog **零硬依赖**——session router 走接口注入、eventlog 走 SetPersistSink hook，均已解耦；仅剩 shim.Manager（最重但边界清晰，可保留或抽 transport 接口）和 metrics 埋点（callback 注入即可消除）两个可管理纠缠点。拆包可行性超预期，不再阻塞 |
| **A4** | 网络出站模型：microVM ↔ naozhi / Bedrock | 🟡 **单租户阶段已决；多租户凭证隔离未决**（v1.2 降级） | microVM→naozhi 不存在（数据走 naozhi 发起的连接回来），无需 VPC/公网端点——此部分已决。CC→Bedrock 走 Runtime IAM 执行角色，但 **microVM 隔离≠凭证隔离**：所有租户共享执行角色权限（§6.7）。多租户 SaaS 阶段需 per-tenant 角色/scoped 凭证，与 B5 增强档同期设计 |

### B 层：设计决策（Phase 1 选回避档，后续增强）

| # | 决策 | Phase 1 回避档 | 后续增强档 |
|---|---|---|---|
| **B5** | secrets 注入（原 OQ5） | payload 注入（microVM 内存活 + 焚毁），但 **run record 只存引用名不落值**（§5.1 红线）；确认 CloudTrail data events 不捕获 payload 明文，必要时关闭该 data event（§6.7） | AgentCore Identity 托管（多租户 SaaS 阶段再上，更合规） |
| **B6** | MCP 策略 | 只支持纯远程 MCP（HTTP 直接注入配置）或无 MCP 的 cron 任务——**不让 MCP 阻塞首发** | Gateway 聚合（Phase 4，§9）：顺手解决本地二进制问题 + 语义工具检索 |
| **B7** | 幂等/去重粒度 | `run_id` 去重 + **副作用围栏前移**（v1.2，原增强档提前）：`side_effects: true` 任务 failed-transport 后不自动 replay，进人工确认队列（§6.2） | 任务级幂等声明 + 自动副作用探测（如查 PR 是否已存在） |
| **B8** | partial-result 判定 | **三态判定**（v1.2 细化，§6.1）：success / failed-clean / failed-transport，replay 安全性分级；failed-transport 必须先 `StopRuntimeSession` 终止确认 | — |
| **B9** | 成本/配额护栏 | 每类 job 配 `max_duration`（钳 `maxLifetime ≤ 60min`，§6.2）+ naozhi 侧并发上限 + **InvokeAgentRuntime 错误分类**（throttle→指数退避重试 / 4xx→fail 不重试 / runtime crash→按 §6.1 三态）；上线前核实账户级并发 session 与 TPS 配额（v1.2 新增，"0→数千弹性"受配额约束需实测） | 月度预算告警 + per-tenant 配额 |
| **B10** | workspace 获取与产物回传（v1.2 新增，§4.4） | 无 workspace / B10-b 小内嵌任务；产物回传走 git/事件流，不设文件回传通道 | B10-a clone-on-boot（Phase 1.5）：浅克隆 + 只读凭证；B10-c 共享存储已排除（违背零持久化） |

### C 层：实现细节（Phase 0 后定，不阻塞）

| # | 事项 | 备注 |
|---|---|---|
| **C10** | base 镜像通用工具链范围（原 OQ4） | 用代表性 cron 任务在 Phase 0 实测倒推，避免拍脑袋定预装清单 |
| **C11** | run record 内容寻址实现 | hash 算法、GC 策略、存储位置（§5.2） |
| **C12** | 事件回流协议（原 OQ3） | **已被 A1-a 消解**：退化为 Protocol 收流后本地双写（events.ndjson + eventlog），无新协议 |
| **C13** | 镜像/Runtime 版本发布与灰度流程 | 镜像版本记入 run record meta，重放时可还原当时镜像 |
| **C14** | dashboard 展示云端 cron run | **已升格为 §7 界面设计**（v1.3）：placement 徽标三态 / run 详情 / 确认队列；此条保留作交付追踪 |

### 决策依赖链

```
A1(✅ hold流) ──┬──► A4(🟡 单租户已决/多租户凭证未决)
               └──► C12(✅ 被消解)
A2(✅ 纯单turn) ──► A3(✅ Go,拆包核实通过) ──► C10(镜像工具链)
B5/B6/B10 独立，Phase 1 均选回避档
§6.2 双跑封堵 ──► B7/B8/B9 的 Phase 1 档位（已联动更新）
```

**Phase 0 前已无架构阻塞项**（v1.2：A3 拆包核实通过；A4 余下的多租户凭证隔离不阻塞单租户 Phase 0-3）。可直接进入 Phase 0 验证 V1-V4 + V8。
