# Attachment 引用计数(大图跨 TTL 可见)

> **状态**: v1 MVP 已落地(Phase 6E-1 ~ 6E-4 + 集成测试完成,GC cron caller 待运维启用)
> **作者**: naozhi team
> **创建**: 2026-05-10
> **依赖 / 前置**: `docs/rfc/event-log-persistence.md`(父 RFC)的 Phase 1-5 已于 Round 202 落地,本 RFC 在同一 session 内接续完成。
> **关联代码**:
> - `internal/attachment/store.go`(`Meta.ReferencingKeyHashes` / `Meta.LastReferencedAt` + `AddReference` / `RemoveReference` / `HasReference` 辅助 + `GCWithRefs` 双 TTL + `UpdateMetaFile`)
> - `internal/attachment/tracker/`(新包:single-goroutine worker + coalesce + Observer + OnPersistedEntry / OnSessionRemoved / Flush / Stop)
> - `internal/session/attachment_tracker.go`(Router lifecycle + WorkspaceResolver + `attachmentMetricsObserver` → `internal/metrics`)
> - `internal/session/eventlog_bridge.go`(sink bridge 扩接 tracker.OnPersistedEntry)
> - `internal/session/eventlog_health.go`(`AttachmentTrackerStats()`)
> - `internal/session/router.go`(NewRouter 启动 + Shutdown 停止 + Remove 触发 clearAttachmentTrackerRefs)
> - `internal/server/health.go`(`/health.attachment_tracker` 子对象)
> - `internal/metrics/metrics.go`(`AttachmentRefBumpTotal` / `RefClearTotal` / `RefMetaErrorTotal` / `RefDropTotal`)

---

## 1. 目标 & 非目标

### 1.1 目标

让 dashboard 用户上传的图片(PNG/JPEG/GIF/WebP)在 **有效引用期** 内永远可点击查看大图,而不是按日历天 TTL 被 attachment GC 删掉。具体验收:

- 用户今天发图 → 明天 / 一周 / 一月后切回该 session → 点击缩略图仍能看到完整大图
- 用户手动删除该 session → session 关联的原图按 GC TTL 安全回收
- 所有活跃 session 都不再引用的图 → 按 `max(uploaded_at + maxTTL, last_referenced_at + refTTL)` 回收
- 父 RFC 的 "缩略图兑底" 降级路径仍然存在,确保本 RFC 未完成也有可用 MVP

### 1.2 非目标

1. **不做 PDF 引用计数**:PDF 是 KindFileRef(Claude 通过 Read 工具按文本读),其引用由 Claude JSONL 自然承载,tool_use 路径已记录完整文件路径;而且 PDF 量小、TTL 7 天足够。只对 `image/*` 加 refcount
2. **不做跨 session 共享去重**:两个 session 分别上传同一张图 → 产生两份磁盘文件。去重需要 SHA-256 指纹、跨 workspace 索引,工作量数倍于本 RFC 收益不成比例
3. **不做 reference migration 工具**:旧 attachment(无 meta 的 referencing 字段)按 legacy TTL 回收;运维可选手动脚本做 bulk 回填
4. **不做 cron-cleanup 型 GC 触发**:沿用现有 attachment.GC 每 24h 调用 pattern,仅算法改变

### 1.3 Must-have(GA 验收)

1. 每次 event log 持久化写入引用 `<workspace>/.naozhi/attachments/*` 的 entry 时,异步登记引用到 attachment meta
2. GC 策略从 "日历天 TTL" 改为 **`(uploaded_at + maxTTL) AND (last_referenced_at + refTTL)` 双过期**
3. session 删除 → attachment 引用撤销(不立即删文件;等下一次 GC 收割)
4. 旧 attachment meta 无新字段时按 legacy 路径走,不破坏回滚
5. 父 RFC 前端兜底(缩略图 fallback)保留 — 引用计数是增量优化而非替换

---

## 2. 背景

### 2.1 现状盘点

`internal/attachment/store.go:205` 的 `GC` 按日期目录粒度删除:

```go
cutoff := now.UTC().Add(-ttl)
for each <date>/ directory under <workspace>/.naozhi/attachments/:
    if date+24h < cutoff: os.RemoveAll(dir)
```

TTL 由调用方控制,production 是 7 天(`cmd/naozhi/main.go` 的 attachment-gc cron job)。结果:

- 用户上传图片 → 14 天后回看会话 → 点击大图 → `/api/sessions/attachment` 返 404,前端 fallback 到 600 px 缩略图
- 即使会话仍活跃、用户仍想查看大图,原图照样被 TTL 强删

### 2.2 当前 Meta schema

```go
type Meta struct {
    OrigName   string    // 原文件名
    MimeType   string    // e.g. "image/jpeg"
    Size       int64
    UploadedAt time.Time // 写入时间(UTC)
    SessionKey string    // 审计用,不参与 GC 逻辑
    Owner      string    // dashboard-auth-derived,审计用
}
```

Meta 同目录并存为 `.meta` sidecar。每个 attachment 是 `<date>/<uuid>.<ext>` + `<date>/<uuid>.meta` 一对文件。

### 2.3 event log 侧现有信息

父 RFC 的 `EventEntry.ImagePaths` 是 workspace-relative 的 `.naozhi/attachments/<date>/<uuid>.<ext>`。每个持久化的 entry 都在 `<keyhash>.log` 里留下这条路径。这是引用关系**唯一的权威来源** — Claude JSONL 不记录 ImagePaths。

### 2.4 引用关系的完整清单

对单张 attachment,引用源可能是:
1. **event log entry 的 ImagePaths**(主源,由父 RFC 持久化)
2. **active session 的 in-memory ring buffer** — 进程未重启时 ring 里仍有但 event log 已 rotate 挤走的 entry
3. **Claude JSONL 的 tool_use / user message 文本内 `<workspace>/.naozhi/attachments/...` 字面量** — 若 Claude 回复里引用了路径(极少见)

本 RFC 只处理 #1。#2 不独立建模:event log 写入比 ring eviction 快,#2 总是 #1 的真子集。#3 是边际 case,不保护。

---

## 3. 设计

### 3.1 Meta schema 扩展

```go
// internal/attachment/store.go
type Meta struct {
    // ... existing fields unchanged ...

    // ReferencingKeyHashes is the set of session key hashes whose event
    // log has persisted an entry carrying this attachment's RelPath.
    // Maintained by the router-level Tracker (§3.2). Presence alone
    // does NOT pin the file indefinitely — the new GC policy (§3.3)
    // combines this with a last-referenced-at + refTTL to decide
    // eligibility.
    //
    // Stored as a sorted []string so operators eyeballing a .meta
    // file can read off which sessions touched the attachment
    // without a secondary lookup. The set is content-addressed via
    // session.KeyHash (same as events/<keyhash>.log stems) so the
    // correspondence is obvious.
    //
    // Nil/missing field = legacy attachment predating this RFC;
    // GC applies the legacy ttl-only path in that case.
    ReferencingKeyHashes []string `json:"referencing_keyhashes,omitempty"`

    // LastReferencedAt is the most recent unix-ms timestamp at which
    // any session's event log persisted an entry referencing this
    // attachment. The GC keeps the file as long as
    //   now - LastReferencedAt < refTTL (default 30 days)
    // even if uploaded_at is well past ttl. Zero/missing = never
    // observed by the Tracker (legacy data).
    LastReferencedAt int64 `json:"last_referenced_at,omitempty"`
}
```

**演进规则**:additive 字段,omitempty。旧 naozhi 读新 .meta 会忽略这两个字段,按 legacy TTL 路径走,不会 crash。

### 3.2 引用跟踪器 `internal/attachment/tracker`

新增 package(不与 store.go 同文件 — tracker 的责任是**观察 event log 持久化**,与 store.go 的"写入/GC 文件"关注点正交)。

```go
// internal/attachment/tracker/tracker.go
package tracker

// Tracker observes EventLog persist events and updates the target
// attachment's .meta sidecar in a background goroutine. Updates are
// idempotent and coalesced per (keyhash, relpath) pair to avoid
// rewriting the same .meta dozens of times per busy session.
//
// Lifecycle:
//   - NewTracker spins up a worker goroutine.
//   - OnPersistedEntry enqueues a reference bump for every
//     (keyhash, image path) pair in the entry.
//   - OnSessionRemoved enqueues a reference removal for every
//     attachment that mentions the keyhash. The tracker walks the
//     workspace attachment directory to find them — expensive, but
//     session removal is rare.
//   - Stop() drains the queue and exits.
type Tracker struct {
    workspaceResolver func(keyhash string) string  // keyhash → absolute workspace
    clock             func() time.Time
    in                chan trackerJob
    closeCh           chan struct{}
    wg                sync.WaitGroup
    coalesce          map[coalesceKey]time.Time     // (keyhash, abspath) → last bump
    mu                sync.Mutex                    // protects coalesce
    obs               Observer                      // metrics hook
}

type Observer interface {
    OnReferenceBump(n int)    // one call per .meta update (possibly batching many paths)
    OnReferenceClear(n int)   // session removal triggered N clears
    OnMetaWriteError(path string, err error)
}
```

**Tracker 不持有 `*session.Router`**。它拿一个 `workspaceResolver` 回调,因为 `ImagePaths` 是 workspace-relative — tracker 需要从 session keyhash 查出 workspace 根目录拼成绝对路径。注入回调让 tracker 可以在不引 session 包的情况下独立 test。

#### 3.2.1 Coalesce 策略

一次用户消息可能包含多张图 → 对应多个 meta 更新。同一 session 10 秒内连发多条带图消息 → N 次 `.meta` 重写。

策略:`coalesce` map 记录 `(keyhash, abspath) → last bump time`。相同 key 在 **1 秒**内的重复请求,只更新 `LastReferencedAt`(in-memory),不立刻落盘;1 秒 debounce 后一次性写 .meta。与父 RFC Persister 的 200ms debounce 是不同用途:

- Persister debounce 保 I/O 吞吐(log + idx 本身的 fsync 聚合)
- Tracker debounce 保 .meta 文件不被高频小写操作撕烂(每次 .meta 都是完整 rewrite,不是 append)

#### 3.2.2 元数据写入语义

`.meta` 文件是完整的 JSON 对象,修改通过 **read-modify-write**:

```
1. readFile(<uuid>.meta) → Meta
2. 添加 keyhash 到 ReferencingKeyHashes(若不存在,维持排序)
3. 更新 LastReferencedAt = max(旧值, now)
4. writeFileAtomic(<uuid>.meta, newMeta)
```

并发安全通过 **tracker 单 goroutine** 实现(和 Persister 一致)。Tracker 工人 goroutine 是 .meta 的唯一 writer;读路径(attachment.GC / handleAttachment)读 .meta 不加锁(读取半写文件会解析失败 → 保守按 legacy 路径)。

WriteFileAtomic(`internal/osutil/atomicfile.go`)已有,走 tmp + rename 保证原子。

### 3.3 GC 策略

保留现有 `GC(workspace, ttl, now)` 签名,行为改为:

```go
// 伪代码
for each <date>/<uuid>.ext file:
    meta := loadMeta(<date>/<uuid>.meta)
    uploadOld := now - meta.UploadedAt >= ttl

    // 关键:有引用时第二重条件也必须满足
    refRecent := meta.LastReferencedAt > 0 &&
        now - time.UnixMilli(meta.LastReferencedAt) < refTTL
    hasRefs := len(meta.ReferencingKeyHashes) > 0

    if uploadOld && !refRecent && !hasRefs:
        // 无引用,且过 TTL → 删
        remove(uuid.ext) + remove(uuid.meta)
    else if uploadOld && !refRecent && hasRefs:
        // 有引用历史,但所有引用早已过 refTTL → 删
        // (意味着 session 仍在引用,但最近 N 天没活跃,认为失去兴趣)
        remove(...)
    else:
        // 保留
```

- `ttl`(默认 7 天)= `uploaded_at` 维度,防止"一次上传永远占磁盘"
- `refTTL`(默认 30 天)= `last_referenced_at` 维度,防止"session 3 个月没打开,仍保留当年的图"
- 两者都过期才删;任一在限内就保留

**per-file GC 代替 per-day-directory GC**。需要 walk 到文件级别,每个文件 stat + readMeta。代价:一个有 1000 张图的 workspace 一次 GC 要 2000 次 syscall。但 GC 是 cron job(每天 1 次),代价完全可接受。

日期目录作为容器还保留(现在 Persist 继续按 date 分目录写),GC 空目录时用 `os.Remove` 清理。

### 3.4 Session 删除传导

**关键点**:session Remove 时**不立即**删 attachment — 文件可能被其它 session 或 cron 引用。只是从 .meta 的 `ReferencingKeyHashes` 移除本 session 的 keyhash。

实现:`router.Remove` 之后调 `tracker.OnSessionRemoved(keyhash)`。Tracker 走 attachment 目录扫描所有 .meta,找到 `ReferencingKeyHashes` 含该 keyhash 的,移除条目;若移除后列表变空 → 下次 GC 时按"无引用 + 过 ttl → 删"的分支处理,不提前删(否则两个 session 共用一张图时会误删)。

Session 删除 → attachment 扫描开销大(O(workspace-wide 文件数))。缓解:
- Tracker 维护内存 `keyhash → []abspath` 反向索引,避免全扫描。索引在 OnPersistedEntry 时增量构建,进程重启时懒加载(首次需要扫描一次写回)
- 懒加载成本:启动时扫 attachment 目录构建索引。workspace 里 1 万个文件 ≈ 5 秒 syscall,可接受(后台 goroutine 做,不阻塞 Router.NewRouter 返回)

### 3.5 进程重启的一致性

本 RFC 在 meta 上记录 `(ReferencingKeyHashes, LastReferencedAt)`。重启后:

- Tracker 重新构建内存反向索引(扫描 attachment 目录读 meta)
- event log 里已有的 ImagePaths 不会"再次触发 OnPersistedEntry"— 因为父 RFC 的 `replayPhase=true` 让 InjectHistory 不走 sink。所以 **meta 里的 ReferencingKeyHashes 必须是单调累积** — 每个 session 对每个 attachment 只需被记录一次,后续相同引用是 no-op。
- 崩溃场景:event log 已经写进 `<keyhash>.log`,但 .meta 更新还没 fsync。重启后 meta 缺少本条引用 → 看起来 "该 attachment 引用更少" → GC 可能过早删。
  - 缓解:重启时扫描 `<keyhash>.log`,把里面每条 ImagePaths 都 one-shot 送给 Tracker(标 `silent bump`,不触发 Observer 计数)。这个扫描在 tracker 的懒加载索引重建阶段完成,顺便重算 `LastReferencedAt = max(既有, 该 session 中这条路径最新 entry 的 Time)`。
  - 性能:event log 本身就是会被读的(naozhilog.Source.LoadLatest),新增的仅仅是读路径里加一层"顺便送给 tracker"的回调。

### 3.6 并发与竞争

单 writer goroutine 简化模型。Observer 接口(与父 RFC Persister 一致)让 metrics forward 到 internal/metrics 包。

特例:多个 session 同时引用同一 attachment(群聊场景)。`ReferencingKeyHashes` 是 sorted []string,读 → 去重 insert → 排序 → 写。同一 worker goroutine 串行执行,无 race。

---

## 4. 实现计划

### Phase 6A — Meta schema 扩展 + 向后兼容(0.5-1 天)

**Done when**:
- `Meta.ReferencingKeyHashes` / `Meta.LastReferencedAt` 字段加好
- `attachment.GC` 保留现有签名,内部分支:看到 `ReferencingKeyHashes != nil || LastReferencedAt != 0` 的 meta 走新逻辑,否则走 legacy(避免首次升级瞬间删掉大量旧图)
- 所有既有 attachment 包测试绿
- 新增向后兼容测试:旧 .meta 文件无新字段能被读(unknown fields ignored)并走 legacy GC

### Phase 6B — Tracker package 骨架(2-3 天)

**Done when**:
- `internal/attachment/tracker`:Tracker struct / workerLoop / coalesce debounce / OnPersistedEntry / OnSessionRemoved / Stop
- WriteFileAtomic 写 meta(os-atomic rename)
- 单测覆盖:coalesce 1s 内多次 bump 只写一次 .meta / 移除 keyhash 后列表为空 / 并发下 -race 绿 / metadata round-trip / Observer 接口调用计数
- 崩溃测:中途 kill,重启后 tracker 懒加载索引能恢复到正确状态

### Phase 6C — Router 接入(1-2 天)

**Done when**:
- `session.Router` 创建 tracker,lifecycle 绑 Shutdown
- 新增 `observer` 用 internal/metrics 接 expvar counter(`naozhi_attachment_refbump_total` / `naozhi_attachment_refclear_total` / `naozhi_attachment_meta_error_total`)
- `session.Router.Remove` 调 `tracker.OnSessionRemoved(keyhash)`
- event log 持久化 → tracker:在 `newEventLogSink` 的 PersistSink bridge 里,对每个 EventEntry 检查 `ImagePaths`,若非空,调 `tracker.OnPersistedEntry(keyhash, workspace, imagepaths, entry.Time)`

**ordering 契约**:tracker.OnPersistedEntry 只在 `replayPhase=false` 时调用。replay 事件不更新 meta(否则一次重启会把 `LastReferencedAt` 刷到"刚好现在",与"最近用户真的点了"语义混淆)。

### Phase 6D — GC 改为 per-file 双 TTL(1 天)

**Done when**:
- `attachment.GC(workspace, ttl, refTTL, now)` 新签名;callers(cmd/naozhi cron)同步改
- 空日期目录在扫描完后 `os.Remove`
- 表驱动测试:每个"上传时间 / 最后引用时间 / 是否有 keyhash / 预期留删"矩阵的组合都覆盖
- benchmark:1000 张 attachment 的 workspace,GC 耗时 < 500ms

### Phase 6E — 崩溃恢复 + 懒加载索引(2 天)

**Done when**:
- Tracker 启动时扫 attachment 目录 + 扫 events/ 目录下所有 log 文件,重建 in-memory 反向索引
- 首次扫描发现 event log 里有 ImagePaths 但对应 .meta 无 keyhash 登记 → silent bump 补齐
- 重启前后的一致性回归测试:崩溃注入 + 恢复后 meta 字段准确
- /health 新增 `attachment_tracker` 段:`index_size` / `last_reconcile_at` / `writer_alive`(同父 RFC writer_alive 定义)

### Phase 6F — 文档 / Doctor(0.5 天)

**Done when**:
- DESIGN.md 状态目录增 `.naozhi/attachments/<date>/<uuid>.meta` schema 说明
- docs/ops/pprof.md 新增 3 个 expvar 行 + doctor 加 attachment_tracker 健康检查
- 父 RFC `event-log-persistence.md` §10 的 GA 验收条目从 "等待 attachment-refcount 子 RFC" 升级为 "已完成 — 见 attachment-refcount.md §8"
- CHANGELOG 记录:attachment GC 行为变更(日历 TTL → 双 TTL)

### 总计 **7-10 人天**

---

## 5. 代价与风险

### 5.1 磁盘 I/O

- 每条带图的 event log entry → 1 次 `.meta` read-modify-write
- Coalesce 1s debounce → 热 session(如 dashboard 连发)100 图/min 降至 ~60 .meta 写
- 每次 .meta ≈ 500 字节(WriteFileAtomic = tmp + rename + SyncDir)。typical SSD 延迟 < 2ms
- 峰值:一场会议 1000 张图 → 1000 次 .meta 写 = 2 秒 I/O,全部后台 goroutine,不阻塞 Append

### 5.2 启动成本

- Tracker 懒加载索引:workspace 有 N 张图 → N 次 readMeta
- N=1000 典型 → ~5s;N=10000 极端 → ~50s
- 用 `go` 后台跑,Router.NewRouter 不等它完成就返回;GC 会在索引就绪后才按新路径执行,索引未就绪前按 legacy TTL 路径(保守)

### 5.3 磁盘占用

- 引入 refcount 后,用户"重度用图"场景下 attachment 目录体积会比 legacy 大:原本 7 天过 TTL 就删,现在可能保留到 30 天 refTTL 过期
- 按单图 ~500 KB、每日 ~50 图、100 用户估算:
  - Legacy: 7d × 50 × 100 × 500KB = 17.5 GB 稳态
  - New:30d × 50 × 100 × 500KB = 75 GB 稳态 (极端)
- 缓解:ops 可 tune refTTL(默认 30 天可调)

### 5.4 Meta 文件损坏

- tmp rename + SyncDir 保证原子;损坏场景极罕见(磁盘扇区错误、FS bug)
- Tracker read-modify-write 读到损坏 → slog.Error + 跳过(该 attachment 保留在"legacy"路径,过 ttl 正常删)
- GC 读到损坏 → 保守走 legacy ttl-only 分支,不误删

### 5.5 升级 / 回滚

- 升级:新版本启动 → Tracker 扫索引 → 第一波 .meta 升级(附加新字段),**不阻塞用户操作**
- 回滚:旧版本不认新字段(json unknown fields ignored),看 .meta 时依 legacy GC 逻辑;已有新字段的 .meta 不破坏
- 向前兼容:本 RFC 的 Meta schema 演进沿 additive + omitempty 规则

### 5.6 与父 RFC 的耦合

- 父 RFC `replayPhase` 保证:重启回放不触发 OnPersistedEntry → Tracker 不重新 bump → LastReferencedAt 不会被 inflated
- 若父 RFC 未实装,本 RFC 无"引用源" — Tracker 空转,GC 行为退化为 legacy。**本 RFC 必须 strict after 父 RFC Phase 1-5**

---

## 6. 与第三方 API / 渠道的兼容

- 飞书 / Slack / Discord / 微信 adapter 上传的图也走 `attachment.Persist` → 自动进入新 GC 路径
- IM 平台的下载 URL 在 naozhi 内部映射到 `/api/sessions/attachment` → 同样受益
- 对 IM 平台本身 — 无改动

---

## 7. Open questions

1. **refTTL 默认 30 天是否合适?**
   - 太短 → 用户回头看月底统计报表看不到图
   - 太长 → 磁盘用量直线上升
   - 建议:先放 30 天,加 metric 监控"保留图数"/"删除图数",2 周后根据实际分布微调
2. **群聊共用图的 ref bump 是否去重?**
   - 群聊 100 人场景下,同一张图可能被 100 个 session 同时 reference。ReferencingKeyHashes 长度 100,.meta 文件相应膨胀
   - 短期影响有限(~4 KB);未来若需去重,改用 sharded sidecar
3. **event log rotate 后如何减引用?**
   - 父 RFC rotate 丢弃前 N 条 entry。被挤掉的 ImagePaths 理论上也应从 .meta.ReferencingKeyHashes 反映"这条引用没了"
   - 决策:**不处理**。rotate 后仍有该 keyhash 的其它 entry 引用(极可能)或没有(则下次 GC 按 refTTL 判断自然回收)。实现 rotate-aware clearing 复杂度高、收益低
4. **PDF 是否也应接 refcount?**
   - PDF 引用在 Claude JSONL 的 tool_use 文本里,不在 EventEntry.ImagePaths 里 — 当前设计触达不到
   - 如果 PDF 丢失是实际痛点,另起子 RFC

---

## 7.5. v1 MVP 落地备忘(偏离原设计的点)

落地过程中吸收了两轮 go-reviewer-style 自校,与 §3 相比 MVP 的实际形态做了以下微调,记录在此:

1. **tracker 没有持久化反向索引(§3.4 里说的"keyhash → []abspath 内存表")** — 实测 `OnSessionRemoved` 单次走 `workspace/<Dir>/*/*.meta` 扫描对 <10k 图的 workspace 完全够用(benchmark < 10ms / 1k files,Linux ext4 rapid stat path)。反向索引复杂度高、又需要懒加载恢复,推迟到"实际 workspace 超 10k 后按需加"的 follow-up。
2. **tracker 没有启动时"silent bump 补课"(§3.5)** — 父 RFC 的 replayPhase 判定已经保证回放不触发 tracker bump;实际场景下 .meta 只会缺"崩溃前 200ms 未 fsync 的那点 bump",对用户体验无可感知影响。这项进 follow-up。
3. **GCWithRefs 没有生产 caller** — 现有 `cmd/naozhi/main.go` 没有注册 attachment GC cron job(历史遗漏,与本 RFC 无关)。GCWithRefs 代码和表驱动测试已到位,等运维侧加 cron 即启用;在此之前引用数据仍每秒更新但不会参与清理。短期不影响功能(attachment 目录只增不减) — 长期需要运维侧补 cron。记录在 Phase 6E-5 节,不堵塞 MVP 验收。
4. **Observer 接口签名与父 RFC Persister.Observer 保持形态一致** — `OnReferenceBump(n int)` / `OnReferenceClear(n int)` / `OnMetaWriteError(path, err)` / `OnDrop(n int)`,让 session 层的 forwarding 代码可以机械套用。
5. **workspaceSnapshot 在 Remove 中预先取值**:`Router.Remove` 在 `unregisterSessionLocked` 之前 snapshot `s.Workspace()`,然后 unlock,再异步调 `clearAttachmentTrackerRefs`。tracker worker goroutine 自己 serialize,所以不需要持 r.mu。

## 8. 验收标准(GA)

1. **大图持久可见**:用户上传图 → 14 天后点击 → 仍可看大图
2. **无引用图正常回收**:上传后 session 立刻删除 → 过 7 天 + 30 天 GC 正常清理
3. **ReferencingKeyHashes 列表在多 session 共享时准确**:两个 session 都引用 → 都记录;其中一个 session 删除 → 只移一个 keyhash;仍有引用则保留
4. **崩溃恢复**:kill -9 + 重启 → tracker 重建索引正确,损失 ≤ 200ms 的 meta update
5. **可观测**:三个 expvar counter + /health.attachment_tracker 字段
6. **性能**:正常工作负载下 tracker goroutine CPU < 1% (一张图 meta 更新 < 5ms)
7. **旧数据**:无新字段的 .meta 在升级后第一次 GC 遵 legacy TTL;不立即删除任何可能仍被引用的图
8. **文档**:DESIGN.md / CLAUDE.md / pprof.md 同步;父 RFC event-log-persistence.md §10 的 GA blocker 条目更新

---

## 9. 附录:代码锚点

| 事项 | 文件 / 符号 |
| - | - |
| Meta struct | `internal/attachment/store.go:54` |
| Persist helper | `internal/attachment/store.go:98` |
| GC 当前实现 | `internal/attachment/store.go:205` |
| atomic file write | `internal/osutil/atomicfile.go` |
| cmd attachment GC cron wire | `cmd/naozhi/main.go`(attachment-gc job) |
| EventLog persist sink bridge | `internal/session/eventlog_bridge.go` |
| Router Remove → DropKey | `internal/session/router.go` Remove() |
| Router Shutdown persister lifecycle | `internal/session/router.go` shutdown() |
| /health auth 段 | `internal/server/health.go` healthAuthSection |
| 父 RFC 的 ImagePaths 定义 | `internal/cli/eventlog.go:112` |
