# Phase C — 本地灰度实测报告

> **状态**: 完成（所有场景通过 + 发现并修复 2 个 bug）
> **测试日期**: 2026-05-06
> **测试方式**: 隔离 binary `/tmp/naozhi-passthrough` + 独立 config `/tmp/naozhi-passthrough.yaml` + 独立端口 9181 + 独立 state_dir `/tmp/naozhi-passthrough-shims`
> **生产保护**: 全程**未修改** `bin/naozhi`，systemd naozhi 服务未中断

## 1. 测试路径

Dashboard HTTP API（不走 IM 平台）：

- `POST /api/sessions/send` → `handleSend` → `sessionSend` → Passthrough 分支 → `runTurnPassthrough` → `Hub.sendWithBroadcastPriority` → `ManagedSession.SendPassthrough` → `Process.SendPassthrough`
- `POST /api/sessions/interrupt` → `Router.InterruptSessionSafe` → `InterruptViaControl` → stream-json `control_request`
- `/new` / `/clear` 文本消息 → `sessionSend` early branch → `Router.Reset` → 进程 Close → readLoop 退出 → `discardAllPending(ErrProcessExited)`
- `/urgent <text>` 文本消息 → sessionSend passthrough 分支识别前缀 → `runTurnPassthrough(..., "now")` → CLI priority:"now" 抢占

## 2. 场景结果

### C.3 单消息 smoke test ✅

- Send "Reply with just GREEN"
- CLI result = "GREEN"（3s 内完成，cost $0.047）
- EventLog 里没有 user replay 条目（replay 过滤正确）

### C.4 并发 burst 5 消息 ✅

原始实测首次发现 **spawn race bug**：5 个并发 goroutine 同时 GetOrCreate 同 key → 4 个失败于 shim socket dial guard ("refusing to clobber")。

**Bug 修复** (`router.go` GetOrCreate)：引入"重试循环"检测 `spawningKeys[key]` in flight，等 20ms 再试。修复后重测：
- 5 条消息 → 0 spawn error, 1 次 session spawn
- CLI 独立处理 APPLE（独立 replay），然后合并 BANANA+CHERRY+DATE+ELDERBERRY（4 slot 共享一个 turn result）
- Fan-out：head=BANANA 拿完整 text "BANANA"，follower=CHERRY/DATE/ELDERBERRY 拿 `MergedCount=4 + MergedWithHead + HeadText`（无 Text，避免 IM 刷屏）
- 4 个 goroutine 全部返回（4 次 "turn complete" log）

### C.5 /urgent 抢占 ✅

- T=0 long bash task（40s）
- T=3s `/urgent Say only PIVOT`（via dashboard `/urgent` 前缀）
- T=3s CLI emit `result subtype=error_during_execution`
- T=3s long slot 收到 `ErrAbortedByUrgent` 并返回（elapsed=3s）
- T=6s urgent replay 匹配 urgent slot → fan-out result="PIVOT."

验证了 priority:"now" 原生抢占机制 + `reapAbortedPreempted` 正确跳过 priority="now" 自身。

### C.6 /stop 软中断 ✅

原始实测首次发现 **State=Running 未设置 bug**：passthrough 模式下 `Process.State` 从未翻 Running，`InterruptViaControl` 检查 state != Running 就返回 `ErrNoActiveTurn` → dashboard 收到 `not_running` 无法中断。

**Bug 修复** (`passthrough.go` `onSystemInit` + `onTurnResult`)：passthrough turn 开始时设 State=Running；最后一个 slot 消费完 pending 后设 State=Ready。修复后重测：
- T=0 long bash
- T=3s `POST /api/sessions/interrupt` → `{"status":"ok"}`
- T=3s readLoop 收到 error_during_execution result → fan-out → slot 返回
- State 正确转换

### C.7 /new pending 清理 ✅

- 3 条消息排队（long + GAMMA + DELTA）
- `/new` → `Router.Reset` → `proc.Close()` → readLoop EOF → `discardAllPending(ErrProcessExited)`
- 3 个 goroutine 全部返回（每个都拿 `ErrProcessExited`）
- session 从 sessions 列表移除

**改进建议**（Phase D）：Reset 路径应显式走 `DiscardPassthroughPending(ErrSessionReset)` 而不是借用 process-exit 路径，UX 文案更准确。

## 3. 发现的 Bug + 修复

| # | Bug | 根因 | 修复位置 | 严重性 |
|---|---|---|---|---|
| B.1 | 并发 spawn race | 同 key 多 goroutine GetOrCreate → 4/5 spawn 失败 | `session/router.go` GetOrCreate 等待 `spawningKeys` 循环 | 高 — 实际会丢消息 |
| B.2 | InterruptViaControl 在 passthrough 下失效 | `Process.State` 从未设 Running → state check 直接返回 `ErrNoActiveTurn` | `cli/passthrough.go` `onSystemInit` 设 Running，`onTurnResult` 在 pending 空时设 Ready | 中 — /stop 无效 |

## 4. 生产保护确认

全程 `bin/naozhi` mtime 锁定在 08:29:50（phasec-revert HEAD 版本）：

```
stat -c "%Y %n" /home/ec2-user/workspace/naozhi/bin/naozhi
→ 1778056190 /home/ec2-user/workspace/naozhi/bin/naozhi
```

生产 shim 参数**未包含** `--replay-user-messages`：

```
4076419 .../bin/naozhi --config ...config.yaml
4076694 .../bin/naozhi shim run --key ... --cli-arg --verbose --cli-arg --setting-sources ... （无 --replay-user-messages）
```

systemctl is-active naozhi → active。

## 5. 未覆盖场景（留给 Phase D）

- IM 路径（飞书 /Discord 等）端到端 — 需要真实 bot 账号 + webhook
- Dashboard WS 双向事件流的 onEvent 实时 thinking 展示
- 跨 naozhi-restart 的 passthrough pending slot 恢复（ErrReconnectedUnknown）
- ACP 协议的 passthrough 降级路径
- 大消息（~1MB）的 passthrough flow
- 持续负载下 MaxPending=16 的压力点

## 5.1 ⚠️ 待 review 的设计决策

- **`/urgent` 命名 + 抽象**：当前是 naozhi 自创的命令，包装 CC 原生 `priority:"now"` 字段。CC TUI 本身没这个命令（只有 ESC 中断）。详细讨论见 `passthrough-mode.md` §11.8。候选：`/urgent` / `/now` / `/interrupt` / `/pre-empt` / 合并到 `/stop <msg>` / 前缀 `!!` / 完全不暴露给 IM 用户。**Phase D 启用前需要锁定**
- **IM 路径下 slash 命令的 agent 解析**：`/urgent` 和 `/stop` 硬编码 `agentID="general"`，多 agent 场景下会找错 session。详见 `passthrough-mode.md` §11.9

## 6. 总结

**Phase C PASS** — Passthrough 模式在 dashboard 路径上 ✅ 完整工作：

- ✅ 单消息
- ✅ 并发 burst + 合并 fan-out
- ✅ /urgent priority:"now" 抢占
- ✅ /stop 软中断
- ✅ /new pending 清理
- ✅ 2 个生产级 bug 发现 + 修复
- ✅ 全量单元测试通过（`go test ./internal/cli/ ./internal/session/`）
- ✅ 生产未受影响

准备进入 Phase D（IM 路径验证 + 灰度上线）。
