# RFC: cron NotifySender — 切断对 platform map 的反向依赖

> **状态**: Draft v1（待评审）
> **作者**: naozhi team (cron-cr)
> **创建**: 2026-06-04
> **范围**: 用一个 cron-local 的 NotifySender 接口替换 `internal/cron` 对 `internal/platform` 具体 map 的直接依赖，沿用 cronSessionAdapter 的 IoC 模式把 platform 翻译挪到 wireup 层
> **关联 issue**: #725
> **关联代码**:
> - `internal/cron/scheduler.go:21`（`import platform`）、`:166`（`SchedulerConfig.Platforms`）、`:396`（`configMapsPtr atomic.Pointer[cronConfigMaps]`）、`:1056`（`cronConfigMaps.platforms`）、`:1206`（`maps.Clone(cfg.Platforms)`）
> - `internal/cron/scheduler_notify.go:260`（`notifyTarget`）、`:282`（`s.configMaps().platforms[plat]`）、`:317`（`p.MaxReplyLength()`）、`:321`（`platform.SplitText`）、`:356`（`platform.ReplyWithRetry`）、`:269`（stopCtx 短路）
> - `internal/metrics/metrics.go:299`（`CronNotifyPartialTotal`）
> - `internal/wireup/schedulers.go:89/193`（`Platforms` 注入）、`internal/wireup/cron_router_adapter.go`（cronSessionAdapter 先例 R20260527122801-ARCH-1 / #1318）
> - `cmd/naozhi/cron_no_session_import_test.go`（import-edge pin 的先例）

---

## 1. Background & problem

### 现状

`internal/cron` 直接 import `internal/platform` 并持有一份 `map[string]platform.Platform`：

- `SchedulerConfig.Platforms map[string]platform.Platform`（`scheduler.go:166`），在 `NewScheduler` 经 `maps.Clone` 存入 `cronConfigMaps.platforms`（`scheduler.go:1206`、`:1056`），由 `atomic.Pointer[cronConfigMaps]` 发布（`scheduler.go:396`）。
- `notifyTarget`（`scheduler_notify.go:260`）是唯一真正消费这张 map 的生产代码路径：
  1. `p := s.configMaps().platforms[plat]`（`:282`）按 platform 名查具体 adapter；
  2. `maxLen := p.MaxReplyLength()`（`:317`）；
  3. `chunks := platform.SplitText(text, maxLen)`（`:321`）；
  4. `platform.ReplyWithRetry(replyCtx, p, OutgoingMessage{…}, limits.PlatformReplyMaxAttempts)`（`:356`）。
- 这条路径还编织了三块横切逻辑：硬 chunk 上限 `cronNotifyMaxChunks=5`（`:124`、`:329`）、partial-delivery 遥测 `metrics.CronNotifyPartialTotal.Add(1)`（`:345`、`:370`）、以及 `stopCtx` 短路 + replyCtx 父链（R243-SEC-14 / #799，`:269`、`:298-316`）。

### 为何是问题

依赖箭头是「反向」的。`internal/cron` 是一个调度域，理论上不应该知道 IM 平台的存在；但它直接 import `internal/platform` 并对其 `Platform` 接口、`SplitText` / `ReplyWithRetry` 自由函数、`DefaultMaxReplyLen` 常量、`OutgoingMessage` 结构体做硬编码调用。这与项目已经确立的「cron 只看 cron-local 类型」不变量不一致——session 域的反向依赖已经在 R20260527122801-ARCH-1（#1318）用 cronSessionAdapter 切断（见 `scheduler.go:98-102`、`scheduler_session.go:12`），platform 域是同一笔技术债的残留。

具体可观察症状：

- `internal/cron/scheduler.go` 仍出现在 `grep -rln 'internal/platform' internal/cron/*.go` 的非测试结果里（连同 `scheduler_notify.go`），与 session 域的「零反向 import」状态不对称。
- `scheduler.go:368-382` 的 godoc 已经记录了 R219-ARCH-8（#670）提出过 `Notifier` 接口方案，但因为「refactor entangles with deliverNotice's chunked send loop … per-target retry budget … four call sites」被 defer，挂到 cron-sysession-merge Phase E。该 Phase 至今未落地（cron-sysession-merge RFC 状态为 Implemented v3，Phase C deferred），#725 是把它从大重构里拆出来单独推进的窗口。

这不是运行时 bug——现状行为正确。这是一个架构/可维护性问题：反向依赖让 cron 的单测必须 import platform（见 `notify_target_partial_test.go` 等 5 个测试文件），也让未来给 cron 换 notify 后端（如 dashboard-only 部署、或把 notify 走 dispatch 统一错误处理）必须改 cron 包本体。

---

## 2. Goals & non-goals

### Goals

- G1：让 `internal/cron` 的**生产代码**不再 import `internal/platform`（或至少把这条依赖收敛到一个明确、可被 import-edge pin 守住的接口边界）。
- G2：沿用 cronSessionAdapter 先例——cron 定义 cron-local 接口，`internal/wireup` 持有 `platform` 与 `cron` 两个类型宇宙并构造 adapter。
- G3：完整保留现有 notify 协议语义：chunk 上限、partial 遥测、stopCtx 短路 + replyCtx 父链、empty-text no-op、platform-not-found WARN、resolveNotifyDecision 优先级阶梯。**零行为变更**。
- G4：保留所有已 pin 的回归测试可通过（stopCtx、chunk cap、partial、empty-text、cap-drop accounting、background-ctx anchor）。

### Non-goals

- NG1：**不**把 notify 协议统一进 dispatch 的 `replyText`（#670 的完整版）。那需要把 chunking 推进 Notifier 或把 chunk 元数据回吐给 cron（multi-error 契约），属于 cron-sysession-merge Phase E 的更大范围，本 RFC 显式不做。
- NG2：**不**改 `CronNotifyPartialTotal` 的指标名、语义或归属层（见 §3 方案 A 的遥测归属讨论——Phase 1 不搬遥测）。
- NG3：**不**引入热重载 / 动态 platform 注册（`configMapsPtr` 的 atomic.Pointer 已为此预留，但本 RFC 不消费该能力）。
- NG4：**不**改 on-disk 格式（`cron_jobs.json`）、不加 config flag、不动 `NotifyTarget` 结构体的 IM 坐标语义。
- NG5：**不**重命名 `NotifyTarget` / `NotifySource` / `NotifyDecision` / `resolveNotifyDecision`——这些已是 cron-local 类型，与 platform 无关，不在本次切断范围内。
- NG6：**不**触碰 `deliverNotice` 的异步 triggerWG 契约（R242-GO-13/14）与 Stop drain 语义。

---

## 3. Alternatives considered

### 方案 A（推荐方向，但分阶段）：cron-local `NotifySender` 接口 + wireup adapter，**整段 notify 协议留在 cron**

cron 定义一个窄接口，仅抽象「按 platform 名拿到一个能 split + reply 的发送器」，把 `platform.Platform` / `SplitText` / `ReplyWithRetry` / `DefaultMaxReplyLen` 全部藏到接口背后：

```go
// internal/cron 内（无 platform import）
type NotifySender interface {
    // Lookup 返回该 platform 的发送句柄；不存在返回 (nil, false)。
    Lookup(platform string) (PlatformReplier, bool)
}

type PlatformReplier interface {
    MaxReplyLength() int
    Reply(ctx context.Context, chatID, text string) (msgID string, err error)
}

// SplitText 也需要 cron-local 化：要么 cron 内置一份等价实现，
// 要么 NotifySender 暴露 Split(text string, maxLen int) []string。
```

`notifyTarget` 改为：`r, ok := s.notifySender.Lookup(plat)` → `r.MaxReplyLength()` → split → `r.Reply(replyCtx, chatID, chunk)`（内含 retry 预算）。chunk 上限、partial 遥测、stopCtx 短路**全部留在 cron**，因为它们是 cron 的调度策略，不是 platform 的职责。

wireup 写一个 adapter：`platformNotifySender{platforms map[string]platform.Platform}`，`Lookup` 返回一个包住 `platform.ReplyWithRetry(ctx, p, OutgoingMessage{…}, limits.PlatformReplyMaxAttempts)` + `platform.SplitText` 的 `PlatformReplier`。

**遥测归属**：`CronNotifyPartialTotal` 留在 cron 的 `notifyTarget` 循环里（不动）。这是正确的——partial 是 cron 调度策略（「chunk 失败就 abort 避免交错」「deadline 截断」）的产物，不是 platform 的产物。adapter 只负责单次 Reply，partial 的判定逻辑仍在 cron。

**stopCtx 语义（R243-SEC-14）**：完全留在 cron。`replyCtx` 仍由 cron `context.WithTimeout(s.stopCtx, cronNotifyTimeout)` 构造并传给 `r.Reply(replyCtx, …)`。adapter 的 `Reply` 把 ctx 透传给 `platform.ReplyWithRetry` 的第一个参数。短路 guard（`:269`）原样保留。

**胜出理由**：最小化跨包搬迁，遥测 / stopCtx / chunk 策略零迁移，与 cronSessionAdapter 先例 1:1 对应（cron 定义接口、wireup 建 adapter、import-edge test pin）。代价是 `SplitText` 需要 cron-local 化（内置一份或经接口暴露）——这是唯一的实质决策点。

### 方案 B：窄的 per-platform `Sender`，但 cron 仍 import platform

只把 map 的 value 类型从 `platform.Platform` 换成一个本地 `interface{ MaxReplyLength() int; Reply(...) }`，但 `SplitText` / `ReplyWithRetry` / `OutgoingMessage` / `DefaultMaxReplyLen` 仍直接调 `platform.*`。

**不选**：这不能达成 G1——cron 仍 import platform（SplitText/ReplyWithRetry 是 platform 包的自由函数）。triage 已指出「option B is a narrow per-platform Sender that does NOT fully sever」。只在需要避免把 SplitText 搬动时作为退路。

### 方案 C：把整段 notify 协议（~100 行 chunk loop + retry + partial 遥测 + stopCtx 短路）搬到 cmd/wireup，cron 只持 NotifyTarget + 一个 `Notify(ctx, target, text)` 回调

cron 完全不知道 chunk / retry / 平台。

**不选**：跨 cron/cmd seam 搬迁 100 行带遥测和并发契约的协议，风险高（partial 遥测会从 cron 包移出 → 现有 `notify_target_partial_test.go` 的 in-package 断言全部要重写；stopCtx 父链要重新建立；deliverNotice 的 triggerWG drain 契约要跨包重新论证）。triage 标 MAYBE 且明确这是「relocates the ~100-line notify protocol across the cron/cmd seam」——属于需要先评审的大改，不在 Phase 1。这是 cron-sysession-merge Phase E 的归宿。

---

## 4. Test strategy

### 既有回归点（必须继续通过，不改语义）

- `notify_stopctx_test.go` / `notify_stopctx_short_circuit_test.go`：stopCtx 取消后 `notifyTarget` 2s 内返回；nil stopCtx 仍正常发送。
- `notify_target_chunk_cap_test.go` / `notify_target_cap_drop_accounting_test.go`：chunk 上限截断 + dropped 计数 + WARN。
- `notify_target_partial_test.go`：partial 路径 bump `CronNotifyPartialTotal` 恰好 ≥1。
- `notify_target_empty_text_test.go`：empty-text 不触达 Reply。
- `notify_background_ctx_test.go`：源码级 anchor pin（`func (s *Scheduler) notifyTarget(` 仍在、仍引用 `s.stopCtx`、不用 `context.Background()`）。**这是个源码字符串扫描测试，方案 A 重写 `notifyTarget` 函数体时必须保留这些 anchor 字符串**，否则会误报回归。
- `config_maps_atomic_test.go`：`cm.platforms["feishu"]` 索引——若 `cronConfigMaps.platforms` 改类型，此测试要随接口调整。
- `deliver_notice_async_test.go`：triggerWG 异步 + drain 契约。

### 新增测试

- **import-edge pin**（关键，仿 `cmd/naozhi/cron_no_session_import_test.go`）：新增 `internal/cron/no_platform_import_test.go`，扫描 `internal/cron/*.go` 的非测试文件，断言不再 import `github.com/naozhi/naozhi/internal/platform`。这是 G1 的守门测试，防止未来 PR 偷偷把 platform 调用加回 cron。
- **NotifySender 接口契约 test**：cron 内用一个 fake `NotifySender` 驱动 `notifyTarget` 的全部分支（lookup miss → platform-not-found WARN；MaxReplyLength→split→Reply 成功；Reply 失败 → partial bump + abort；deadline → partial bump）。现有的 fake platform 测试改造为 fake NotifySender。
- **wireup adapter test**：`internal/wireup/cron_notify_sender_test.go`，断言 `platformNotifySender.Lookup("feishu")` 返回的 `PlatformReplier.Reply` 正确委托到 `platform.ReplyWithRetry` 且透传 ctx / `limits.PlatformReplyMaxAttempts`；lookup miss 返回 `ok=false`。
- **SplitText 等价 test**（若选「cron 内置一份 SplitText」）：table-driven 对拍 cron-local split 与 `platform.SplitText` 在边界（CJK、超长、空、恰好 maxLen）上的输出一致。**若选「接口暴露 Split」则不需要**，但接口会更宽。

### 防回归手段

- `go test -race ./internal/cron/... ./internal/wireup/...`（lock-free configMaps 读 + triggerWG 并发）。
- import-edge pin + background-ctx anchor pin 双守门。
- 覆盖率：notify 路径目标维持 ≥80%（现有测试已密集覆盖该函数）。

---

## 5. Risk & rollback

### 出错会 break 什么

- **stopCtx 父链断裂**（R243-SEC-14 / #799）：如果 adapter 自己起 `context.Background()` 或吞掉传入 ctx，hung webhook 会重新把 `triggerWG.Wait` 钉在 30s stopBudget 上。**缓解**：`PlatformReplier.Reply(ctx, …)` 必须把 cron 传入的 `replyCtx` 透传给 `platform.ReplyWithRetry` 的第一个参数；`notify_stopctx_test.go` 是这条不变量的 pin。
- **partial 遥测漏报**：若把 abort/deadline 判定挪进 adapter，`CronNotifyPartialTotal` 的语义会漂移。**缓解**：方案 A 把判定留在 cron 循环里，遥测零迁移。
- **chunk 上限 / dropped accounting**：`cap-drop accounting` 测试对 `total = len(chunks)+dropped` 的精确计数敏感。**缓解**：留在 cron，不动。
- **SplitText 行为漂移**（仅当选「cron 内置一份」）：CJK 切分若与 `platform.SplitText` 不一致，用户会看到不同的分页。**缓解**：对拍测试 + 优先考虑「接口暴露 Split」以复用唯一实现。

### load-bearing 不变量与已有 pin

- **lock-free configMaps 读**（`scheduler.go:360-396`）：`notifyTarget` 无锁读 `configMaps()`。新 `NotifySender` 字段必须同样 write-once-at-NewScheduler、之后只读（或同样塞进 `cronConfigMaps` 走 atomic.Pointer）。`config_maps_atomic_test.go` 是 pin。
- **triggerWG.Add(1) before `go`**（`scheduler_notify.go:242`）：不动。
- **empty-text no-op before triggerWG.Add**（`deliverNotice` `:239`）+ **notifyTarget 层 empty guard**（`:279`）：不动。
- **stopCtx 短路在 SplitText alloc 之前**（`:269`）：保留为函数头第一个 guard。

### 回滚

纯架构重构、零 on-disk / config / 协议变更——回滚 = `git revert` 该 PR，无数据迁移、无状态残留。adapter 与接口是新增代码，删掉即恢复直接 import map 的旧路径。

---

## 6. Observability

- **无新增 metric**。`CronNotifyPartialTotal`（`metrics.go:299`）名称、语义、归属层不变。
- **无新增 log**。`notifyTarget` 的三条 WARN（platform-not-found `:284`、chunk-cap-dropped `:332`、partial-after-failure/deadline `:351`/`:376`）原样保留，字段不变。
- adapter 层（wireup）不新增日志——它是纯委托，错误经 `error` 返回值回到 cron 循环，由 cron 的既有 WARN 聚合。
- **不变的可观察形状**：cancelled-mid-flush 仍表现为 ReplyWithRetry 的 context error → partial WARN + counter bump，与现状一致（`scheduler_notify.go:287-297` 的契约）。

---

## 7. Compatibility & migration

- **向后兼容**：纯内部重构。`SchedulerConfig.Platforms` 字段可保留（adapter 在 wireup 内基于它构造 `NotifySender`），或替换为 `SchedulerConfig.NotifySender`。两种都对 cmd/naozhi 之外的调用方透明，因为唯一生产构造点是 `wireup.WireSchedulers`（`schedulers.go:191-205`）。
  - **推荐**：`SchedulerConfig` 用 `NotifySender NotifySender` 替换 `Platforms map[string]platform.Platform`，由 wireup 把 `deps.Platforms` 包成 adapter 后注入。这样 cron 的 config 表面也不再提 platform 类型（彻底达成 G1）。
- **on-disk 格式**：无变更。`cron_jobs.json` 不含 platform 句柄，只含 `NotifyPlatform`/`NotifyChatID` 字符串字段（`NotifyTarget` 的 IM 坐标），这些是名字不是类型，不受影响。
- **config flag**：N/A —— 无新 flag，无 opt-in/opt-out（行为零变更）。
- **迁移路径**：单 PR 内完成 cron 接口定义 + wireup adapter + 调用点切换；无跨版本兼容窗口需求（内部 API）。

---

## 8. Rollout plan

一次性切换（非 flag-gated、非分阶段灰度），因为是零行为变更的内部重构——分阶段灰度对「同一二进制内的纯依赖反转」没有意义。但实现本身分两步落地以控制 review 面：

### Phase 1（本轮**可独立安全落地**，hasLandablePhase1=true）

**做什么**：在 `internal/cron` 内**新增** `NotifySender` + `PlatformReplier` 接口（含 `Split` 方法以复用 `platform.SplitText`，避免 cron 内置重复实现），把 `notifyTarget`（`scheduler_notify.go`）的 `s.configMaps().platforms[plat]` + `p.MaxReplyLength()` + `platform.SplitText` + `platform.ReplyWithRetry` 四处替换为经接口调用；在 `internal/wireup` 新增 `platformNotifySender` adapter，由 `WireSchedulers` 基于 `deps.Platforms` 构造并注入 `SchedulerConfig`；`SchedulerConfig.Platforms` 替换为 `SchedulerConfig.NotifySender`，删除 cron 生产代码对 `internal/platform` 的 import。chunk 上限、`CronNotifyPartialTotal`、stopCtx 短路 + replyCtx 父链、empty-text guard、WARN 全部**原样留在 cron**。

**确切文件级改动清单**：

| 文件 | 改动 | 预估行数 |
| :--- | :--- | ---: |
| `internal/cron/notify_sender.go`（新增） | `NotifySender` + `PlatformReplier` 接口定义 + godoc | ~30 |
| `internal/cron/scheduler_notify.go` | `notifyTarget` 内 4 处调用改走接口；删 `import platform`；保留所有 anchor/guard | ~25（净改） |
| `internal/cron/scheduler.go` | `SchedulerConfig.Platforms` → `NotifySender`；`cronConfigMaps.platforms` 改持 `NotifySender`（或新增独立 write-once 字段）；删 `import platform` | ~25 |
| `internal/wireup/cron_notify_sender.go`（新增） | `platformNotifySender` adapter（Lookup + PlatformReplier.Reply 委托 `platform.ReplyWithRetry` + Split 委托 `platform.SplitText`） | ~40 |
| `internal/wireup/schedulers.go` | `NewScheduler` config 里 `Platforms:` → `NotifySender: newPlatformNotifySender(deps.Platforms)` | ~3 |
| **生产代码合计** | | **~120** |

**风险**：medium。理由：触碰 stopCtx 父链（#799 安全不变量）与 lock-free configMaps 读路径，且 `notify_background_ctx_test.go` 是源码字符串扫描——重写 `notifyTarget` 函数体时必须逐字保留其 anchor。但范围明确、有密集既有 pin 守护、零 on-disk/协议/遥测迁移、可一键 revert，符合「明确的、≤150 行、不破坏已 pin 不变量的抽取/适配」判据。

测试侧另加 ~80 行（import-edge pin + adapter test + fake NotifySender 改造既有 platform-fake 测试），不计入生产行数。

### Phase 2（**不在本轮**，需独立评审 → cron-sysession-merge Phase E）

把整段 notify 协议（chunk loop + retry 预算 + partial 遥测 + stopCtx）统一进 dispatch 的 `replyText`（#670 完整版）。涉及把 chunking/multi-error 契约推进 dispatch 层、遥测可能换归属、跨 cron/dispatch seam——属于大重构，本 RFC 不落地代码。
