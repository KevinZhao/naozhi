# Server Consumer-Side Interfaces — Contracts

> **Status**: 初稿 v0.1（2026-05-25），Phase 4 前补全完整契约。
> **Source**: [server-split-phase4-design.md](server-split-phase4-design.md) §四.1.3 / §四.4
> **Enforcement**: AST linter rule 4 (Phase 1 前完成) 验证 `// satisfies:` 注释与本文件方法集一致。

本文件枚举 server / dashboard / wshub 包对外部具体类型（如 `*dispatch.MessageQueue`）的依赖**接口**，并显式表达**跨方法时序契约**——这种 cross-method invariant 在单方法 godoc 里写不下，必须在本文档钉死。

---

## 1. `wshub.MessageEnqueuer` — Hub 写路径

**Consumer**: `internal/server/wshub.go` (Phase 4 后 → `internal/wshub/`)
**Producer**: `*dispatch.MessageQueue` (`internal/dispatch/msgqueue.go`)
**Source**: [internal/server/wshub_types.go](../../internal/server/wshub_types.go)

### 方法集

```go
type MessageEnqueuer interface {
    Enqueue(key string, msg dispatch.QueuedMsg) (isOwner, enqueued, shouldInterrupt bool, gen uint64)
    DoneOrDrain(key string, gen uint64) []dispatch.QueuedMsg
    Discard(key string)
    Mode() dispatch.QueueMode
    CollectDelay() time.Duration
}
```

### Cross-method 契约

#### CT-MQ-1: Enqueue → DoneOrDrain ownership

`Enqueue` 返回 `isOwner=true` 时，调用方 **必须** 在自己的 owner-loop 中持续调用 `DoneOrDrain(key, gen)` 直到返回空 slice 才能释放对该 key 的所有权。**违反**：同 key 后续 `Enqueue` 永远返回 `isOwner=false, enqueued=true`，但没人 drain → 队列堆积 + 用户看到"please wait"无法解除。

```
正确路径：
  isOwner, ok, _, gen := q.Enqueue(key, msg)
  if isOwner {
      go ownerLoop(key, gen, q)  // 必须实现 DoneOrDrain 循环
  }

owner-loop 退出条件：
  next := q.DoneOrDrain(key, gen)
  if len(next) == 0 { return }   // 释放所有权
  // 否则处理 next 中每条消息再下一轮 DoneOrDrain
```

#### CT-MQ-2: Discard 不调用 DoneOrDrain

`Discard(key)` 直接清空 key 的队列且不通知 owner-loop。一般用于 session reset / shutdown。**约束**：Discard 调用方必须保证 owner-loop 已通过其他途径退出（如 `ctx.Done()`），否则 owner-loop 会卡在下一次 `DoneOrDrain` 期待消息（实际为空，会立即返回，但语义上是"被强制释放"）。

#### CT-MQ-3: Mode 静态读取

`Mode()` 返回值在 `*MessageQueue` 生命周期内不变（构造时设定）。Hub 可缓存到 wsclient.go 局部 — 不需每次重读。

#### CT-MQ-4: CollectDelay 静态读取

同 CT-MQ-3。`CollectDelay()` 是配置值，调用一次读到位即可。

### 演化策略

- **加方法**：`MessageEnqueuer` 加方法 → `*dispatch.MessageQueue` 必须先实现，否则编译失败（var _ 编译期 gate）
- **改签名**：禁止；调用方约束变化要新增方法 + deprecated 路径

### 实现侧 godoc 模板

```go
// MessageQueue ...
//
// satisfies: server.MessageEnqueuer (internal/server/wshub_types.go)
//
// Cross-method contract (see docs/design/server-consumer-contracts.md):
//   - CT-MQ-1: Enqueue isOwner=true → caller drives DoneOrDrain to drain
//   - CT-MQ-2: Discard skips DoneOrDrain; caller must exit owner-loop separately
type MessageQueue struct { ... }
```

linter rule 4 (Phase 1 前) 解析此 godoc 头比对方法集。

---

## 2. `dashboard/<domain>` 子包 → `*wshub.Hub` (broadcaster)

**Consumer**: `internal/dashboard/cron/`, `internal/dashboard/session/`, etc.
**Producer**: `*wshub.Hub`
**Source**: 各子包 `deps.go` 内 `broadcaster` 接口

### 方法集

```go
type broadcaster interface {
    BroadcastSessionsUpdate()
}
```

### Cross-method 契约

#### CT-BC-1: BroadcastSessionsUpdate 是 best-effort

调用 `BroadcastSessionsUpdate()` 不保证立即广播——Hub 内部有 debounce（默认 100ms）合并多次调用。调用方**不能依赖**单次调用产生单次广播；**必须容忍**广播延迟到 debounce 窗口结束。

#### CT-BC-2: Shutdown 后无声丢弃

Hub 进入 Shutdown 流程后（`debounceClosed=true`）调用 `BroadcastSessionsUpdate` 是 no-op。dashboard 子包的 handler 在 server context 已 cancel 时也不应再调用。

---

## 3. (其他子包接口待 Phase 1+ 推进时补全)

随 Phase 1-3f 推进，每个 dashboard 子包定义自己的本地接口时，必须在本文件加一节，列出：

- Consumer / Producer
- 方法集
- Cross-method 契约（即便没有也要写"None — 方法间无序"）
- 演化策略

---

## 4. 规约总结

1. **Cross-method invariant 必须在本文档显式写**——godoc 行内表达不了多方法时序的，进本文件
2. **每个跨包接口都有锚点**：`#1.1`, `#1.2` 形式，CONTRACT 编号 `CT-MQ-1`, `CT-BC-1` 这种 `<scope>-<id>`，方便 godoc 反向引用
3. **AST linter rule 4** (Phase 1 前) 自动验证：实现侧 godoc 中的 `satisfies: <pkg>.<Iface>` 必须在本文档有对应 §
4. **接口加方法时**：本文件 + linter exemptions.yaml + 实现侧 godoc 三处必须同步更新（PR description 列出三处 diff 行号）

---

## 5. TODO Phase 4 前补全清单

- [ ] CT-BC-* 系列：dashboard 子包对 wshub 的依赖契约（Phase 1 先做 cron）
- [ ] CT-RT-* 系列：server / dashboard 对 `*session.Router` 的依赖（白名单具体类型，但 cross-method 契约仍需文档化，例如 `GetOrCreate` 后必须配套 `Remove` 或让 TTL 自然回收）
- [ ] AST linter rule 4 实现 + CI 集成
