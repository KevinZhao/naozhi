# AgentCore 云上沙箱 Phase 0 可行性验证报告

> **状态**: Phase 0 实测报告（不是设计文档）
> **作者**: naozhi team
> **日期**: 2026-06-10
> **环境**:
> - claude CLI 2.1.170（ARM64 glibc ELF，动态链接仅 libc 系）
> - AgentCore Runtime: `naozhi_sandbox-f7slOW6vlM`（us-west-2，PUBLIC 网络模式，`idleRuntimeSessionTimeout=60s`，`maxLifetime=3600s`）
> - base 镜像: `788668107894.dkr.ecr.us-west-2.amazonaws.com/naozhi-sandbox:phase0`（amazonlinux:2023 ARM64 + claude CLI + Go bootstrap handler）
> - 模型: `global.anthropic.claude-haiku-4-5-20251001-v1:0`（验证用，省成本）
> - 平台: Linux 6.1.170 aarch64（EC2 控制面侧）
> **关联 RFC**: `agentcore-cloud-sandbox.md` v1.3 §11（本报告是其升 v2 的前置材料）
> **Spike 代码**: `spike/agentcore/`（bootstrap handler + Dockerfile + build.sh，不进 naozhi 二进制）

---

## 0. TL;DR

V1-V4 + V8 全部跑通。结论：

- ✅ **V1 过** — CC 在 microVM 内以 stream-json 完成带 Bash 工具调用的完整 turn
- ✅ **V2 过且优于预期** — 热 runtime 下注入→spawn→init ~0.6s；首次调用（runtime 冷启动）wall 13.8s
- ✅ **V3 过且余量大** — microVM 8GB 内存中 CC RSS 仅 ~250MB（无 MCP 配置）
- ✅ **V4 过** — `StopRuntimeSession` 中途焚毁 = failed-transport 形态可靠（无 result/exit 事件、SSE 戛然而止）；failed-clean（CLI 自报错）= `result.is_error=true` + exit code 1，三态可区分
- ✅ **V8 过** — 30.2min 全程 hold 流不断（6 个 5min 静默段），但暴露关键发现 F6：平台按"SSE 流静默"判 idle，keepalive 是硬前提（§6）
- ⚠️ **6 个实现层发现**（§7）：F1 AWS CLI 不流式落盘（agentcoreRunner 必须 SDK hold 流）；F2 CC 会把长任务丢后台提前返回；F3 `runtimeSessionId` ≥33 字符；F4 容器须非 root；F5 专用执行角色；**F6 idle 按流静默判定，keepalive 为硬前提**

**判定：RFC 可升 v2，Phase 1（agentcoreRunner + cron placement 字段）无架构级阻塞。**

---

## 1. V1：CC 在 microVM 内完成完整 turn

**做法**：bootstrap handler（Go，`/invocations` + `/ping`）收 payload → 物化 `~/.claude/settings.json` + `CLAUDE.md` → spawn `claude -p --output-format stream-json --input-format stream-json --verbose --dangerously-skip-permissions --setting-sources user` → 喂单条 user 消息 → stdout 逐行转 SSE 回传。

**payload**（RFC §4.1 注入通道，Phase 0 字段集）：

```json
{
  "settings": {"env": {"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_REGION": "us-west-2", "ANTHROPIC_MODEL": "...haiku..."}},
  "claude_md": "回答尽量简短。",
  "prompt": "1) 运行 `uname -m && cat /etc/os-release | head -2 && free -h | head -2`；2) 总结运行环境。"
}
```

**结果**：35 个 SSE 事件，CC 调了 Bash 工具并拿到结果：

```
架构：aarch64 / Amazon Linux 2023 / 内存 7.8GB 总量
result: subtype=success, is_error=false, num_turns=2, cost=$0.019
```

- 事件流与本机 spawn 完全同形（system/init → assistant(tool_use) → user(tool_result) → result）——**协议解析复用 protocol_claude.go 的前提成立**（RFC §4.2 v1.3 重定性的实证）。
- CC→Bedrock 凭证走 Runtime IAM 执行角色（容器内无任何注入凭证），`CLAUDE_CODE_SKIP_BEDROCK_AUTH` 不需要。

## 2. V2：冷启动端到端延迟

| 阶段 | 实测 | 说明 |
|---|---|---|
| 首次 invoke（runtime 平台冷启动 + 镜像拉取）wall | **13.8s** | 仅发生在 runtime 部署后首个 microVM |
| 后续 invoke（每 job 全新 microVM）payload→materialize | **<1ms** | bootstrap 物化注入层极快 |
| spawn→system/init | **0.6-0.7s**（3 次采样 0.63/0.61/0.71） | CC 进程启动 |
| init→result（单轮无工具） | **1.8-2.4s** | 模型推理为主 |

**结论**：cron/后台负载（无人等首字节）完全可接受，符合 RFC §1.3 预期。注意每个 job 的 runtimeSessionId 唯一 → 每次都是新 microVM，上表"后续 invoke"即稳态成本。

## 3. V3：内存峰值

microVM 内实测（`ps aux --sort=-rss`）：

```
claude CLI RSS ≈ 251MB（无 MCP），bootstrap 5.5MB，总占用 355MB / 7.8GB
```

**结论**：8GB 上限余量充足。本机 ~1.6GB 的实测值主要来自 MCP 子树；Phase 1 围栏（纯远程 MCP 或无 MCP）下云端内存压力远低于本机。带 MCP 的实测留到 Phase 1.5/2。

## 4. V4：partial-result 三态判定

| 终态 | 实测做法 | 观察到的形态 | 判定依据成立？ |
|---|---|---|---|
| success | 正常跑完 | `result` 事件（`is_error=false`）+ bootstrap `exit` 事件（code 0）+ SSE 正常 EOF | ✅ |
| failed-clean | 注入不存在的 model id | `result` 事件 `is_error=true`（API Error 文本）+ `exit` code 1 + 正常 EOF | ✅ |
| failed-transport | invoke 后 20s 调 `StopRuntimeSession`（CC 正在 sleep 120） | SSE 流**戛然而止**：84 个事件后无 result、无 exit、连接关闭（AWS CLI 正常退出 rc=0，输出文件以最后一个普通事件结尾） | ✅ |

**关键观察**：

1. failed-transport 的判定信号 = "流结束但未见 `result` 事件"。注意 **AWS CLI 侧 rc=0 不可作为判定依据**——必须看事件内容（agentcoreRunner 要按"是否收到流尾 result"判，不是按 HTTP 调用是否成功判）。
2. `StopRuntimeSession` 立即生效（秒级），作为 §6.2 双跑封堵的终止原语可用。
3. **Stop 后同 runtimeSessionId 再 invoke 得到全新 microVM**：`uptime -p` 报 "up 0 minutes"、文件系统无前次残留（注：`~/.claude/projects/` 里的条目是本次 turn 自己产生的 transcript，不是残留）。"阅后即焚"语义实证成立。

## 5. failed-clean 补充：CLI 错误的事件形态

CC 对 API 层错误的报告方式是 **`result` 事件 `is_error=true` + 进程 exit code 1**，不是裸退出。这意味着 agentcoreRunner 的三态判定可以完全基于事件流内容（与本机 wrapper 的 result 处理同构），bootstrap 的 `exit` 事件（带 code）作为辅助信号。

## 6. V8：30min hold event-stream 连接稳定性

**做法**：prompt 要求 CC 前台同步跑 6 轮 `sleep 300 && date -u`（总 ~30min），每轮间隔 5 分钟模型完全静默——这是对 NAT/中间设备/SDK 读超时/平台 idle 判定最严苛的形态。控制面侧 `--cli-read-timeout 0`。

**第一次尝试（失败，发现 F6）**：`idleRuntimeSessionTimeout=60s` 配置下，job 在 ~85s 断流（无 result/exit，failed-transport 形态）。CC 进程明明活着（前台 sleep 中），但 **SSE 输出流静默 60s 就被平台判 idle 焚毁**——"进程活性"不算 activity，"流上有字节"才算。

**修正后通过**：bootstrap 加 15s 间隔 keepalive 事件 + idle timeout 调到 300s：

```
总时长 1813s（30.2min）· 218 个 SSE 事件（120 keepalive + 95 cli + boot/exit）
6/6 心跳全部到达 · result: is_error=false "30分钟测试完成" · exit code 0
流窗口 12:32:04 → 13:02:18 无一次断流（含 6 个 5min 模型静默段）
```

**结论**：✅ A1-a hold 流在 30min 级任务上稳定，但 keepalive 是**硬前提**不是优化——没有它任何含 >60s 静默工具调用的 job 都会被平台误杀（详见 F6）。

## 7. 实现层发现（Phase 1 必须吸收）

| # | 发现 | 对 Phase 1 的要求 |
|---|---|---|
| F1 | **AWS CLI 的 invoke-agent-runtime 不流式落盘**（收完整流后一次性写 outfile） | agentcoreRunner 必须用 **aws-sdk-go-v2 `bedrockagentcoreruntime` 客户端**直接 hold `InvokeAgentRuntime` 的 streaming body，逐事件解析+落盘（RFC §6.1 流式落盘要求 CLI 工具给不了） |
| F2 | **CC 会把长命令丢进后台任务并立即结束 turn**（首次 V8 尝试 15s 就返回，sleep 在 task 里被 kill） | run-once 语义下 job prompt 须显式禁止后台任务，或 bootstrap 注入的 settings 加围栏；cron 模板要写明"前台同步执行" |
| F3 | **runtimeSessionId 最小长度 33 字符**（API 校验） | run_id 生成规则要保证长度（如 `run-<unix>-<suffix>` 补齐），写进 agentcoreRunner 的 id 构造器 |
| F4 | **非 root 用户必须**：CC 拒绝以 root 跑 `--dangerously-skip-permissions` | 镜像里 `useradd agent` + `USER agent`（已写进 Dockerfile） |
| F5 | **执行角色最小权限集已验证**：`bedrock:InvokeModel*` + ECR pull + CloudWatch Logs 即可跑通 | Phase 1 建一个 naozhi 专用执行角色（不复用旧实验角色，旧角色挂着多余的 ElasticBeanstalk 托管策略） |
| F6 | **idle timeout 按"SSE 输出流静默"判定，不是按 microVM 内进程活性**：60s idle 配置下，CC 正在前台 `sleep 300`（进程活、流静默）的 job 在 ~60s 被平台焚毁（V8 第二次尝试 1m25s 断流，无 result/exit） | bootstrap 必须发 **15s 间隔 keepalive 事件**保持流非静默（已实装）；`idleRuntimeSessionTimeout` 不能当"快速兜底"配太短，Phase 1 取 300s；RFC §4.1 的"60s 兜底"已据此修订 |

## 8. 成本实测

| 负载 | 实测 |
|---|---|
| 单轮问答（haiku，~3s 活跃） | $0.004-0.006/次（模型 token 成本；AgentCore 计算费另计，秒级 job 可忽略） |
| 带 Bash 工具 2 turns | $0.019/次 |

与 RFC §10 量级估算一致。

## 9. 验证脚本与产物

- spike 代码: `spike/agentcore/`（bootstrap handler ~310 行 Go + Dockerfile + build.sh）
- 本机冒烟: `bootstrap` 直跑 + docker 容器跑通（凭证透传仅限本地测试）
- 云上 runtime: `naozhi_sandbox-f7slOW6vlM`（保留供 Phase 1 复用/重建）
- 原始输出: `/tmp/v1-out.txt` `/tmp/v2-out-{1,2,3}.txt` `/tmp/v4-out.txt` `/tmp/fc-out.txt` `/tmp/v8b-out.txt`（EC2 本机，未入库）
