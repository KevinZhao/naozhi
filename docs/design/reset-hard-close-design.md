# Router.Reset 硬关闭设计（fresh_context 可靠重建）

**状态**: 待实现  
**作者**: Kevin Zhao  
**日期**: 2026-04-26  
**关联 bug**: cron `c74dbd9f0229c991` 每次触发必然失败 — `shim already listening on ...sock: refusing to clobber`

---

## 1. 问题陈述

### 1.1 现象

唯一的定时任务（每 6h 跑项目 review）带 `fresh_context=true`，上次触发（2026-04-25 22:09:47）失败，`cron_jobs.json` 写入 `last_error`：

```
session cron:c74dbd9f0229c991: spawn process: start shim:
shim already listening on /home/ec2-user/.naozhi/run/shim-ad9e3b855f6ddfb7.sock:
refusing to clobber
```

日志时序（100 ms 级）：

```
22:09:47.000  cron job executing            (fresh_context=true)
22:09:47.100  readLoop: shim connection closed   ← CLI 已退出
22:09:47.100  session reset                      ← Router.Reset 立即返回
22:09:47.100  creating new session               ← GetOrCreate
22:09:47.100  ERROR: refusing to clobber         ← StartShim dial-first guard 命中
22:09:47.851  discovered live shim pid 938704
22:09:47.851  orphan shim found, shutting down   ← 750 ms 后 Discover 才补救
```

### 1.2 根因

`cli.Process.Close()` 发 `close_stdin` 给 shim（`internal/cli/process.go:1180`）。shim 收到后只关闭 CLI 的 stdin；shim **自身**仍在 30 s grace period 里持有 `listener` 和 socket 文件（`internal/shim/server.go:168-198`）。

紧接着 `Router.Reset` 返回，`Cron.execute` 调 `GetOrCreate → spawnSession → StartShim`。`StartShim` 在 bind 前做 dial-first guard（`internal/shim/manager.go:198`、`internal/shim/server.go:1008`）：

```go
if conn, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond); err == nil {
    _ = conn.Close()
    return fmt.Errorf("shim already listening on %s: refusing to clobber", socketPath)
}
```

老 shim 还在 listening → guard 命中 → 失败。

`socketPath` 由 `KeyHash(key)` 决定（`internal/shim/manager.go:162`），同一 key 必然同一 socket，所以 **fresh_context 的 Reset+Recreate 在同 key 下稳定复现此竞争**。

### 1.3 为什么 dial-first guard 必须保留

guard 2026-04-25 才刚加（UCCLEP-2026-04-25 回归修复）。如果在新 shim bind 前盲删老 socket 文件，老 shim 的 listener fd 仍然被内核持有但无人能 dial，变成不可达的 zombie，同时它还占着 CLI 槽位、继续缓冲消息。这是绝对不能再回退的。

### 1.4 不是局部问题

这个竞争不限于 cron fresh_context：

- `ResetAndRecreate`（router.go:1599）在 `proc.Close()` 之后直接调 `spawnSession`，**同样命中**。
- 任何对活跃 session 做 Reset 然后立刻重开的路径（未来可能的"reset session" 按钮等）都会踩。
- 日志里 `local:takeover:home-ec2-user-workspace-efa-validation:general` 的 3 min 重试风暴也是同一个坑 —— shim 30 s 没真死，reset + spawn 反复打架。

## 2. 方案对比

| 方案 | 做法 | 优点 | 缺点 |
|------|------|------|------|
| **A**：`Close()` 改用 `shutdown` | process.Close 发 `shutdown` 而非 `close_stdin`，等 socket 文件消失 | 最彻底，所有 Reset 路径受益 | 语义扩散 —— 原来的"软关 stdin"概念没了；Close 的 5 s 超时需要与 shim 5 s waitOrKill 对齐 |
| **B**：Router.Reset 额外等 socket 消失 | proc.Close 不变，Reset 之后 sleep+stat | 改动面小 | Close 仍然返回早 → Close 单独调用的路径（evict、Shutdown）仍然会在 30 s 内占 socket；半吊子 |
| **C**：ResetAndRecreate 专属路径 | 给 fresh_context 走专用路径，`SendMsg("shutdown") → waitSocketGone → spawn` | 不改 Close 语义 | ResetAndRecreate 已经存在但 bug 未修；调用方（cron fresh_context 目前走的是 Reset+GetOrCreate 两步）必须迁移；未来新调用方又可能踩坑 |
| **D**：StartShim 再重试 | 命中 refusing to clobber 时自旋等 | 最小改动 | 治标不治本；正常关闭路径还是脏；shim 的 30 s grace 被 naozhi 隐式"赶时间"背下来 |

**选 A**。理由：

1. 根因是 `Close` 语义和使用者预期错位 —— 所有调用 `proc.Close()` 的地方（Reset、Shutdown、evict、TOCTOU guards）都希望"这个 session 和它的 shim 都彻底走掉"。真正需要保活 shim 的只有 naozhi 优雅退出时的 `DetachAll`，它走的是独立的 `Detach` 接口，本来就不该和 Close 混。
2. 改动局限在一个函数；调用方零改动。
3. shim 端 `shutdown` 分支（`internal/shim/server.go:814-823`）本来就已经做了 `closeStdin → waitOrKill(5s) → initiateShutdown`，再加上 defer 链 `listener.Close() + os.Remove(socketPath)`，语义比 `close_stdin` 干净。

## 3. 详细设计

### 3.1 语义对齐：`Close` = "session 和 shim 都走完"

改 `cli.Process.Close()`（`internal/cli/process.go:1180-1190`）：

```go
// Close gracefully shuts down the CLI and instructs the shim to exit.
// After Close returns (or times out and falls through to Kill), the shim
// socket file is guaranteed to be either gone or imminently gone — callers
// that immediately spawn a fresh shim for the same key will not hit the
// dial-first guard.
func (p *Process) Close() {
    _ = p.shimSend(shimClientMsg{Type: "shutdown"})
    timer := time.NewTimer(processCloseTimeout)
    defer timer.Stop()
    select {
    case <-p.done:
    case <-timer.C:
        slog.Warn("process close timeout, force killing", "pid", p.cliPID)
        p.Kill()
    }
}
```

**单行改动**：`close_stdin` → `shutdown`。

### 3.2 shim 端验证

`internal/shim/server.go:814-823` 的 `shutdown` 分支已经做对了：

```go
case "shutdown":
    if s.cli.alive() && time.Since(s.startedAt) < 60*time.Second {
        slog.Warn("ignoring shutdown: CLI alive and shim recently started", ...)
        return
    }
    s.cli.closeStdin()
    s.cli.waitOrKill(5 * time.Second)
    s.initiateShutdown()
    return
```

然后主 loop 的 `case <-s.done` 分支：

```go
case <-s.done:
    slog.Info("shutdown initiated")
    cli.closeStdin()
    cli.waitOrKill(5 * time.Second)
    slog.Info("exiting: shutdown done")
    return nil
```

Run 函数 defer 链（server.go:95-96）：

```go
defer listener.Close()
defer os.Remove(cfg.SocketPath)
```

Run 返回 → defer 执行 → **socket 文件被 unlink + listener fd 被 close**。`p.done` 关闭由 shim 连接 EOF 触发 readLoop 退出（process.go:426）。时序保证：shim socket 消失发生在 `p.done` 关闭之前或同一时间点（两者都由 shim 进程真正退出驱动）。

⚠️ **已知子问题**：`shutdown` 分支有个"60 s 内拒收"的保护（`time.Since(s.startedAt) < 60*time.Second && s.cli.alive()`）。如果 fresh_context 触发时距离 shim 启动不足 60 s，shutdown 会被忽略，shim 单方面 return 到 handleClient 退出但主 loop 不退；socket 还在。

**缓解 1（推荐）**：把这个保护改成只针对**无 client 连接的 shutdown** —— 有 authed client 发 shutdown 本质是 naozhi 自己的意愿，不需要防护。已连接意味着 naozhi 知道它在干什么。

**缓解 2**：保留保护，但 fresh_context 路径命中时 fall through 到 Kill 兜底，依赖 `processCloseTimeout` 计时（当前 5 s） + `processExit → close(done) → readLoop 退出 → select timeout → Kill`。Kill 走 shim conn `Close()` → shim 端 `handleClient return` + CLI 被 shim-pid 的 setsid 进程组 SIGKILL 但 socket 仍然不会被删。这条路仍然坏。

**结论**：必须动这个 60 s 保护。见 §3.3。

### 3.3 shim 侧改动：shutdown 60s guard 放宽

当前 `internal/shim/server.go:815` 条件：

```go
if s.cli.alive() && time.Since(s.startedAt) < 60*time.Second {
```

改为：

```go
s.mu.Lock()
clientConn := s.clientConn
s.mu.Unlock()
// Only refuse an "early shutdown" when it comes from a non-authenticated
// path. Authenticated clients (naozhi) issuing shutdown within the 60s
// window means naozhi has made the deliberate choice to tear this shim
// down — e.g. fresh_context cron, explicit Router.Reset, config drift
// handling. Blocking those would leave the shim's socket live for 30+
// seconds and cause the "refusing to clobber" regression on fast restart.
//
// The 60s window was originally added to protect against handshake
// glitches where a half-ready shim receives an errant shutdown before
// buffers are primed. That's only meaningful when the client isn't the
// one actively driving the lifecycle.
if s.cli.alive() && time.Since(s.startedAt) < 60*time.Second && clientConn == nil {
    slog.Warn("ignoring shutdown: CLI alive, shim recently started, no client attached",
        "age", time.Since(s.startedAt).Round(time.Millisecond))
    return
}
```

处理 case：

- **authenticated client 发 shutdown**：总是生效（fresh_context、Reset、drift）
- **无 client 的 spurious SIGTERM/SIGUSR2 < 60s**：仍然拒绝（原保护意图）

这里有个微妙点：`handleClient` 是在 `setClient` 之后进入消息 loop（`server.go:700-780` 附近，已读过），所以 shutdown 消息到达这个 switch 时 `clientConn != nil` 必然成立。上面的判断是对的。

### 3.4 调用方兼容性审计

搜 `proc.Close` / `(*Process).Close` 的所有调用点：

| 位置 | 场景 | 新语义影响 |
|------|------|-----------|
| `router.go:846` | spawnSession 竞争窗口发现 session 被另一路 spawn 抢先，丢弃自己刚造的 proc | **正向**：丢弃的 proc 现在会把 shim 也杀干净（原来会留个 30 s grace 僵尸） |
| `router.go:1096` | 另一竞争窗口 | 同上 |
| `router.go:1298` | TOCTOU guard 1 | 同上 |
| `router.go:1371` | TOCTOU guard 2 | 同上 |
| `router.go:1504` | evict 最老空闲 session | **正向**：evict 后立即腾出 socket，避免后续 spawn 踩同一 key（虽然 evict 一般是不同 key，但保险） |
| `router.go:1550`（Reset 内部） | 显式 Reset | **正向**：本次修复的核心路径 |
| `router.go:1590`（ResetAndRecreate 内部） | 显式 Reset+Create | **正向**：本次修复的核心路径 |
| `Router.Shutdown` 路径（若有） | 需要确认是否想保活 | 见下 |

**关键审计**：naozhi 优雅退出时走的是 `ShimManager.DetachAll()`（`internal/shim/manager.go:738-755`），那是独立接口，不经过 `proc.Close`。所以 Close 改语义不会影响"shim survives naozhi restart"这条核心设计。

再查一次以确认：

- `router.Shutdown` / `managed.Shutdown` 如果有调 `proc.Close` 会破坏热重启语义 —— 必须验证。见实现清单第 5 项。

### 3.5 超时预算对齐

时间线（最坏情况）：

```
t=0      naozhi: shimSend("shutdown")
t=0+ε    shim: closeStdin(CLI)
t=0+ε    shim: waitOrKill(5s)                 — CLI 不退就 kill
t=5s     shim: initiateShutdown() → done closed
t=5s+ε   shim: Run main loop: closeStdin+waitOrKill(5s) — 第二轮保险
                                                        （CLI 已死会立即返回）
t=5s+ε   shim: listener.Close + os.Remove(socket)
t=5s+ε   shim: 进程退出，naozhi 的 shimConn EOF
t=5s+ε   naozhi: readLoop 返回 → close(p.done)
t=5s+ε   naozhi: Close select 唤醒 → 返回
```

`processCloseTimeout` 当前是 5 s（process.go:68）。最坏 5 s + ε，不超时的话 CLI 会先响应 stdin 关闭然后 exit，shim 端 `waitOrKill(5s)` 的第一轮提前返回，实际 ≪ 1 s。

需要把 `processCloseTimeout` 从 5 s 提到 **8 s**：shim 端是 `waitOrKill(5s)` 两轮串联（理论最坏 10 s，但第二轮走到的条件是 CLI 存活且没响应第一轮 kill，极罕见），加上网络和 shim 进程退出开销。给 8 s 留一点缓冲，超过就 Kill 兜底，Kill 走 `shimConn.Close()` → shim 端 handleClient 退出 → 但不一定触发 initiateShutdown（naozhi 端直接关 conn 只是断连）。

⚠️ **Kill 后的兜底**：Kill 当前只发 `kill` 消息后立即关 conn。`kill` 消息会让 shim 把 CLI kill 掉（`server.go:803-804`），但不走 initiateShutdown。然后客户端掉线 → grace timer 起来 30 s → 30 s 后 socket 才消失。

所以 **Kill 路径本身仍然有 30 s socket 残留**。如果 shutdown 超时走到 Kill，下一次 fresh_context 仍然踩雷。

**补救**：把 `Kill` 改成也走 shutdown 语义 —— 发 `shutdown` 后 conn.Close。不过 Kill 一般是 force kill，调用者的预期是"立即返回"；如果在 Kill 里同步等 socket 消失，Shutdown 超时退路就不是退路了。

**选定方案**：
- `Close()` 发 `shutdown` 等 done，超时后调 `Kill()`
- `Kill()` 除了发 `kill` 再额外发一条 `shutdown`（不等），依赖 shim 端把两条都处理掉
- 验证 shim 在已经 close conn 的情况下仍能把最后一条 shutdown 处理掉：shim 的 handleClient 是 line loop，收到 kill 处理完下一轮 read 会 EOF，然后 return，**不会处理后续消息**。
- 所以必须**在 Kill 里走另一条路径**：关 conn 前给 shim 发 SIGUSR2（立即 shutdown），这是 shim 原生支持的快速通道（`server.go:200-207`）。

**改 Kill**（实现清单第 3 项）：

```go
func (p *Process) Kill() {
    p.killOnce.Do(func() {
        close(p.killCh)
        p.shimWMu.Lock()
        defer p.shimWMu.Unlock()
        p.shimConn.SetWriteDeadline(time.Now().Add(time.Second))
        _ = p.shimSendLocked(shimClientMsg{Type: "kill"})
        p.shimConn.Close()
        // Send SIGUSR2 to ensure the shim tears down its listener+socket
        // immediately instead of waiting out the 30s disconnect grace
        // period. Without this, a Kill path leaves the socket file live
        // and the next StartShim for the same key fails the dial-first
        // guard. shimPID is populated on spawn; best-effort only.
        if p.shimPID > 0 {
            _ = syscall.Kill(p.shimPID, syscall.SIGUSR2)
        }
    })
}
```

需要 `Process` 结构体持有 `shimPID`。查一下现在有没有：

### 3.6 shimPID 可用性

process.go:1187 里 `slog.Warn("...", "pid", p.cliPID)` —— 说明至少有 cliPID。shimPID 是否已经有字段存？

**实现时任务**：grep 确认；如果没有，spawn 时从 shim ready 消息里取（manager.go:227 已有 `PID` 字段），传进来。

### 3.7 orphan shim 逻辑兼容

`router.go:692-713` 的 "orphan shim found, shutting down" 分支已经用的是 `handle.Shutdown()`（manager.go:711），那条路径的逻辑已经是对的。本次改动不影响它。

### 3.8 fresh_context 的"上一跳 GetOrCreate 创建的 shim 还在"额外保护

即便 Close 改好，`Cron.execute` 的流程是：

```go
if fresh {
    s.router.Reset(key)           // 改后：同步杀 shim + 等 socket 消失
    ...
}
sess, _, err := s.router.GetOrCreate(ctx, key, opts)
```

`Router.Reset` 的 `proc.Close()` 同步阻塞 5-8 s，cron 的 `execTimeout`（默认 5 min）足够覆盖。Reset 之后 socket 必然已消失（§3.5 时序保证）→ `StartShim` 的 dial-first guard 直接走 `os.Remove` 空操作分支。

**但是**：Reset 是 best-effort，如果 `proc == nil || !proc.Alive()`，它直接跳过 Close（router.go:1549）。存在这样的场景：上次 cron 执行留下的是 dead process + 活着的 shim（比如 naozhi 刚接管 shim，process 还没 readLoop 起来）—— 本次设计下 Reset 跳过 Close，shim 仍然活。Router.Reset 其实不需要管"这个 key 对应的 shim 还活不活"这件事，因为如果 process 已经死了，对应 shim 的 watchdog/idle timeout 会自理；但 fresh_context 要求"下一 spawn 立即可用"。

**加固**：Reset 末尾额外等 socket 文件消失（stat 轮询，最长 2 s，保底）。如果到时还在，fall through；StartShim 仍然会 refusing to clobber，但至少比现在强。

具体实现见 §4。

## 4. 实现清单

### 4.1 内部 API 新增

**`internal/shim/manager.go`** 新增工具函数：

```go
// WaitSocketGone polls the socket path until it disappears or the deadline
// elapses. Returns true when the socket is gone, false on timeout. Used by
// callers that intend to spawn a fresh shim on the same key and need to
// observe the prior shim's listener fully released before bind. Interval
// is 20 ms — socket unlink is synchronous in the peer shim's Run defer
// chain so normally this returns in one tick.
func WaitSocketGone(socketPath string, maxWait time.Duration) bool {
    deadline := time.Now().Add(maxWait)
    for {
        if _, err := os.Stat(socketPath); os.IsNotExist(err) {
            return true
        }
        if time.Now().After(deadline) {
            return false
        }
        time.Sleep(20 * time.Millisecond)
    }
}
```

### 4.2 `cli.Process.Close` / `Kill` 改写

- `processCloseTimeout`: 5 s → 8 s
- `Close()`: `close_stdin` → `shutdown`
- `Kill()`: 加 `syscall.Kill(p.shimPID, syscall.SIGUSR2)` 兜底

若 `Process` 无 `shimPID` 字段：
- `struct Process` 加 `shimPID int`
- spawn 入口（grep `shimPID` / spawn ready 消息解析点，大概率在 `wrapper.Spawn` 或 `SpawnReconnect`）接住 ready 消息里的 PID，塞进 Process

### 4.3 `shim.server` shutdown guard 放宽

`server.go:815` 的条件加 `&& clientConn == nil`。

### 4.4 `Router.Reset` 尾部加 socket 等待

在 `router.go:1551` 的 `proc.Close()` 之后：

```go
if proc != nil && proc.Alive() {
    proc.Close()
    // Belt-and-suspenders: even if Close observed p.done, a pathological
    // shim might defer its listener close behind other exit work. Give
    // the socket file up to 2s to disappear before notifying dashboards
    // so the next GetOrCreate for this key hits a clean StartShim.
    if r.shimManager != nil {
        socketPath := shim.SocketPath(shim.KeyHash(key))
        _ = shim.WaitSocketGone(socketPath, 2*time.Second)
    }
}
```

查 `r.shimManager` 字段是否存在 —— 看 router 的依赖注入。如果 router 没有 shim manager 的直接引用（可能走 wrapper），需要从 wrapper 取或者直接算 socketPath（因为 KeyHash + SocketPath 都是纯函数）。

### 4.5 `ResetAndRecreate` 同样处理

`router.go:1596` 的 `proc.Close()` 后也加 WaitSocketGone。

### 4.6 确认 `router.Shutdown` / `ManagedSession.Shutdown` 不踩

grep：在 Shutdown 路径里如果调了 `proc.Close`，会误关所有 shim，破坏热重启。需要验证。

预期结果：Shutdown 走的是 `DetachAll`（manager 层）或 `proc.Detach`（cli 层），不是 `proc.Close`。如果发现有调用点，改成 `proc.Detach`。

## 5. 测试清单

### 5.1 单元测试

**`internal/cli/process_test.go`** 新增 `TestProcess_Close_SendsShutdown`：
- mock shim server
- 调 `p.Close()`
- 验证 mock 收到的消息 Type == "shutdown"（之前是 "close_stdin"）

**`internal/shim/server_test.go`** 新增 `TestShutdown_WithAuthedClientWithin60s`：
- 启动 shim
- 客户端 attach 并 auth 成功
- 立即（< 60 s）发 shutdown
- 验证 shim 退出、socket 消失
- 当前会被拒，新代码应放行

**`internal/shim/server_test.go`** 保留 `TestShutdown_NoClientWithin60s`（如果已有）：
- 启动 shim
- 不 attach，60 s 内发 SIGTERM 或其他触发 shutdown 的通道
- 验证仍然被拒（原保护意图）

**`internal/shim/manager_test.go`** 新增 `TestWaitSocketGone`：
- 启动 shim
- 另一 goroutine 触发 shim shutdown
- 调 `WaitSocketGone(path, 5s)`
- 验证 true 且用时 < 1 s

### 5.2 集成测试

**新增 `internal/session/router_fresh_context_test.go`**（可能已有类似文件）：
- 完整启动一个 router（in-process）
- `GetOrCreate(key, opts)` → 触发 spawn
- `Reset(key)`
- **立即** `GetOrCreate(key, opts)`
- 验证返回新 session、无 "refusing to clobber" 错误
- 重复 3 次，每次间隔 < 200 ms

### 5.3 手工验证

部署后观察 cron `c74dbd9f0229c991`：

1. 部署新二进制（`systemctl restart naozhi`）
2. 等下一次触发（`last_run_at + 6h`，或者 dashboard 手动 trigger）
3. 期待 `last_error` 为空、`last_result` 有内容
4. 跑 `ls /home/ec2-user/.naozhi/run/` —— 无残留 socket（或者只有正常活着的）
5. 连续手动 trigger 2 次，间隔 10 s，两次都要成功

## 6. 风险与回滚

### 6.1 风险

**R1 — 破坏热重启**：如果误改了 Shutdown 路径调用 Close，会导致 naozhi 重启时把所有 shim 也杀了，热重启失效。  
缓解：§3.4 的审计；新增一条集成测试 `TestRouter_Shutdown_DoesNotKillShim`（若尚无）。

**R2 — Close 阻塞时间变长**：从"发消息立即返回 + 5 s 最坏"变成"等 shim 真的退 5-8 s"。调用方如 evict、TOCTOU guards 的热路径可能敏感。  
评估：evict 只在超过 MaxProcs 时触发，低频。TOCTOU guards 在 spawn 抢跑时才走（极低频）。Reset 是显式操作，延迟是可接受的。不阻塞热路径。

**R3 — shim 端 waitOrKill 超时 CLI 未退**：shim 会 SIGKILL CLI，session 状态在 EventLog 里可能留半截数据。  
评估：现在也是这样（shutdown 分支原本就会 waitOrKill）。无新增风险。

**R4 — SIGUSR2 发错进程**：shimPID 存的是 spawn 时的值；如果 shim 已经退出、PID 被复用，SIGUSR2 可能杀到无关进程。  
缓解：Kill 的 SIGUSR2 是兜底之兜底，先发 `kill` 消息 + conn.Close，SIGUSR2 只在 conn 已关但 shim 仍活的极端场景起作用。加 PID 有效性检查：`os.FindProcess(pid).Signal(syscall.Signal(0))` 成功才发。更稳的做法：检查 `/proc/<pid>/comm` 包含 "naozhi" 或 "shim" 字样。实现时取前者，简单够用。

### 6.2 回滚

所有改动在 2 个文件（`process.go` + `server.go`）加 router 2 处 WaitSocketGone。回滚单个改动 = 改回 `close_stdin`、回退 guard 条件 —— 1 个 commit revert。

## 7. 部署

按 `docs/ops/naozhi-deploy-skill.md` 流程：review → build → test → deploy → push。

---

**实现顺序**：

1. shim.server 60s guard 放宽（§3.3）+ 单测
2. cli.Process `shimPID` 字段 + spawn 时接住
3. cli.Process.Close 改 shutdown + 单测
4. cli.Process.Kill 加 SIGUSR2 兜底 + 单测
5. shim.manager.WaitSocketGone 新增
6. router.Reset / ResetAndRecreate 末尾加 WaitSocketGone
7. router.Shutdown 路径审计（确认无 proc.Close 调用）
8. 集成测试
9. 部署验证
