# Attachment GC Daemon 设计文档

> **状态**: 已实现(v2.1 设计落地;完整 daemon 版,默认 enabled:false + dry_run 待运维启用)
> **作者**: naozhi team
> **创建**: 2026-05-31
> **修订**: 2026-05-31(v2 / v2.1 / 实现)
> **关联 issue**: #1198 `[DEADCODE-5] Attachment GC subsystem is wired in code but never invoked at runtime`
> **前置 RFC**: [attachment-refcount.md](attachment-refcount.md)(双 TTL refcount 算法)、[system-session.md](system-session.md)(Daemon 框架)

## 0.1 v2.1 变更摘要(相对 v2 — 枚举完整性复审)

v2 把枚举从"活跃 session"改为"已知 workspace 根",消除了 prune-session 缺口。但复审发现 v2 的 root 集合定义仍不完整,且引入若干新约束:

| 编号 | 问题 | v2.1 修正 |
|---|---|---|
| **E1 必修** | `KnownWorkspaceRoots = Default ∪ overrides` **漏掉 project-bound workspace**。project session 的 workspace 走 `routing.go:197/204` 的 `b.WorkspaceDir`(= `project.Path`),**不进 `workspaceOverrides`**。用户在绑定 project 的群里传 PDF,附件落 project 目录,v2 扫不到 | root 集合补 **∪ 所有 project 的 `Path`**(`project.Manager.All()` 枚举);scratch session 的 `opts.Workspace` 确认继承自上述集合(§4.4) |
| **E2** | root 字符串去重不防 symlink / `..` / 相对路径导致的同目录不同字符串 → 重复扫 + 并发 `os.Remove` 同一文件 | 枚举后 `filepath.Abs`+`EvalSymlinks` 规范化再去重;多实例场景声明"未防护,假设单实例"(§4.4/§10 F7) |
| **E3** | per-tick cap 全局 + 固定遍历顺序 → 高产的第一个 root 每轮耗尽 budget,靠后 root **饥饿永不清** | cap 改 **per-root**,并加 root 间 round-robin cursor(§4.6-2) |
| **E4** | 存量首跑 dry-run 显示几千 would-remove,但**无法区分**"真该删"与"tracker 还没 bump 的活跃引用",运维仍不敢开真删 | dry-run 报告**按原因分桶**统计(legacy无meta / 有meta无refs / refs已过期),让运维看风险构成(§6) |
| **E5 策略** | RFC 体量已从"补 cron caller"涨成中等 feature | §11 新增"最小止血版"选项说明,供快速消灭 p1 |

## 0. v2 变更摘要(相对 v1)

v1 经事实核查 / 架构 / 并发 / 运维四路 review 后,以下决策与新增点构成 v2:

| 类别 | v1 | v2 |
|---|---|---|
| **宿主** | daemon,声称"零框架改动接线" | daemon **+ 诚实列出三项前置框架增强**(§4.6):`GCWithRefs` 加 `ctx`、per-tick 删除上限、daemon startup-tick。v1 的"零算法改动"承诺**不成立**,已撤回 |
| **workspace 枚举** | `VisitSessions` 枚举活跃 session(§4.5 留 orphan 缺口) | 改为**枚举已知 workspace 根**(default + overrides,稳定不随 prune 消失)再扫 `.naozhi/attachments`(§4.4)。orphan 缺口消除,`DefaultWorkspace()` 接口侵入争议一并消失 |
| **首次上线** | OQ-1 轻描淡写 | 升为 **BLOCKER**:默认 `enabled:false` + 强制 dry-run + per-tick cap(§4.6/§5/§11) |
| **可观测/回滚** | 仅 3 个聚合计数器 | 新增 **dry-run 模式 + 逐文件删除审计日志**(§6) |
| **删除顺序竞态** | 未提 | 新增 §10「并发与一致性」:**先删 .meta 后删 payload** 防孤儿 meta(F1) |
| **引用窗口竞态** | "引用计数每秒都在更新"(误导) | §10 文档化 refcount 是 **best-effort 异步**,可 drop/延迟;加 `.meta` mtime grace skip(F2) |
| **EventLogDir 空部署** | 埋在 §3.1 删代码论证里 | 新增 §4.7「EventLogDir 空部署退化语义」:明示 refTTL 失效(M3) |
| **文字/事实** | 多处行号/措辞错误 | 全部修正(§12) |

## 1. 目标

把已实现但**从未在运行时被调用**的 attachment GC 子系统接入一个真实的周期触发器,让 `<workspace>/.naozhi/attachments/<date>/` 下的附件真正被回收,消除磁盘无限增长。

**非目标**(本次不做):
- 重写双 TTL refcount 算法 —— `GCWithRefs` / `shouldKeepAttachment` 的**判定逻辑**不改;但其**函数签名要改**(加 `ctx` + per-tick cap,见 §4.6),v1 "零算法改动"的承诺已撤回。
- event-log / JSONL 的清理 —— 那是 sysession §12 `transient-sweeper` 的另一半,本 RFC 只做 attachment 这一半,但预留同一 sweeper 家族容纳它(§9)。
- 磁盘配额 / quota 强制(`RNEW-OPS-415` 跟踪),GC 是软清理不是硬限额。
- 跨节点 / 远程 workspace 的 GC —— 只清本进程已知的本地 workspace 根。

## 2. 背景与关键事实

### 2.1 现状:代码活着,运行时是死的

`internal/attachment/store.go` 有完整的 GC 实现:

| 符号 | 位置 | 作用 | 生产 caller |
|---|---|---|---|
| `GCWithRefs(ws, uploadTTL, refTTL, now)` | `store.go:406` | refcount-aware 双 TTL 反应器(RFC §3.3) | **无** |
| `GC(ws, ttl, now)` | `store.go:320` | legacy 按 date-dir 整天删的旧反应器 | **无** |
| `shouldKeepAttachment` | `store.go:516` | 双 TTL keep 判定(仅被 `GCWithRefs:468` 调) | 间接 |
| `metaPathFor` | `store.go:578` | payload → .meta 路径(仅被 GC 路径 `:487`/`:517` 调) | 间接 |
| `DefaultRefTTL = 30d` | `store.go:380` | 第二时间界默认值 | **无** |

grep 验证(2026-05-31):

```
grep -rn 'GCWithRefs\|attachment\.GC\b\|shouldKeepAttachment' --include=*.go . | grep -v _test.go
# 仅函数定义命中,cmd/ internal/cron/ internal/server/ 下零调用
```

> ⚠️ **stale godoc**:`GCWithRefs` 的 godoc(`store.go:403-405`)自称 "Callers: the attachment-gc cron job in cmd/naozhi/main.go" —— **该 cron job 根本不存在**。这条注释既佐证"无 caller",又是 §2.2 那类误导线索,落地 PR 必须一并修正(列入 §7)。

写侧(`internal/attachment/tracker` + `internal/session/attachment_tracker.go`)**已接线并在跑** —— 每条带附件路径的 event-log entry 都会 bump `.meta` 的 `ReferencingKeyHashes` / `LastReferencedAt`。即:**引用计数在持续更新(但 best-effort 异步,见 §10),却没有任何东西消费它来删文件**。

### 2.2 误导线索(顺手清理)

- `cmd/naozhi/main_helpers.go:156` 仍打印 `hint: "prune attachments/events; see docs/ops/disk-budget.md"` —— 提示运维去手动清,而本该自动清的代码是死的。
- `internal/metrics/metrics.go:226` 注释提到 "fall back to upload-only TTL GC"。**注意**:这句描述的是 tracker 写不动 .meta 时的退化语义,本身不是错误注释;§6 改写它要保住原意。
- `internal/session/router_cleanup.go:130` 注释 "A failure only delays attachment GC by a generation" —— 说明 session Remove 路径**已经**在为一个本该存在的 GC 做 refcount 清理(移除 keyhash),但下游 GC 缺席。该描述准确,不动。

### 2.3 前置 RFC 怎么说

`attachment-refcount.md` 头部状态已诚实标注:

> v1 MVP 已落地(Phase 6E-1 ~ 6E-4 + 集成测试完成,**GC cron caller 待运维启用**)

并在 §6E-5 记录 GCWithRefs 无生产 caller、等运维补 cron。本 RFC 兑现这笔欠账 —— 但**把宿主从 cron 改成 sysession daemon**(理由见 §3.2)。

## 3. 决策:三选一 + 宿主选择

### 3.1 A/B/C(issue 给的三个方向)

issue #1198 给了三个方向。本 RFC 选 **(C) 混合**:

| 方向 | 内容 | 取舍 |
|---|---|---|
| A 全接线 | 接 `GCWithRefs`,保留 legacy `GC` | ❌ 留着 legacy `GC` 是第二条死路径 |
| B 全删 | 删整个 GC 路径,接受无限增长 | ❌ p1-correctness,且浪费已完成的 refcount 写侧投资 |
| **C 混合** ✅ | **删 legacy `GC`,接线 `GCWithRefs`,清理误导提示** | legacy `GC` 的卖点在 persist 层常驻后已不成立 |

`GC`(legacy)删除论证:`GCWithRefs` 对**无 `.meta` 的文件**已 fallback 到 **date-dir 目录名解析时间**(`time.Parse("2006-01-02", dirname)`,**注意不是文件 mtime**)做 upload-TTL 判定(`shouldKeepAttachment` 的 `meta==nil` 分支,`store.go:526-529`),行为与 legacy `GC` 等价。删粒度上 legacy `GC` 是整目录删、`GCWithRefs` 是逐文件删 + 空目录回收,后者更细且覆盖前者 —— 真超集成立。

> ⚠️ 删 `GC` 前:`store_test.go` 的 `TestGC_DropsOldDatesKeepsRecent`(:141)/`TestGC_NoAttachmentDirIsNotAnError`(:178)/`TestGC_IgnoresNonDateDirectories`(:189)改测 `GCWithRefs` 的 legacy-fallback 分支(保住覆盖),而非直接删除。

### 3.2 宿主:sysession Daemon,不是 cron(诚实版对比)

issue 建议宿主用 cron。本 RFC 仍选 **`internal/sysession` Daemon 框架**,但 v2 修正了 v1 对 cron 的不公平贬低,并诚实承认 daemon 需要前置框架增强(§4.6)。

| 维度 | cron Scheduler | sysession Daemon | 裁决 |
|---|---|---|---|
| 语义 | 用户可见、可编辑、IM-out 通知的业务任务 | 进程内置、不可运行时注册的系统后台线程 | GC 属系统线程,daemon 胜 |
| 用户列表可见性 | 所有 job 用户可见可编辑可删,**无 hidden/system-job 机制** | daemon 不进用户 cron 列表 | daemon 胜(关键) |
| 工作形态 | 核心是"派生 session 跑 prompt" | "ticker 驱动纯 side-effect worker" | GC 不跑 prompt,daemon 形态更贴 |
| **手动 trigger** | `TriggerNow`(`scheduler_jobs.go:1397`)**今天就有** | `DaemonTriggerManual` 仅保留常量,Manager **无 trigger API** | **cron 胜**(诚实承认) |
| **run-history 持久化** | 完整 `runStore`(落盘+ring) | DaemonRun 仅内存 ring,重启即丢 | **cron 胜**(诚实承认) |
| **到点必跑** | cron 表达式到点必跑 | jitter+ticker,**无 startup-once**,重启重置(见 §4.6) | **cron 胜**(需框架增强补) |
| 重量级依赖 | 9.7K LOC 单 struct 单锁(#1368) | daemon goroutine 独立 | daemon 胜 |
| 长期路线 | —— | §12 已预告 `transient-sweeper`/`OrphanCleaner` 家族 | daemon 胜 |

**裁决**:cron 在 trigger/history/到点必跑三项上确实更强,但这三项都可由 daemon 框架增强补齐(§4.6),而 cron 的"无 hidden-job 机制 + prompt-job 形态 + 碰 #1368 巨锁"是结构性的、补不掉。**选 daemon,但前置三项框架增强是落地的硬依赖,不是可选项**。

## 4. 设计

### 4.1 daemon 名与注册

- daemon name: **`attachment-gc`**(13 字符,匹配 `^[a-z][a-z0-9-]{1,30}$`)。
- 注册:`internal/sysession/registry.go` 的 `builtinDaemons` 追加一项:

```go
var builtinDaemons = []builtinDaemonFactory{
    {Name: "auto-titler",   Build: func(deps DaemonDeps) (Daemon, error) { return newAutoTitler(deps) }},
    {Name: "attachment-gc", Build: func(deps DaemonDeps) (Daemon, error) { return newAttachmentGC(deps) }},
}
```

### 4.2 daemon 结构

```go
// internal/sysession/attachment_gc.go
type attachmentGC struct {
    roots      WorkspaceRootLister // §4.4:枚举已知 workspace 根(含 project)
    uploadTTL  time.Duration       // 默认 7d
    refTTL     time.Duration       // 默认 30d (attachment.DefaultRefTTL)
    perRootCap int                 // §4.6-2:每个 root 单 Tick 删除上限,0=不限(仅测试)
    cursor     int                 // §4.6-2:round-robin 起点,防 root 饥饿(仅内存)
    metaGrace  time.Duration       // §10 F2:跳过 .meta mtime 在此窗口内的文件
    dryRun     bool                // §6:只 log would-remove,不真删
    nowFn      func() time.Time    // 测试注入;nil → time.Now
}

func (a *attachmentGC) Name() string        { return "attachment-gc" }
func (a *attachmentGC) Description() string { return "回收超过 TTL 且无引用的附件文件" }
```

实现 `Daemon` + `Configurable`,**不实现** Runner 相关(对照 auto-titler 需 `deps.Runner`;本 daemon `Build` 不校验 Runner)。

### 4.3 Tick 算法

```
Tick(ctx):
  roots = a.roots.KnownWorkspaceRoots()   // §4.4,已规范化+去重(含 project)
  examined, acted = 0, 0
  start = a.cursor % len(roots)            // §4.6-2 round-robin,防 root 饥饿
  for i in 0..len(roots):
      root = roots[(start + i) % len(roots)]
      if ctx.Err() != nil: break          // workspace 边界响应 shutdown
      if root == "" or !filepath.IsAbs(root): continue
      removed, err := attachment.GCWithRefs(ctx, root, opts{
          uploadTTL, refTTL, now,
          maxRemove: a.perRootCap,         // ★per-root budget,非全局
          metaGrace, dryRun,
      })                                    // §4.6:GCWithRefs 加 ctx + maxRemove
      examined++
      a.cursor++                            // 推进 cursor,下一 Tick 从下个 root 起
      if err != nil and not ctx.Canceled:
          log.Warn; metrics.AttachmentGCErrorTotal.Add(1); continue
      acted += removed
      reap_or_wouldreap.Add(removed)        // dry-run→WouldReap,否则 Reaped
  metrics.AttachmentGCSweepTotal.Add(1)
  return TickReport{Examined: examined, Acted: acted}, firstErr
```

- `Examined` = 扫过的 workspace 根数,`Acted` = 删除文件总数。
- `GCWithRefs` 内部现在也检查 `ctx`(§4.6),所以单 workspace 的大目录 walk 可被 shutdown/超时中断 —— 闭合 v1 的 force-exit 风险(§10 F4)。
- **idempotent**:同 now 跑两次,第二次删 0。

### 4.4 workspace 根枚举(v2 核心改动:消除 orphan 缺口)

**v1 缺陷**:用 `VisitSessions` 枚举活跃 session 的 workspace。但 `VisitSessions` 只遍历内存 `r.sessions`,**被 prune 的 session、重启后未回填的 dead session 都不在其中** → 它们的 workspace 永不 GC,而这些恰是 refcount 已归零、最该清的目录。`maxWorkspaceOverrides=1024`(`router_workspace.go:9`)也证明 workspace 可能上百,"很少"的假设不成立。

**v2 方案**:枚举的不是 session,而是 **workspace 根集合**。关键洞察:**消失的是 session,不是 workspace 路径** —— 这个集合**稳定、不随 session prune 收缩**。

> ⚠️ **澄清**:附件**不**落在 dataDir 下,而是落在 `<workspace>/.naozhi/attachments/`(`store.go:42`),workspace 是任意配置的 cwd。因此"扫 dataDir"在本项目里精确化为"**枚举已知 workspace 根,对每个根扫 `<root>/.naozhi/attachments`**"。

**E1 — root 集合的完整定义(v2.1 修正)**:复审发现附件 workspace 有**三个**来源,v2 只列了前两个:

```
KnownWorkspaceRoots =
    {DefaultWorkspace}              // config session.cwd / r.workspace
  ∪ {workspaceOverrides 的 value}   // SetWorkspace per-chat override
  ∪ {所有 project 的 Path}          // ★v2 漏掉:project-bound session 走
                                    //  routing.go:197/204 的 b.WorkspaceDir
                                    //  (= project.Path,datasource.go:47),
                                    //  不进 workspaceOverrides
```

证据链:附件 workspace 由 `send_persist.go:91/121` 的 session workspace 决定;project-bound session 的 workspace 在 `routing.go:197/204` 被设为 `b.WorkspaceDir`,源头是 `project/datasource.go:47` 的 `p.Path`;`GetWorkspace`(`router_workspace.go:45`)的解析链只有 `overrides → r.workspace`,**不含 project 路径**。因此漏掉 project workspace = 用户在绑定 project 的群/私聊里上传的所有附件都不被 GC。

> scratch session(追问抽屉)的 `opts.Workspace`(`scratch.go:280`)继承自源 session 的 workspace,而源 session 必属上述三类之一,故被覆盖 —— 无需单列,但测试要验证。

**接口设计(E1 + 不侵入 SystemSessionRouter)**:`SystemSessionRouter` 刻意保持最小(`router.go:27-43`),不加方法。改为给 daemon 注入两个最小 lister,在 `cmd/naozhi/main.go` 构造时拼装(main.go 已同时持有 `router` 和 `projectMgr`,见 `main.go:296`):

```go
// internal/sysession/attachment_gc.go
type WorkspaceRootLister interface {
    // KnownWorkspaceRoots 返回去重 + 规范化后的所有 workspace 根。
    KnownWorkspaceRoots() []string
}

// main.go 注入的实现拼装三个源:
//   - router.DefaultWorkspace()              (router_core.go:1540,已存在)
//   - router.WorkspaceOverrideValues()       (新增:返回 overrides 的 value 去重)
//   - projectMgr.All() → 每个 .Path          (manager.go:166,已存在)
// 再统一 §E2 规范化去重。
```

`project.Manager.All()` 已存在并返回所有 project 快照(含 `.Path`);只需 router 侧补一个 `WorkspaceOverrideValues()` 导出器(读 `r.workspaceOverrides` value 去重)。**不进** `SystemSessionRouter` 接口 —— v1 的 `DefaultWorkspace()` 接口侵入争议(原 OQ-4)随之消失。

**E2 — 路径规范化去重(v2.1)**:`KnownWorkspaceRoots` 返回前对每个 root 做 `filepath.Abs` + `filepath.EvalSymlinks`,再按规范化字符串去重。否则 `~/ws`、`/home/u/ws`、`/home/u/ws/`、symlink 指向同一目录的不同字符串**不会被去重** → 同一附件被多次扫、并发 `os.Remove`(虽幂等但浪费且产 ENOENT 噪声日志)。规范化失败(目录不存在)的 root 跳过 + log,不阻断其余。

**残余缺口(诚实记录)**:若运维曾配置某 override / 创建某 project、后来移除了绑定/删了 project,但其旧附件目录仍在磁盘 —— 这类"已解绑 workspace"不再出现在三个源里,仍不被 GC。真实但极窄(需主动改配置/删 project 才触发),记入 §11 OQ-2,Phase 2 由 `OrphanCleaner`(§9)扫已知 root 的**父目录**兜底。比 v1 的"所有 prune session 都漏"窄了一个数量级。

### 4.5 (已合并入 §4.4,v1 此节的 orphan 缺口讨论已被 §4.4 方案消解)

### 4.6 前置框架增强(daemon 落地的硬依赖)

v1 假装"零框架改动接线",经 review 证实不成立。落地 attachment-gc **必须**先做以下三项:

**(1) `GCWithRefs` 加 `ctx` + `maxRemove`(attachment 包改动)**
现状:`GCWithRefs` 是不可中断的全目录 walk,`ctx` 不可见。大目录(数万文件)下:① 违反 Daemon "≤几百ms 返回" 契约(`daemon.go:53`);② 跑满共享的 30s `TickTimeout`(`manager.go:652`)被静默截断;③ **二阶效应**:walk 阻塞 `wg.Wait` → `Stop` 的 `stopCtx` 超时 → `OnHardFail` 触发 **`os.Exit(2)`**(`manager.go:484`),一次大目录 GC 能把进程在 shutdown 时打成 force-exit。
改法:`GCWithRefs(ctx, ws, opts)`,在 day-dir 循环和 file 循环里检查 `ctx.Err()`,可中断返回 partial removed。`maxRemove` 触顶即停。**这破坏 v1 "零算法改动" 承诺,是必要变更**。

**(2) per-root 删除上限 + round-robin(B1 首跑 IO 风暴防护 + E3 防饥饿)**
存量部署首跑可能一轮删数千文件 → IO 风暴 + 半清理状态。`maxRemove` 上限让大存量靠多轮(6h tick × N 轮)摊销,单 Tick 快速返回。
**E3 修正**:上限必须是 **per-root**(每个 workspace 根独立 budget,默认 500),**不能**是 per-tick 全局 —— 否则固定遍历顺序下,持续高产的第一个 root 每轮耗尽全局 budget,靠后的 root **饥饿、永不被清**。再加一个 **round-robin cursor**(在 daemon 内存留 last-visited root index,每 Tick 从上次停的下一个 root 起):即使某 root 每轮触顶,下一 Tick 也从别的 root 开始,保证所有 root 公平轮到。cursor 仅内存(重启从头开始可接受,GC 幂等)。

**(3) daemon startup-tick(到点必跑 / 重启不重置)**
现状:`runDaemonLoop`(`manager.go:577`)是 `jitter[0,tick) → NewTicker`,**无立即首跑**。tick=6h 则首次 GC 在启动后 6~12h;频繁重启的部署每次重启重置 jitter,**可能永远跑不到一次 GC**。
改法二选一(OQ-1):
- (a) 给 `DaemonRuntimeConfig` 加 `RunOnStart bool`,Manager 在 jitter 前先跑一次 `runOnce`。框架级,惠及未来所有 sweeper。**推荐**。
- (b) attachment-gc 不用 daemon tick 的首跑,而在 `buildSysessionManager` 启动时像 `sweepOldJSONL` 那样调一次 startup GC + 之后交给 tick。更局部,但首跑逻辑与周期逻辑两处维护。

### 4.7 EventLogDir 空部署的退化语义(M3)

`attachment_tracker.go:85`:`EventLogDir==""` 时 tracker **不启动**,`.meta` 永不写 `ReferencingKeyHashes`/`LastReferencedAt`。此时 `GCWithRefs` 对所有文件退化为**纯 upload-TTL(7d)**,**refTTL 完全不生效**。

**对该类部署的影响**:用户 8 天前上传、今天仍想查看的图,**会被删**(因为没有引用数据保护它)。这是从"只增不删"到"7 天后必删"的行为变化。

**建议**:此类部署应把 `upload_ttl` 调大(如 90d),或保持 `enabled:false`。RFC 在 config 注释和 disk-budget runbook 中明示这一点。

## 5. 配置

复用 `sysession.daemons.<name>` 通路。`config.go` 的 `SysessionDaemonConfig`(`config.go:322`,现共享 struct)加 attachment-gc 字段,并同步修订该 struct 上 "AutoTitler is the only daemon for now" 的过时注释(`config.go:326`):

```go
type SysessionDaemonConfig struct {
    Enabled bool   `yaml:"enabled,omitempty"`
    Tick    string `yaml:"tick,omitempty"`
    // auto-titler 字段……(略)
    // attachment-gc 字段:
    UploadTTL  string `yaml:"upload_ttl,omitempty"`  // 默认 168h (7d);0/负值=用默认(非禁用)
    RefTTL     string `yaml:"ref_ttl,omitempty"`     // 默认 720h (30d)
    PerRootCap int    `yaml:"per_root_cap,omitempty"`// §4.6-2:每 root 单 Tick 删除上限,默认 500;0=不限(仅测试)
    DryRun     bool   `yaml:"dry_run,omitempty"`     // 默认 false
}
```

`cmd/naozhi/main_helpers.go:296` 翻译循环加解码(对照现有 `min_rename_interval` parse 模式),daemon `Configure` 校验:

```go
func (a *attachmentGC) Configure(cfg DaemonConfig) error {
    if v, ok := cfg["upload_ttl"].(time.Duration); ok && v > 0 { a.uploadTTL = v }
    if v, ok := cfg["ref_ttl"].(time.Duration);   ok && v > 0 { a.refTTL    = v }
    if v, ok := cfg["per_root_cap"].(int);        ok && v > 0 { a.perRootCap = v }
    if v, ok := cfg["dry_run"].(bool);            ok          { a.dryRun     = v }
    if a.refTTL < a.uploadTTL {
        return fmt.Errorf("attachment-gc: ref_ttl(%s) < upload_ttl(%s) 无意义", a.refTTL, a.uploadTTL)
    }
    return nil
}
```

⚠️ **配置陷阱明示**(写入 config 注释):
- `upload_ttl: "0"` → `v>0` 守卫使其回落默认 7d,**不是**"立即删"或"禁用"(与 Runner 的 `jsonl_max_age:"0"=禁用` 语义相反,易混淆)。
- `tick` 设下限:对大目录,过短 tick(如 30s)每轮全量 walk → 持续 IO。建议 `Configure` clamp tick floor(如 ≥1h),防误配(对照 auto-titler clamp batch)。

`config.yaml` 示例段(接在 `auto-titler:` 之后):

```yaml
  daemons:
    auto-titler:
      enabled: true
      tick: 30s
      # …
    attachment-gc:
      enabled: false     # 首版默认关:存量部署须先 dry-run 评估(§11 OQ-3)
      tick: 6h
      upload_ttl: 168h   # 7d:超过且无引用即删
      ref_ttl: 720h      # 30d:被引用文件最后引用后再留 30d
      per_root_cap: 500  # 每 root 单 Tick 删除上限,大存量多轮摊销 + round-robin 防饥饿
      dry_run: true      # 首次上线建议 true,观察 would-remove 日志再转 false
```

> ⚠️ `config.example.yaml` 当前**没有 sysession 段**(grep 已确认)。落地要补整个 sysession 示例段,工作量比 v1 暗示的大(§7)。

## 6. 可观测性与 dry-run(破坏性操作的安全网)

GC 是不可逆删除,聚合计数器不足以定位"删错了什么"。v2 强制:

**dry-run 模式**(`dry_run: true`):走完整 keep/delete 判定,只 `slog.Info` 记 `would-remove path/reason/uploaded_at/last_ref`,**不真删**。计入独立计数器。这是存量部署安全上线的前提(§11 OQ-3)。

**E4 — dry-run 按原因分桶(否则报告无法指导决策)**:存量部署首次带 tracker 启动时,历史附件的 `.meta` 还没有 refcount 数据(tracker 不补课)。dry-run 会显示大量 would-remove,但其中混了"真该删的死文件"和"tracker 还没来得及 bump 的活跃引用",**一个总数无法区分**,运维仍不敢开真删。因此 dry-run 报告必须**按删除原因分桶**统计:

| 桶 | 含义 | 运维解读 |
|---|---|---|
| `legacy_no_meta` | 无 `.meta`,走目录名 upload-TTL 判定 | refcount RFC 落地前的老文件,删除较安全 |
| `meta_no_refs` | 有 `.meta` 但 `ReferencingKeyHashes` 空 | 可能是真无引用,**也可能是 tracker 尚未 bump** —— 高风险桶,需结合 tracker 运行时长判断 |
| `refs_expired` | 有引用但 `LastReferencedAt` 超 refTTL | 确实长期未引用,删除安全 |

分桶计数进 `naozhi_attachment_gc_would_reap_total{reason=...}`(或三个独立 expvar)。运维看到 `meta_no_refs` 占比高时,应延长观察期让 tracker 充分 bump 后再开真删。

**逐文件删除审计**:真删时对每个被删文件 `slog.Info` 记 path + 决策原因(`uploadOld` / 无 refs / refExpired),作为事后审计唯一线索。大目录可采样/限流,但不能完全静默(现状 `GCWithRefs` 只 log 失败项,成功删除静默 —— 要改)。

**指标**(`internal/metrics/metrics.go`,对照现有 `AttachmentRef*` 命名,均 `expvar.NewInt` + `_total`):

```go
AttachmentGCReapedTotal    = expvar.NewInt("naozhi_attachment_gc_reaped_total")     // 真删文件数
AttachmentGCWouldReapTotal = expvar.NewInt("naozhi_attachment_gc_would_reap_total") // dry-run 拟删数
AttachmentGCSweepTotal     = expvar.NewInt("naozhi_attachment_gc_sweep_total")      // Tick 次数
AttachmentGCErrorTotal     = expvar.NewInt("naozhi_attachment_gc_error_total")      // workspace 级 error 数
```

> ⚠️ `AttachmentGCErrorTotal` 计的是 **workspace 级**错误(`GCWithRefs` 返回 err,即 ReadDir 失败);**文件级** remove 失败当前只 log 不返回 err,不计入此计数器 —— §6 注释要写清,别让运维以为它覆盖文件级。

**回滚**:关 daemon(或 `enabled:false`)即止血,但已删文件不可恢复 —— 这正是 dry-run 先行的理由。

`metrics.go:226` 注释按 §2.2 提示谨慎改写(保住"tracker 写不动 .meta 时退化"原意)。daemon `TickReport` 自动进 `/api/system/daemons`,运维可在 dashboard System 抽屉看 examined/acted。

## 7. 删除与清理清单(C 方案的"删/改"半)

| 动作 | 文件:符号 |
|---|---|
| 删 legacy `GC` 函数 | `internal/attachment/store.go:320 func GC` |
| **修正 stale godoc** | `store.go:403-405` `GCWithRefs` godoc 自称的不存在 cron caller |
| 改造 legacy GC 测试 | `store_test.go:141/178/189 TestGC_*` → 改测 `GCWithRefs` legacy-fallback |
| 评估 `DefaultRefTTL` 去留 | daemon 默认引用它 → 保留 |
| 清理误导提示 | `main_helpers.go:156` `prune attachments` hint → 改"GC 自动运行,调 TTL 见 config"或删 |
| 谨慎改注释 | `metrics.go:226`(保原意);`router_cleanup.go:130` 准确,不动 |
| **修正文档路径错误** | `docs/ops/disk-budget.md:15,28` 把附件路径写成 `~/.naozhi/attachments`,实际是 `<workspace>/.naozhi/attachments` |
| **改写 runbook** | `disk-budget.md` §23-36 手动清理 runbook:改为"GC 自动跑,手动仅用于已解绑 workspace" |
| **补 sysession 示例段** | `config.example.yaml` 当前无 sysession 段,需补全 |

## 8. 测试计划(TDD,目标 80%+)

### 8.1 daemon 层(新增 `attachment_gc_test.go`)

- `Tick` 空 root 集 → `TickReport{}`,无 panic。
- `Tick` 单 root 有过期文件 → `Acted == 删除数`,计数器递增。
- `Tick` 多 root 去重正确。
- **per-tick cap**:文件数 > cap → 单 Tick 删 cap 个,`budget<=0` break;下一 Tick 删剩余(摊销验证)。
- **dry-run**:`dry_run:true` → 文件不删,`WouldReapTotal` 递增,删除审计 log 出现。
- `ctx.Done()` 在 root 边界 → 提前返回,已处理计入 report。
- `ctx.Done()` 在**单 root walk 内部**(§4.6 改造后)→ `GCWithRefs` 中断返回 partial。
- 单 root `GCWithRefs` 报错 → 不中断整轮,`ErrorTotal` 递增。
- `Configure`:`ref_ttl < upload_ttl` 返回 error(daemon 禁用而非崩 Manager);`upload_ttl:"0"` → 回落默认;tick < floor → clamp。
- `nowFn` 注入确定性 keep/delete;**idempotency**:同 now 连跑两次,第二次 `Acted==0`。
- `KnownWorkspaceRoots` 假实现:default + overrides 去重正确,prune 后 root 仍在。

### 8.2 算法层(`GCWithRefs` 改造后必须新增/补强,不能假设既有覆盖)

既有 `refcount_test.go`(420 行)覆盖双 TTL 判定,但 §4.6 改了签名(ctx + maxRemove),需补:
- **ctx 中断**:walk 中途 cancel → 返回 partial removed,无 panic。
- **maxRemove 触顶**:超过上限即停,返回值 == cap。
- **符号链接攻击**(安全关键,§GCWithRefs `store.go:432` 用 Lstat 拒绝):date-dir 是指向 `/etc` 的 symlink → 不遍历、不删。**必须显式测**。
- **损坏 .meta**:`loadMetaFile` 返 err → `shouldKeepAttachment` 返 err → `kept++` 保留(err-on-keep)。测"损坏 meta 不误删"。
- **权限拒绝中途**:`os.Remove` EACCES → log 跳过、继续,不中断整轮。
- **UTC 跨天边界**:00:00 UTC 前后 keep/delete 决策稳定。
- **空 day-dir prune**:老空目录被 prune、较新空目录保留。
- **删除顺序(§10 F1)**:先删 .meta 后删 payload,验证竞态下不留孤儿 meta。
- **.meta mtime grace(§10 F2)**:mtime 在 grace 窗口内的文件本轮跳过。

### 8.3 集成

- `main_helpers.go` 配置解码:坏值 → log warn + 默认,不崩。
- daemon 经 `sysession.NewManager` 真实注册 → 假 ticker 驱动一次 Tick → 文件被删(对照 auto_titler manager 集成测试)。
- startup-tick(§4.6-3a)若实现:验证启动即跑一次。
- `go test -race ./internal/sysession/... ./internal/attachment/...` 通过。

## 9. 与 sysession §12 路线图的关系

§12 预告了 `transient-sweeper`(清 JSONL,startup-once `sweepOldJSONL` 的 pull-up)和 `OrphanCleaner`(清孤儿 attachment)。**诚实定位**:
- 本 RFC 的 `attachment-gc` 是 sweeper **家族**成员,但**不是** §12 那条 `sweepOldJSONL` pull-up 路线的延续(attachment GC 从无 startup-once 版本)—— 是另起的 daemon。
- §4.4 残余缺口(已解绑 workspace)正对应 §12 的 `OrphanCleaner` 职责;v2 把主缺口(prune session)已消除,只把最窄的残余留给未来 OrphanCleaner,而非 v1 那样把核心职责整个推迟。
- §4.6 的 startup-tick / per-daemon 增强惠及未来所有 sweeper —— 是对 §12 框架的正向投资。

## 10. 并发与一致性(v2 新增)

GC daemon goroutine 与 tracker 单写 goroutine 对同一 `.meta` **无共享锁**(`UpdateMetaFile` godoc 的"caller owns serialization"只对 tracker 单写成立,GC 进来后前提被打破)。已知约束与对策:

**F1 — 孤儿 meta【低-中,必修】**:GC 删 payload 后、删 .meta 前被调度走,tracker 同时 `UpdateMetaFile` 重写同一 .meta → payload 没了但 .meta 复活,下轮因 `.meta` 后缀被 skip → 永久孤儿(磁盘泄漏)。
**对策**:**删除顺序反转 —— 先删 `.meta` 再删 payload**。竞态窗口里 tracker 的 `loadMetaFile` 返回 `(nil,nil)` → `UpdateMetaFile` 走 `m==nil` 分支返回 error 不重建(`store.go:607-611` 已有防御)。残留最多是 payload(下轮按 legacy-fallback 删),不会是不可回收的孤儿 meta。一行级改动,进 PR-1。

**F2 — 引用窗口误删【低,需文档化 + grace 兜底】**:老附件(>7d)被新会话首次引用,event-log 已写但 bump 还在 channel(满时**直接 drop 不重试**,`tracker.go:213`)或 1s coalesce 窗口内未落盘,GC 此刻读到旧 `LastReferencedAt` → 误删活跃引用的老图。7d uploadTTL **兜不住**(附件已过 uploadTTL)。
**对策**:① 文档化 refcount 是 **best-effort 异步**(撤回 v1 "每秒都在更新" 的强一致误导);② GC 跳过 **.meta mtime 在 `metaGrace`(默认数分钟,≥coalesce 窗口)内**的文件 —— 刚被 tracker 改过 meta 的不在本轮删,闭合"bump 刚落盘但 GC 用了删除前快照"窗口;③ 依赖 refTTL(30d)≫ uploadTTL(7d)作软兜底:只要附件近 30d 内成功 bump 过一次即安全,危险仅限"老附件+首次引用+该次 bump 丢失"三重巧合。

**F3 — Persist vs 空目录 prune【极低,加注释固化】**:Persist 写当天 dateDir,GC 只 prune `≥uploadTTL` 老目录,两者**不相交**。在 `GCWithRefs` prune 处加注释固化此不变量,防未来有人放宽 prune 条件到当天。

**F4 — 不可中断 walk → force-exit【中,§4.6 已解】**:见 §4.6-1,`GCWithRefs` 加 ctx 后闭合。

**F5 — 长 Tick 被 CAS gate skip【低,自愈】**:`runOnce` 的 `inflight.CompareAndSwap`(`manager.go:622`)对重叠 tick 直接 skip 不排队(设计意图)。GC 幂等,错过一周期无害。与 F4 叠加的 force-exit 风险已由 §4.6 ctx 改造消除。

**F6 — root 集合快照陈旧【信息】**:`KnownWorkspaceRoots` 在 RLock 下只读 default + overrides + project 列表;walk 在锁释放后跑。陈旧只导致某 root 这轮漏/多扫,下轮自愈,无正确性问题。

**F7 — 多实例 / 同目录不同字符串并发删除【低,E2 部分缓解】**:附件按 workspace 绑定而非 session(`store.go:60-62`),同一目录可能:① 被多个 root 字符串命中(symlink/`..`/相对路径)→ E2 的 `EvalSymlinks`+`Abs` 规范化去重已缓解;② 被两个 naozhi 进程同时 GC → **未防护**。后者下并发 `os.Remove` 同一文件是幂等的(第二个拿 ENOENT,已 log-skip 容忍),但 dry-run 计数会重复、空目录 prune 可能 race。**v2.1 立场:假设单实例部署**(naozhi 典型形态),多实例共享 workspace 不在支持范围,在 RFC 与 daemon godoc 显式声明,不静默。

## 11. 落地步骤(建议 PR 切分)

1. **PR-1a(框架增强,§4.6)**:`GCWithRefs` 加 `ctx`+`maxRemove`+`dryRun`+`metaGrace`,改删除顺序(F1);改造 `refcount_test.go`/`store_test.go`;daemon startup-tick(若选 4.6-3a)。**纯 attachment+sysession 包内,不接 daemon,可独立 review/合并**。
2. **PR-1b(接线)**:新增 `attachment_gc.go`+测试;`registry.go` 注册;`WorkspaceRootLister`(default + `router.WorkspaceOverrideValues()` + `projectMgr.All().Path`,§4.4 E1)+ 规范化去重(E2)+main.go 注入;`config.go`/`main_helpers.go` 解码;`metrics.go` 计数器(含 §6 分桶);删 legacy `GC`+stale godoc;`config.yaml`/`config.example.yaml` 示例段;清 `main_helpers.go:156` hint;修 `disk-budget.md` 路径+runbook。
3. **验收**:`go build` + `go test -race ./...` 绿;本地起服务,放 8 天前假附件目录(default / override / **project** 三类 workspace 各一,验 E1),`dry_run:true` 驱动一次 Tick,确认分桶 would-remove 日志 + `would_reap_total` 递增;转 `dry_run:false` 确认真删 + per-root cap + round-robin 生效。
4. 关 issue #1198;`attachment-refcount.md` 头部状态改为"GC 由 attachment-gc daemon 自动运行(本 RFC)"。

### 11.1 最小止血版(E5 — 若需快速消灭 p1)

v2.1 完整版已是中等 feature(改签名 + 新接口 + 框架增强 + dry-run + project 枚举 + round-robin)。issue #1198 的本质只是"磁盘泄漏",可先发一个**最小止血版**止血,完整 daemon 留作后续迭代:

- **范围**:只做 PR-1a 的 `ctx` + **先删 .meta(F1)**,然后在 `buildSysessionManager` 启动时(像 `sweepOldJSONL` 那样)对**已知 root(含 project)各调一次 `GCWithRefs`**,`dry_run` 默认开。**不做** daemon tick / round-robin / per-root cap / 分桶。
- **优点**:几十行,启动即止血,风险低;保留 §4.4 的完整 root 枚举(E1 不能省 —— 否则止不住 project 目录的血)。
- **缺点**:无运行中周期清理(长寿进程要等下次重启);无 cap 防首跑 IO 风暴(靠 dry-run 默认开 + 运维手动转真删兜底)。
- **取舍**:若 p1 紧急,先上最小版止血;否则直接做完整版(只多 1~2 个 PR,长期形态正确)。记入 OQ。

## 12. 文字/事实修正(已并入正文,留档)

- §4.4 `SessionSnapshot.Workspace` 在 `managed.go:445`(v1 误作 :412,那是 struct 声明行)—— v2 已不依赖该字段(改用 `KnownWorkspaceRoots`)。
- §3.1 "date-dir mtime" → 实际是**目录名解析时间**(`time.Parse`),已改。
- `GCWithRefs` godoc stale cron caller → 列入 §7 修正。
- `disk-budget.md` 路径错误 → 列入 §7。
- `config.example.yaml` 无 sysession 段 → 列入 §7。
- **v2.1 枚举完整性**:v2 的 `KnownWorkspaceRoots = Default ∪ overrides` 漏 project workspace(E1);root 字符串去重不防 symlink/同目录(E2);per-tick 全局 cap 致 root 饥饿(E3);dry-run 总数无法指导决策(E4)。全部并入 §0.1 / §4.4 / §4.6-2 / §6。

## 13. Open Questions(待评审)

- **OQ-1**:daemon startup-tick 选 §4.6-3a(框架级 `RunOnStart`,惠及未来 sweeper)还是 3b(attachment-gc 局部 startup GC)?倾向 3a。
- **OQ-2**:§4.4 残余缺口(已解绑 override / 已删 project 的旧附件)MVP 接受 / 还是 PR-1 就扫已知 root 的父目录?倾向 MVP 接受(缺口已极窄),记 Phase 2 OrphanCleaner。
- **OQ-3**:首版默认 `enabled:false`+`dry_run:true` 是否作为硬性发布门槛(运维必须先观察分桶 dry-run 日志、确认 `meta_no_refs` 桶占比可接受后再开真删)?倾向是。
- **OQ-4**:`tick` floor 设多少(1h?)、`per_root_cap` 默认 500 是否合适?需结合典型部署附件规模定。
- **OQ-5**:`metaGrace`(F2)默认值 —— coalesce 1s 之上留多少余量?数分钟够否?
- **OQ-6(E5)**:先上 §11.1 最小止血版快速消灭 p1,还是直接做完整 daemon 版?取决于 #1198 紧急程度。
