# EventLog 持久化(图片 / 历史事件跨重启可见)

> **状态**: v3 已落地(GA 就绪)— Phase 0-5 + 6b + 6c 完成;attachment-refcount 子 RFC 的 MVP(Phase 6E-1 ~ 6E-4)已完成并接入 Router,GCWithRefs / tracker 已验证,仅等运维侧启用 attachment GC cron job
> **作者**: naozhi team
> **创建**: 2026-05-10
> **依赖 / 前置**: `docs/rfc/attachment-refcount.md`(子 RFC,本 RFC **GA blocker**,见 §3.6 / §10)
> **关联代码**: `internal/cli/eventlog.go` · `internal/cli/uuid.go` · `internal/eventlog/schema/` · `internal/eventlog/persist/` · `internal/history/naozhilog/` · `internal/history/merged/` · `internal/session/router.go` · `internal/session/eventlog_bridge.go` · `internal/session/eventlog_health.go` · `internal/session/eventlog_metrics.go` · `internal/session/eventlog_orphans.go` · `internal/server/health.go` · `internal/server/static/dashboard.js` · `internal/discovery/history.go` · `internal/attachment/store.go`

---

## 0. 修订历史

### v3 GA 实装(2026-05-10 完成 attachment-refcount MVP)

在 event-log-persistence MVP 基础上完成 `attachment-refcount.md` 子 RFC 的 Phase 6E-1 ~ 6E-4:

- `internal/attachment/store.go`:Meta schema 扩展 `ReferencingKeyHashes` / `LastReferencedAt`,新增 `AddReference` / `RemoveReference` / `HasReference` / `GCWithRefs` / `UpdateMetaFile`,保留 `GC`(legacy day-dir 路径)。向后兼容:旧 Meta 无新字段读入不 panic、GCWithRefs 走 legacy 分支。
- `internal/attachment/tracker/`:新包,single-goroutine worker,1s coalesce window 去抖 `.meta` 写,`Observer` 接口镜像 Persister,`OnPersistedEntry` 非阻塞 enqueue(满 channel 丢 + counter)、`OnSessionRemoved` 同步扫 workspace 并串行清 keyhash、`Flush` / `Stop` 幂等。20 条单元测试 + -race 绿。
- `internal/session`:`attachment_tracker.go`(lifecycle + `attachmentMetricsObserver`)+ `eventlog_bridge.go` 扩接 tracker.OnPersistedEntry(只在 replayPhase=false 时触发)+ `eventlog_health.go` 加 `AttachmentTrackerStats()` + Router.Remove 添加 `clearAttachmentTrackerRefs`(在 unregister 前 snapshot workspace)+ NewRouter / Shutdown 绑定启停。集成测试 4 条覆盖 bump / replay skip / clear on remove / disabled config。
- `internal/server/health.go`:`/health.attachment_tracker` 子对象(writer_alive + channel 分量 + 5 个 counter)。
- `internal/metrics/metrics.go`:4 个新 expvar.Int(`naozhi_attachment_ref_{bump,clear,meta_error,drop}_total`)+ `docs/ops/pprof.md` 表 + jq 模板同步。
- 文档:`docs/rfc/attachment-refcount.md` 状态改 v1 MVP 已落地 + §7.5 落地偏离备忘。

30/30 包全 `go test -race -count=1` 绿;`go vet ./...` 无警告。未绑定 GC cron job 的运维缺口记录在子 RFC §7.5 作为后续 ops 工作。

### v3 实装记录(2026-05-10 完成 MVP)

Phase 0 - 5 全部落地,全局 `go test -race -count=1 ./...` 29/29 包绿。新增骨架:

- `internal/eventlog/schema/`(Record / FileHeader / IdxEntry / EntryView,**不 import cli**)
- `internal/eventlog/persist/`(Persister / framing / idx / recovery / rotate / keyhash / FS detection / Observer 接口)
- `internal/history/naozhilog/`(LoadLatest / LoadBefore)
- `internal/history/merged/`(MergedSource:按 `(Time, UUID)` 排序 + UUID 精确去重 + 一侧失败不短路)
- `cli.EventEntry` 新增 `UUID string` 字段;`cli.EventLog` 新增 `SetPersistSink` + 原子 `sinkReady`
- `cli/uuid.go`:`newEventUUID`(crypto/rand)+ `DeriveLegacyUUID`(`uuid5(time + summary + detail)`)
- `discovery.historyLine` 读 Claude JSONL 自带 uuid,缺时 `DeriveLegacyUUID` 兜底
- `session.Router` 生命周期接 Persister;spawnSession / ReconnectShims 严格在 InjectHistory 之后 SetPersistSink
- `session/eventlog_bridge.go`:`cli.PersistSink` → `persist.PersistSink` 的 JSON 序列化桥
- `session/eventlog_health.go` + `session/eventlog_metrics.go`:`/health.eventlog` 与 expvar 暴露层
- `session/eventlog_orphans.go`:NewRouter 启动时扫 > 30 天孤儿 .log / .idx
- `internal/metrics`:5 个新 expvar.Int(written / dropped / fsync / malformed / replay_leak)
- `/health` 新 `eventlog` 段(writer_alive 精确定义 + channel 分量字段 + fs_type / fs_supported)
- `dashboard.js` lightbox 加 `openLightbox(full, thumb)`:`onerror` + `naturalWidth===0` 双兜底 + `?v=<time>` cache-busting

**MVP 验收 §10 已满足**;GA 验收 #9 #10 依赖 `docs/rfc/attachment-refcount.md` 子 RFC(Phase 6a 已完成框架)。

---

### v3 (设计)

基于 second-pass review,吸收 3 个 blocker + 2 个强建议 + 7 个 follow-up:

- **(blocker 1)** §3.2.3 SetPersistSink ordering:AST lint 补 **runtime `replayPhase` 标记** — sink 未设前写入标记为 replay;带标记 entry 穿过 sink → dev panic / prod drop+counter。AST 扫不出的新调用路径由运行时兜底
- **(blocker 2)** §3.2.4 Log/idx 写序硬化:强制 `log.Write → log.Sync → idx.Write → idx.Sync`,idx 永不可能领先 log;启动时截 idx 到 log 内
- **(blocker 3)** §6.3 `/health.writer_alive` 改为 `(last_drain_ms_ago < 5000) && (channel_depth < 0.8 * cap)`;独立导出 `channel_depth` / `last_drain_ms_ago` 字段
- **(强建议 1)** §3.5.2 合并去重默认语义:关闭 fuzzy dedup,走精确 UUID;EventEntry 新增 `UUID` 字段作为 schema v1 的一部分(不再 future work)
- **(强建议 2)** §10 产品验收硬化:子 RFC `attachment-refcount.md` 是**本 RFC GA 的 blocker**,不是"并列推进"。过渡期验收标准写明"大图 TTL 内(至少 7 天)可见"
- (follow-up) §2.4 schema 包加 `EntryView` 接口
- (follow-up) §3.1.1 reader/writer 打开时按 idx 背书 offset ftruncate log
- (follow-up) §3.5.3 明确 seq 只在 local tie-break
- (follow-up) §5.4 NFS/overlayfs 检测写 /health + doctor 飘红 + dashboard banner
- (follow-up) §1.4 三个 500 常量归集到 `internal/cli/limits.go`,注释但不合并
- (follow-up) §3.7 前端 cache-busting(`?v=<uploadedAt>`)+ `naturalWidth===0` 兜底
- (follow-up) §7 每 Phase 加 "done when" exit criteria
- **保持现状**(architect over-engineering warning): PersistSink 单订阅者不改 subscribers list

### v2

基于 first-pass review,吸收 5 个 blocker:
- PersistSink 签名改批量 `func([]EventEntry)`
- SetPersistSink ordering 契约替代 `isReplay` 参数
- 合并语义改 `(time, seq)` + 去重
- length-prefix framing 替代"append 单行原子"假设
- attachment GC 联动拆子 RFC
- 新增 `internal/eventlog/schema` 包解耦磁盘格式
- Rotate 改 O(1) offset-index tail cut
- 文件系统支持矩阵显式化
- 测试矩阵从一句话扩到 20+ 用例

### v1

初稿。

---

## 1. 问题与目标

### 1.1 用户可感知的症状

用户在 dashboard 发送一条带图的消息:
- **首次发送**:缩略图 + 大图点击都正常
- **切换 session 回来 / 刷新 dashboard / 重启 naozhi**:图**消失**

### 1.2 根因

图片在 `EventEntry` 上承载两份数据(`internal/cli/eventlog.go:91-139`):

| 字段 | 内容 | 产生者 |
| - | - | - |
| `Images []string` | `data:image/jpeg;base64,...` 缩略图 | `MakeThumbnail` |
| `ImagePaths []string` | `.naozhi/attachments/<date>/<uuid>.<ext>` | upload handler |

两者**只活在进程内存**:`cli.EventLog.entries` 500 条 ring + `ManagedSession.persistedHistory` 500 条 slice。重启后回退到 `history.Source` → `claudejsonl.Source` → `discovery.parseHistoryLine`,而后者**只提取 text 块**,因此 Images/ImagePaths **永远为 nil**。

Claude CLI JSONL 里也不含 naozhi 自己的 `ImagePaths`。即便扩展解析器,原图路径永远拿不回来。

### 1.3 目标

**Must-have**:
1. `EventEntry` 所有字段(Images / ImagePaths / AskQuestion / Agent-team linkage / todo / ...)跨重启可复原
2. 消除当前 `persistedHistory` 时间戳乱序 + 每次读路径排序的 O(n log n) 开销
3. 不依赖也不替代 Claude CLI JSONL;naozhi 永远可读它作为退化源
4. 持久化失败可观测(metrics + slog),禁止静默丢数据

**Nice-to-have**:
1. backend 无关(ACP / Kiro / Gemini 未来接入零改造)
2. `persistedHistory` 上限可下调至与 ring 一致

**非目标**:
1. 跨节点同步(走 reverseNode 实时流)
2. 按内容查询 / 索引 — 仅顺序回放 + 分页
3. 去重 / 压缩 — 先跑,按实测体积再优化

### 1.4 容量常量归集

`internal/cli/limits.go`(新增)把三处 "500" 放一起,语义不同**不合并**:

```go
package cli

// defaultEventLogSize 是单个 Process 的 in-memory ring buffer 上限。
// 作用:活跃会话内存常驻事件数。超出时环形淘汰最老事件。
const defaultEventLogSize = 500

// maxPersistedHistory 是 ManagedSession.persistedHistory 的 slice 上限。
// 作用:session 在 CLI 挂掉 / 重启后的内存级历史。与 ring 独立。
const maxPersistedHistory = 500  // (位于 session 包)

// DefaultLoadLatestLimit 是 naozhilog.Source.LoadLatest 的单次读页大小。
// 作用:启动时从磁盘 load 回 persistedHistory 的条数。
const DefaultLoadLatestLimit = 500
```

三个 500 恰好同值是历史一致性要求(先满 ring → 满 persistedHistory → 填 LoadLatest),但语义维度不同,**抽到一个配置项反而错**。归集的价值是注释写在一起让 reviewer 一看就懂。

---

## 2. 设计概览

### 2.1 分层

```
┌─────────────────────────────────────────────────────────────────┐
│  cli.EventLog.Append / AppendBatch  (热路径, 有 l.mu)           │
│   ├─ 锁内: 写 ring, 若 sink == nil 标记 Record.ReplayPhase=true │
│   └─ 锁外: 调用 persistSink([]EventEntry)                       │
└────────────────────────────┼─────────────────────────────────────┘
                             ▼
┌─────────────────────────────────────────────────────────────────┐
│  internal/eventlog/persist.Persister   (single goroutine)       │
│   ├─ 过滤带 ReplayPhase=true 的 entry → dev panic / prod drop    │
│   ├─ per-key lazy *os.File fd pool                              │
│   ├─ 200ms debounce fsync (log + idx 严格写序)                  │
│   ├─ length-prefix framing + offset idx sidecar                 │
│   └─ rotate (offset-index 指导, O(1) tail cut)                  │
└─────────────────────────────────────────────────────────────────┘
                             ▼
         ~/.naozhi/events/<keyhash>.log  +  <keyhash>.idx
```

```
┌─────────────────────────────────────────────────────────────────┐
│  internal/history/naozhilog.Source                              │
│    ├─ LoadLatest(key, limit)  → 启动填 persistedHistory         │
│    ├─ LoadBefore(key, beforeMS, limit)  → 分页                  │
│    └─ MergedSource{local, claudejsonl} 按 UUID 精确合并          │
└─────────────────────────────────────────────────────────────────┘
                             ▲
                             │ 由 router.attachHistorySource 注入
                             │
┌─────────────────────────────────────────────────────────────────┐
│  session.Router                                                 │
│    ├─ 启动:loadStore → attachHistorySource(Merged)              │
│    │         → naozhilog.LoadLatest → InjectHistory             │
│    │         → SetPersistSink(persister.SinkFor(key))           │
│    ├─ spawnSession:InjectHistory 完成之后才挂 sink (契约)       │
│    └─ ResetChat/Reset/Remove/Cleanup → persist.DropKey          │
└─────────────────────────────────────────────────────────────────┘
```

### 2.2 核心原则

1. **唯一入口**:`cli.EventLog.Append` 是所有 event 的必经点;持久化只在这一处介入
2. **非阻塞**:持久化永不阻塞 Append 热路径;失败只丢单条 + 打点告警
3. **幂等 replay**:通过 `ReplayPhase` runtime 标记 + SetPersistSink ordering 双保险,replay 事件不回写磁盘
4. **schema 与业务解耦**:磁盘格式独立 package,不等于 `cli.EventEntry`
5. **写序严格**:log 先 fsync,idx 后 fsync;idx 永不领先 log
6. **没有全局 SPOF**:每个 session 自包含文件,任一损坏不波及他人

### 2.3 存储布局

```
~/.naozhi/
  sessions.json                    # 现有
  sessions.meta.json               # 现有
  events/                          # 新增
    <keyhash>.log                  # append-only framed records
    <keyhash>.idx                  # (seq, byteOffset, timeMS) 稀疏索引
    <keyhash>.tmp.<epoch>.log      # rotate 临时文件
    <keyhash>.tmp.<epoch>.idx
```

**无全局 index.json**。文件自包含(header 记在每个 log 第一条)。

### 2.4 数据模型包 `internal/eventlog/schema`

```go
// schema 只承载磁盘 wire 格式,cli / persist / naozhilog 都 depend 它。
// schema 包本身不 import cli。
package schema

const WireVersion = 1

// FileHeader 是每个 <keyhash>.log 的首条记录(type="header")。
type FileHeader struct {
    Version   int    `json:"v"`
    Key       string `json:"key"`        // 原 session key(未 hash)
    CreatedAt int64  `json:"created_at"` // unix ms
    Generator string `json:"gen,omitempty"`
}

// Record 是所有持久化行的统一信封。
type Record struct {
    V     int             `json:"v"`
    Seq   uint64          `json:"seq"`              // 单 session 内单调递增
    Type  string          `json:"type"`             // "header" | "entry"
    Entry json.RawMessage `json:"entry,omitempty"`  // type="entry" 时携带 EventEntry JSON
    Header *FileHeader    `json:"header,omitempty"` // type="header" 时非 nil
}

// EntryView 是 schema 对 Entry 字段的**最小**访问面。
// 由 cli.EventEntry 通过 schema.NewEntryView(json.RawMessage) 构造。
// 让 schema 包能做 round-trip 测试(fixture EventEntry JSON → Record →
// Record → EntryView),保证字段演进在 schema 层 CI 捕获,而不必等到
// persist/naozhilog 层才发现字段丢失。
type EntryView interface {
    Time() int64
    UUID() string      // v1 起必填;用于合并去重(见 §3.5.2)
    Kind() string      // "user" | "text" | "tool_use" | ...
    HasImages() bool   // 供 rotate 策略决定是否触发尺寸检查
}
```

演进规则:
- Additive 字段 + `omitempty` → WireVersion 不 bump
- 破坏性改动 → bump,旧 reader 拒读 + slog.Error,降级到 Claude JSONL fallback
- **永不做 best-effort 跨版本兼容**:不认识的 Version 放弃该文件,不猜

---

## 3. 详细设计

### 3.1 文件格式

#### 3.1.1 Framing

行格式:`<decimal-length>\n<json-record>\n`

```
42
{"v":1,"seq":0,"type":"header","header":{...}}
318
{"v":1,"seq":1,"type":"entry","entry":{"time":...,"uuid":"a1b2...","type":"user",...}}
```

**为什么 length-prefix**:
- `cli.EventEntry` 带 Images data URI ≥ 30 KiB,远超 POSIX `PIPE_BUF` 4 KiB
- `write > PIPE_BUF` 非原子;reader 读写入中的大行必见半行
- length-prefix 让 reader 两步判断:读 length → 按 length 精确读 body;长度不足 / 解析失败 → `slog.Debug("partial trailing record")` + 丢弃
- **永远不尝试修复**,只丢

**Reader/Writer 启动时的截断契约**(follow-up 补):
- Writer 启动时:读 `idxLast.ByteOff + idxLast.Len`(idx 末条背书的边界) → `ftruncate(log, edge)`,把任何 idx 不背书的尾部字节砍掉再 append
- Reader 打开时:按 `log.Size()` 为上界顺序读;读到 length 而 body 不够就停止(不假设后面还有数据)
- Writer 的截断**必须在 SetPersistSink 之前完成**,对外暴露的 log 永远只含 idx 背书过的完整 record

#### 3.1.2 Header 自包含

每个 log 首条记录是 `type="header"`:
- 无需全局 index.json 反查 keyhash → key
- Version 自携带,文件可独立被校验 / 备份 / 移植
- 首次 create 时写 header → `log.Sync() → SyncDir`(确保目录项可见)

#### 3.1.3 Offset Index Sidecar

`<keyhash>.idx` 是稀疏索引,每 N 条(默认 32)一条:

```
struct IdxEntry { Seq uint64; ByteOff int64; Len int32; TimeMS int64 }  // 28 bytes
```

编码:定长二进制,顺序 append。Len 字段是 **本 record 的总字节数**(含 length 前缀和尾 `\n`),让启动截断逻辑不需要再 seek log 读长度。

用途:
- Rotate O(1) tail cut(§5.2):二分 + seek copy
- LoadBefore 分页:按 `timeMS` 二分定位起点
- 启动时 log 完整性校验:`ftruncate(log, idxLast.ByteOff + idxLast.Len)`

**idx 损坏 → 整个扔掉重建**(扫 log 一遍 rebuild idx,100 MiB 约 1s,罕见)。
**log 损坏 → 截到 idx 背书的最后安全点**,不保留 tail 残缺字节。

### 3.2 写入:Persister

```go
// internal/eventlog/persist/persister.go
type Persister struct {
    dir          string
    maxFileBytes int64
    in           chan batchJob
    closeCh      chan struct{}
    wg           sync.WaitGroup

    writers      map[string]*perKeyWriter  // 单 goroutine 访问,无锁

    // 观测字段(atomic)
    writtenCnt   atomic.Int64
    droppedCnt   atomic.Int64
    fsyncCnt     atomic.Int64
    malformedCnt atomic.Int64
    lastDrainNS  atomic.Int64  // 每次从 in channel 取出 batch 后更新(wall-clock ns)
    replayLeakCnt atomic.Int64 // ReplayPhase=true 的 entry 穿过 sink 的次数
}

type batchJob struct {
    Key     string
    Entries []cli.EventEntry
    ReplayPhase bool  // 见 §3.2.3
}

type perKeyWriter struct {
    logFile      *os.File
    idxFile      *os.File
    nextSeq      uint64
    bytes        int64
    flushTimer   *time.Timer
    idleTimer    *time.Timer
    pendingFsync bool
}
```

#### 3.2.1 Sink 签名

```go
// internal/cli/eventlog.go
type PersistSink func(entries []EventEntry, replayPhase bool)

func (l *EventLog) SetPersistSink(fn PersistSink) { ... }
```

**`replayPhase` 参数的含义**:EventLog 自己知道当前是否处于 replay 阶段(见 §3.2.3 的 `replayPhase atomic.Bool`),传给 sink 让 Persister 决策丢弃。

**why batch 签名**(保留 v2 的决定):AppendBatch 锁外一次性 send 整批,Persister channel FIFO 保证 batch 内 ordering;避免单条 send 与其他 goroutine Append 交错导致的顺序错位。

#### 3.2.2 SetPersistSink ordering 契约

调用顺序必须是:

```
proc := cli.Spawn(...)                       // eventLog 空
// (若有历史回放)
proc.InjectHistory(replay)                   // 走 AppendBatch, sink 未设, 不落盘
proc.EventLog().SetPersistSink(persister.SinkFor(key))
// 此后新 event 才走 sink
```

**契约保证**:InjectHistory 全部完成后再 SetPersistSink。

契约测试:
- `spawn_sink_after_inject_test.go`:用 reflect+AST 扫 `SetPersistSink` 的**同方法内**前置调用必须是 `InjectHistory` 或带"inject done"语义的 helper
- **AST lint 覆盖不到的跨方法 / 跨包调用路径**由下一节的 runtime 兜底

#### 3.2.3 Runtime 兜底(blocker 1 修复)

AST lint 只能扫同一函数内的顺序,scratch / shim reconnect / 未来 ACP/Kiro 等跨 goroutine/跨包路径扫不到。靠约定容易失守。

硬化:在 `EventLog` 上加 runtime 状态机:

```go
// internal/cli/eventlog.go
type EventLog struct {
    // ... existing fields ...
    sinkReady atomic.Bool            // true 一旦 SetPersistSink 被调用
    persistSink atomic.Pointer[PersistSink]
}

// 语义:sink 未 Set 之前,所有 Append/AppendBatch 的事件都标记为 replay,
// 即便未来真的泄漏到 sink 也能被过滤。
func (l *EventLog) isReplayPhase() bool {
    return !l.sinkReady.Load()
}

func (l *EventLog) SetPersistSink(fn PersistSink) {
    // 严格顺序:先装载 sink 指针, 再翻 sinkReady。读侧按 LoadAcquire-StoreRelease 语义保证
    pp := &fn
    l.persistSink.Store(pp)
    l.sinkReady.Store(true)
}

func (l *EventLog) AppendBatch(entries []EventEntry) {
    replayPhase := l.isReplayPhase()
    // ...内部处理 ring...
    if p := l.persistSink.Load(); p != nil {
        (*p)(entries, replayPhase)   // 由 sink 实现决策丢弃
    }
}
```

Persister 侧:

```go
func (p *Persister) SinkFor(key string) PersistSink {
    return func(entries []EventEntry, replayPhase bool) {
        if replayPhase {
            p.replayLeakCnt.Add(int64(len(entries)))
            if devMode {
                panic("event log persistence: replay-phase entries reached sink")
            }
            slog.Error("event log persist: replay-phase entries leaked to sink",
                "key", key, "count", len(entries))
            return
        }
        select {
        case p.in <- batchJob{Key: key, Entries: entries}:
        default:
            p.droppedCnt.Add(int64(len(entries)))
            slog.Warn("event log persist: channel full, dropping batch",
                "key", key, "count", len(entries))
        }
    }
}
```

**两层防御**:
1. 编译期:AST lint 扫 SetPersistSink 前置 InjectHistory(同函数)
2. 运行期:EventLog 自己记录 replay 阶段;即便 sink 被提前挂也能丢

这样新增任何路径只要**忘了**先 InjectHistory 就 SetPersistSink,在 dev 构建下立即 panic,CI 脚本 + 手测都会抓到;prod 构建 drop + counter,运维可通过 `naozhi_eventlog_replay_leak_total > 0` 告警。

Counter 契约:`naozhi_eventlog_replay_leak_total` 在生产稳态必须 0,非 0 是 bug signal。

#### 3.2.4 Log/idx 写序硬化(blocker 2 修复)

v2 只讲了 "idx 可落后 log,启动时补齐",但**反方向**(idx 领先 log)未处理。场景:page cache 先接到 idx 的 write,log 的 write 还没落盘就掉电,重启后 idx 指向 log 外的 offset → LoadLatest 读就 panic。

硬化写序(每个 flush 点严格):

```
1. logFile.Write(record bytes)          // user-space → page cache
2. logFile.Sync()                        // page cache → disk (log 先到盘)
3. idxFile.Write(idxEntry bytes)         // idx 入 page cache
4. idxFile.Sync()                        // idx 到盘
```

**保证**:idx 的持久化严格后于 log。无论何时崩溃,`idx.Size() / 28 * 28` 对应的 ByteOff 必然已在 log 内持久化。

启动时校验:
```go
// Writer/Reader 打开 <keyhash>.log 之前:
idxSize := stat(idxFile).Size()
logSize := stat(logFile).Size()

// 1. idx 长度规整到 28 的倍数(tail 未完成写也丢)
idxSize = idxSize - idxSize%28

// 2. 读 idx 最后一条 IdxEntry
last := readIdxAt(idxFile, idxSize - 28)

// 3. 如果 idx 背书的 edge 超过 log 实际大小 → idx 领先, 截 idx
edge := last.ByteOff + int64(last.Len)
if edge > logSize {
    // 从 idx 尾部往前找第一条 edge' <= logSize
    shrinkIdx(idxFile, first-safe-entry)
}

// 4. 然后 log 也截到最后一条 idx 背书的 edge(丢尾部 idx 不背书的字节)
ftruncate(logFile, safeEdge)
```

对应 debounce 内部,Write 序改为:

```go
func (w *perKeyWriter) flush() {
    w.logFile.Sync()                 // 2
    for _, e := range pendingIdx {
        binary.Write(w.idxFile, ...)  // 3
    }
    w.idxFile.Sync()                 // 4
    w.pendingFsync = false
    w.lastFsyncAt = time.Now()
}
```

每批 batch 内:先顺序 `log.Write` N 次 → 一次 `log.Sync` → `idx.Write` N 次 → 一次 `idx.Sync`。单 goroutine 消费保证无并发 interleave。

契约测试:
- `write_order_sync_test.go`:mock 文件 I/O(`syscall.Sync` stub),注入"log.Sync 后、idx.Write 前"崩溃点,验证重启后 idx 至多等于 log,不会超过
- `startup_truncation_test.go`:构造 "idx 领先" / "log 领先" / "两者齐头" 三种 initial state,验证启动 recovery 正确

### 3.3 Replay 不再污染持久化(双保险)

1. **结构性隔离**(v2):SetPersistSink 在 InjectHistory 完成之后,sink == nil 期间 replay 根本没处可泄
2. **Runtime 兜底**(v3 新增):EventLog 的 `replayPhase` 原子标记,即便 sink 被提前挂也能丢

**契约测试** `router_reconnect_no_self_loop_test.go`:模拟 shim reconnect 50 次,验证:
- 单 session 的 `<keyhash>.log` 文件大小与 reconnect 次数无关(差值 < 100 B,容忍 seq 递增的 metadata 波动)
- `naozhi_eventlog_replay_leak_total` 保持 0

### 3.4 读取路径

`naozhilog.Source`:

```go
// internal/history/naozhilog/source.go
type Source struct {
    dir string
    key string
}

func (s *Source) LoadLatest(ctx context.Context, limit int) ([]cli.EventEntry, error)
func (s *Source) LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]cli.EventEntry, error)
```

读取使用只读 fd(`os.Open`),不触发 writer 的截断逻辑(writer 启动早于 reader 首次读)。读到 partial tail record 静默丢弃。

### 3.5 合并算法(MergedSource)

#### 3.5.1 结构

```go
// internal/history/merged/source.go
type MergedSource struct {
    Local    history.Source  // naozhilog.Source
    Fallback history.Source  // claudejsonl.Source 或 history.Noop
}
```

替换 `router.attachHistorySource` 的原 `claudejsonl.Source` 单源。

#### 3.5.2 精确去重(强建议 1 修复)

**v2 的 fuzzy dedup(Summary hash + Time)废弃**。

EventEntry 新增字段作为 schema v1 必填:

```go
// internal/cli/eventlog.go
type EventEntry struct {
    UUID string `json:"uuid"`       // 16 字节 crypto/rand hex, 32 字符
    Time int64  `json:"time"`
    // ...其他字段不变...
}
```

- `cli.EventLog.Append` 在入口处 `if e.UUID == "" { e.UUID = newUUID() }` 统一补齐
- `Process.InjectHistory` 时对**缺 UUID 的历史 entry**(来自 Claude JSONL parser 的老 entry)走 `uuid5(namespace, time + summary)` 稳定派生 —— 保证同一条 Claude record 在两次 InjectHistory 里得到相同 UUID,不会因时间精度漂移被去重判异
- `discovery.parseHistoryLine` 补一个 `deriveUUID(line)` 函数,把 JSONL 里的 `{ "uuid": "<claude-uuid>" }` 直接用上(claude 的 uuid 是其消息 record id,长期稳定);没有时用 uuid5 派生

合并算法:

```go
func (m *MergedSource) LoadBefore(ctx context.Context, beforeMS int64, limit int) ([]cli.EventEntry, error) {
    local, _ := m.Local.LoadBefore(ctx, beforeMS, limit)
    fallback, _ := m.Fallback.LoadBefore(ctx, beforeMS, limit)

    merged := append(local, fallback...)
    // 1. 按 (Time, Source-tag) 排序;Source-tag 让 local 在同 Time 排前
    // 2. 按 UUID 去重, 先出者保留(即 local 覆盖 fallback)
    // 3. 截取 newest `limit` 条
    return sortDedupCap(merged, limit), nil
}
```

**fuzzy 去重作为 feature flag 的降级路径**:`NAOZHI_EVENTLOG_FUZZY_DEDUP=1` 启用(默认 off)。用于升级前已产生的老 EventEntry 没有 UUID 的迁移期——但迁移策略是 `uuid5(time+summary)` 稳定派生,fuzzy 永远不默认。

**lossy 不进默认路径** — 用户连按 "继续" / "ok" 不会被合并。

#### 3.5.3 Seq 的作用范围(follow-up 补)

`Record.Seq` 是 **local-only tie-break**,不参与跨 source 比较:
- 排序主键永远是 `(Time, UUID)`;同 Time 同 UUID 按 Seq 稳定(local 内部)
- 跨 source 的合并:MergedSource 永远先比 Time,从不碰 Seq
- 明确写在 §3.1.3 IdxEntry 定义的 godoc 里

#### 3.5.4 退化矩阵

| local | fallback | MergedSource |
| - | - | - |
| 空 | 空 | nil(end of history) |
| 500 | 空 | local 500 |
| 空 | 500 | fallback 500 |
| 50 重叠 500 | — | 合并后按 UUID dedup,newest 500;时间线无断层 |
| 500 | 500 无重叠(更老) | 按时间交错合并,newest 500 |
| 损坏 | 500 | fallback 500,slog.Warn local 损坏 |
| 500 | (Kiro/ACP → Noop) | local 500 |

### 3.6 Attachment 引用 / 子 RFC 关系(强建议 2 修复)

#### 3.6.1 本 RFC 硬决策:`Images` 缩略图 data URI 强制持久化

- 保证即便原图被 attachment GC 删,缩略图仍可渲染气泡
- 代价:单 session 上限 500 条 × 平均 30-80 KiB/条 ≈ 最坏 40 MiB;多图场景 100-300 KiB/条 → 最坏 150 MiB
- §5.2 rotate(100 MiB 阈值)必须到位

#### 3.6.2 子 RFC `docs/rfc/attachment-refcount.md` 是**本 RFC GA blocker**

**不再是"并列推进"**(v2 措辞)。理由:没有 refcount,大图在 attachment GC TTL 过期后 404,点击只能看模糊缩略图 — 这**不算交付完成**,是功能 bug。

GA 验收(§10):
- MVP 发布条件:本 RFC + Phase 0-5 完成,大图 GC TTL 内可见(至少 7 天)
- GA 发布条件:attachment-refcount 子 RFC 落地,原图在引用期内永不过期

子 RFC 骨架(待独立撰写):
- attachment meta 增加 `referencing_sessions []string` + `last_referenced_at int64`
- event log Append 写 Images/ImagePaths 时异步登记引用到 attachment store
- GC 策略:`(uploaded_at + maxTTL) AND (last_referenced_at + refTTL)` 双过期
- session Remove/Reset 时 attachment store 清理引用
- 旧 attachment 无 meta 字段按 legacy TTL(向后兼容)

### 3.7 前端降级(Phase 0,独立先行)

`dashboard.js` lightbox + cache-busting(follow-up 补):

```js
function openLightbox(fullUrl, thumbDataUri, uploadedAt) {
  const full = fullUrl + (uploadedAt ? ('?v=' + uploadedAt) : '');
  const img = new Image();
  img.onerror = () => { renderLightbox(thumbDataUri); };
  img.onload  = () => {
    if (img.naturalWidth === 0) {  // 200 OK 但 Content-Type 错, onerror 不触发
      renderLightbox(thumbDataUri);
      return;
    }
    renderLightbox(full);
  };
  img.src = full;
}
```

HTML 侧 thumb 元素 `data-full` / `data-thumb` / `data-uploaded-at` 同时携带。

**EventEntry 增加 `UploadedAt []int64` 字段**(Images/ImagePaths 同维度 index-aligned,来自 attachment.Meta.UploadedAt)。若未持久化可回退到 0,不发 `?v=` query。

---

## 4. 接入 `session.Router`

### 4.1 启动流程

```go
NewRouter(cfg)
  ├─ loadStore(sessions.json)
  ├─ for each session entry:
  │    ├─ new ManagedSession
  │    ├─ merged := &merged.Source{
  │    │     Local:    naozhilog.New(dir, key),
  │    │     Fallback: claudejsonl.New(claudeDir, cwd, chainIDs),
  │    │   }
  │    ├─ s.SetHistorySource(merged)
  │    └─ go func() {
  │         local := naozhilog.LoadLatest(key, DefaultLoadLatestLimit)
  │         if len(local) > 0 {
  │           s.InjectHistory(local)
  │           return
  │         }
  │         // 新系统、或本地文件被删 → 走 legacy 路径
  │         legacy := discovery.LoadHistoryChainTailCtx(...)
  │         s.InjectHistory(legacy)
  │       }()
  └─ ...
```

### 4.2 spawnSession 挂 sink(顺序契约)

```go
proc, _ := cli.Spawn(...)
if h := s.snapshotPersistedHistory(); len(h) > 0 {
    proc.InjectHistory(h)   // sink 未挂, replayPhase=true, 不落盘
}
proc.EventLog().SetPersistSink(persister.SinkFor(s.key))
// 此后 replayPhase 原子翻 false, 新 event 才走 sink
```

**AST contract test** `spawn_sink_after_inject_test.go`:扫 `session/router.go` 中所有 `SetPersistSink` 调用点,前 20 个 AST 节点内必须出现 `InjectHistory` 或 "//nolint:sinkorder" 注释(后者需 review 明确)。

### 4.3 删除流程

`ResetChat / Reset / ResetAndRecreate / Remove / Cleanup / scratch 销毁`:

```go
persister.DropKey(s.key)  // 删 <keyhash>.log + <keyhash>.idx + *.tmp.*
```

DropKey 需要:
- 先 close 对应 perKeyWriter 的 fd
- 再 os.Remove 两个文件
- 失败不传播错误,slog.Warn 即可(session 本来就要删,残留文件下次启动会被认为 orphan 再清)

### 4.4 Orphan 清理

启动时扫 `events/*.log` 的文件名列表,与 loadStore 回来的 session keys 对比(通过 KeyHash),孤儿文件 > 30 天直接删。避免用户 `rm sessions.json` 后 events 文件永远堆积。

---

## 5. 代价与风险

### 5.1 磁盘 I/O

- 稳态 5 events/s × 2-80 KiB → 10-400 KiB/s per hot session
- 100 活跃 session 最坏 40 MiB/s(全多图),典型 < 5 MiB/s
- fsync 200ms debounce 下 ≤ 5 次/s;EBS gp3 承载充足
- log + idx 双 fsync 成本 ≈ 40ms/批(gp3),但都在 single writer goroutine 内串行,不影响生产者

### 5.2 Rotate

触发:`perKeyWriter.bytes > maxFileBytes`(默认 100 MiB)。

算法(O(1) tail cut):

```
keep := DefaultLoadLatestLimit * 2     // 保留 1000 条
idxEntries := readIdx(<keyhash>.idx)
cutIdxEntry := idxEntries[len(idxEntries) - ceil(keep/N)]
cutOff := cutIdxEntry.ByteOff

tmpLog := create(<keyhash>.tmp.<epoch>.log)
writeHeader(tmpLog)                        // 新 header, CreatedAt 更新
copyFrom(oldLog, cutOff, tmpLog, oldSize - cutOff)
tmpLog.Sync()

tmpIdx := create(<keyhash>.tmp.<epoch>.idx)
writeIdxReindexed(tmpIdx, tmpLog)          // seq 保持原值, byteOff 重新计算
tmpIdx.Sync()

rename(tmpLog → <keyhash>.log)   // atomic, POSIX
rename(tmpIdx → <keyhash>.idx)
```

rotate 期间 Persister goroutine 串行:
- 消费不暂停(新 event 继续写 old log 的 fd)
- rename 发生瞬间关闭 old fd,打开 new fd
- 新 event 可能写进 "新 log 跳过保留区尾部" 的后续,seq 从保留区最末 + 1 继续
- 阻塞时间 = copy 量(最大 maxFileBytes - cutOff ≈ 50 MiB),EBS gp3 大约 500ms。channel 1024 buffer 够缓冲

**失败回滚**:tmp rename 失败 → 删 tmp 文件,继续用 old,日志 Warn,下个 100 MiB 再试

启动时清理 `*.tmp.*` 文件。

### 5.3 并发正确性

- 写者并发:不存在(single goroutine)
- 读者并发:LoadLatest / LoadBefore 可被 HTTP handler 并发调;只读 fd,与 writer append 通过 length-prefix 容忍 partial tail
- 读 rotate 中的文件:reader open 拿到的是 rotate 前 inode,rename 后其 fd 仍指向 old inode(POSIX),读完整 old 文件,unlink 延后发生。无 torn read

### 5.4 文件系统支持矩阵

| 文件系统 | 支持状态 | 处理 |
| - | - | - |
| ext4 / xfs (Linux) | ✅ 推荐 | 正常 |
| APFS (macOS) | ✅ 开发环境 | 正常 |
| NFS | ⚠️ 风险 | 启动 detect → `/health.eventlog.fs_type = "nfs"` + `doctor` 飘红 + dashboard banner |
| overlayfs (容器) | ❌ 不支持 | 同 NFS 的可见性;建议挂 volume 到本地盘 |
| tmpfs (`/dev/shm`) | ⚠️ 信息级 | 数据重启丢;slog.Info 告知 |

**检测实现**:`syscall.Statfs` 的 `Type` 字段,与 Linux `STATFS_*` 常量对照。

**可见性**:
- 启动 slog.Warn + 一次 / 24h 重复(防日志爆)
- `/health` 响应 `eventlog.fs_type` + `eventlog.fs_supported` 字段
- `doctor` 子命令检查该字段,不支持文件系统飘红
- dashboard 登录成功后拉 `/health`,不支持文件系统在顶栏显示 banner "您的部署使用 NFS,event log 数据可能丢失,建议联系运维"

### 5.5 崩溃恢复

- `fsync` 前崩溃:丢 ≤ 200ms 事件(debounce 窗口)
- `log.Write` 半完成 + length 已写:reader length-prefix 丢尾;writer 启动 `ftruncate` 到 idx 背书 offset
- `idx.Write` 半完成(len % 28 != 0):启动规整到 28 倍数
- idx 领先 log(崩溃于 log.Sync 之后、idx.Write 之后、idx.Sync 之前 — 其实此序在 §3.2.4 硬化后**不可能发生**):启动 detect idx tail → logSize,截 idx
- idx 完全损坏:扫 log 一次重建 idx(100 MiB ≈ 1s)

### 5.6 升级 / 回滚

- 新版本读新 events/:按 Version 判;不认识 → slog.Error + fallback 到 Claude JSONL
- 旧版本不访问 events/,无冲突
- 回滚:`rm -rf ~/.naozhi/events/` 旧版本正常,新版本下次启动自填
- **永不做 best-effort 跨版本兼容**

---

## 6. 可测试性

### 6.1 测试矩阵

| 层 | 用例 | 说明 |
| - | - | - |
| schema | Record / FileHeader round-trip | JSON 序列化反序列化 |
| schema | EntryView 反射 cli.EventEntry | 字段演进 CI 抓 |
| framing | length-prefix 正常读 | |
| framing | 半行截断,reader 丢弃尾记录 | |
| framing | length 非数字,reader 停止 | |
| idx | sparse sampling 触发 N=32 | |
| idx | 28 非整数倍 tail,启动规整 | |
| idx | idx 领先 log,启动截 idx | blocker 2 回归 |
| idx | idx 损坏,扫 log 重建 | |
| write-order | mock FS 注入"log.Sync 后 idx.Write 前"崩溃 → 重启后 idx 不超过 log | blocker 2 回归 |
| rotate | 触发条件 / cut / rename atomic | |
| rotate | 失败回滚不丢数据 | |
| rotate | 期间 reader 不错 | |
| sink-order | SetPersistSink 未调用时 Append 不落盘 | blocker 1 回归 |
| sink-order | sink 被挂后 replay leak counter 增 | blocker 1 回归 |
| sink-order | AST lint 扫 SetPersistSink 前置 InjectHistory | |
| merge | local 空 / fallback 500 | |
| merge | local 50 + fallback 500 重叠 | UUID dedup |
| merge | UUID 冲突 local 覆盖 | |
| merge | Time 相同 UUID 不同 不合并 | 连按重复消息保留 |
| merge | 连按 "ok" 两次 0.1s 内 不合并 | 强建议 1 回归 |
| replay-loop | shim reconnect 50 次文件大小 invariant | |
| concurrency -race | reader vs writer 大量 Append + LoadLatest | |
| concurrency -race | rotate 中 LoadBefore 不 panic | |
| crash | kill -9 before fsync → 重启 log 截完整 | |
| crash | kill -9 mid-rename → 启动清 tmp 保留 old | |
| crash | ENOSPC → Append 不 panic, drop counter 增 | |
| migration | events/ 空 → 走 legacy claude JSONL | |
| migration | 降级后再升级 → 残留 events 文件不冲突 | |
| end-to-end | Dashboard 发图切换回来图在 | |
| end-to-end | 重启服务图在 | |
| end-to-end | attachment GC 过期大图 404,前端降级到缩略图 | |
| end-to-end | FS 探测 NFS → dashboard banner 显示 | follow-up 回归 |
| fuzzy | `NAOZHI_EVENTLOG_FUZZY_DEDUP=1` 启用 时 summary+time 合并 | |

### 6.2 基准

- `BenchmarkEventLogAppend`:sink off vs on 对比,目标 on < 2× off
- `BenchmarkLoadLatest_10kRecords`:冷启动读 10k 条,目标 < 200ms
- `BenchmarkMergedSource_Overlap`:1k 条 local + 1k 条 claude 合并 dedup,目标 < 10ms

### 6.3 可观测性

新增 5 个 `expvar.Int`:
- `naozhi_eventlog_persist_written_total`
- `naozhi_eventlog_persist_dropped_total`
- `naozhi_eventlog_persist_fsync_total`
- `naozhi_eventlog_persist_malformed_lines_total`
- **`naozhi_eventlog_persist_replay_leak_total`**(blocker 1):生产稳态应为 0

`counter_wiring_contract_test.go` 锁每个 Inc 点。

`/health` 响应(blocker 3 修复):

```json
{
  "eventlog": {
    "dir": "/home/user/.naozhi/events",
    "file_count": 17,
    "disk_free_bytes": 1234567890,
    "fs_type": "ext4",
    "fs_supported": true,
    "writer_alive": true,
    "channel_depth": 12,
    "channel_cap": 1024,
    "last_drain_ms_ago": 145,
    "drop_total": 0,
    "replay_leak_total": 0
  }
}
```

`writer_alive` 精确定义:

```go
func (p *Persister) WriterAlive() bool {
    lastNS := p.lastDrainNS.Load()
    if lastNS == 0 { return false }  // 从未 drain,也许死锁
    ageMS := (time.Now().UnixNano() - lastNS) / 1e6
    depth := atomic.LoadInt32(&p.chanDepth)
    capacity := int32(cap(p.in))
    return ageMS < 5000 && depth < capacity*4/5
}
```

`/health.eventlog.writer_alive=false` 触发 monitor 告警。同时独立 `channel_depth` / `last_drain_ms_ago` 字段允许运维判断究竟是卡死还是临近满,不要把判定压在 bool 里。

doctor 检查:
- `writer_alive == true`
- `fs_supported == true`(否则 warning 但不 fail)
- `disk_free_bytes > 1 GiB`
- `replay_leak_total == 0`(否则 error)

---

## 7. 实现阶段(每 Phase 带 "Done when" exit criteria)

### Phase 0 — 前端降级(0.5-1 天)

**独立先行**,零后端依赖。

**Done when**:
- dashboard.js lightbox `openLightbox(full, thumb, uploadedAt)` 接新签名
- `?v=<uploadedAt>` cache-busting 附加到 full URL
- `onerror` + `naturalWidth===0` 双兜底到 thumb data URI
- 手测:attachment 被 GC 后点大图显示缩略图,不出 broken image
- 合并前 **不依赖** 本 RFC 其他任何 Phase

### Phase 1 — Schema + Persister 骨架 + 测试基建(6-8 天)

**Done when**:
- `internal/eventlog/schema` 包:Record / FileHeader / WireVersion / EntryView 接口,单测覆盖 JSON round-trip + EntryView 反射 cli.EventEntry
- `internal/eventlog/persist` 包:Persister 骨架(channel / debounce / fd LRU / DropKey)
- Framing read/write 单测绿(含 partial tail / length 非数字 / body 不足)
- Idx sidecar write/recover 单测绿(含 28 非倍数 / idx 领先 log / idx 完全损坏)
- **Write-order 崩溃测试绿**(blocker 2 回归):mock FS 注入 sync 间崩溃点
- Rotate 单测绿(触发 / tail cut / rename / 失败回滚 / 期间 reader 不错)
- 测试基建:crash injection harness + concurrency race harness + merge matrix parametrization
- 所有单测 + `-race` 并发测 + crash 测全绿;代码覆盖率 ≥ 80%

### Phase 2 — EventLog 钩子 + UUID 字段(2 天)

**Done when**:
- `cli.EventEntry` 新增 `UUID string` 字段,`Append` 入口补齐(crypto/rand)
- `discovery.parseHistoryLine` 补 `deriveUUID(line)`:优先用 Claude JSONL 的 uuid,缺时 uuid5(time+summary) 稳定派生
- `cli.EventLog.SetPersistSink(PersistSink)` + `replayPhase atomic.Bool` 实装
- `AppendBatch` 锁外批量 send,`Append` 单 entry packed 成 slice
- **blocker 1 回归**:`persistsink_replay_leak_test.go` 锁契约 — sink 挂前 N 批 entries 不到 sink;sink 挂后首个 batch 到达 sink 时 replayPhase 已是 false
- AST lint `spawn_sink_after_inject_test.go` 绿(session/router.go 内调用顺序)

### Phase 3 — naozhilog.Source + MergedSource(3-4 天)

**Done when**:
- `internal/history/naozhilog`:LoadLatest / LoadBefore / 启动截断 / orphan 清理
- `internal/history/merged`:合并算法按 (Time, UUID) 去重
- 合并矩阵全绿(§6.1 所列 7 条 merge 用例)
- **强建议 1 回归**:连按 "ok" 两次不合并;UUID 相同 local 覆盖
- `BenchmarkMergedSource_Overlap` < 10ms

### Phase 4 — Router 接入(2-3 天)

**Done when**:
- `NewRouter` 启动填 persistedHistory 来自 naozhilog,空才 fallback legacy
- spawnSession 挂 sink(AST 契约测试锁顺序)
- ResetChat / Reset / Remove / Cleanup / scratch 销毁 → DropKey
- 启动扫 orphan(> 30 天)
- 端到端回归:dashboard 发图切换回来图在;重启服务图在;NFS detect banner 显示
- `go test -race ./... -count=1` 全绿

### Phase 5 — 可观测性 + 文档(1-2 天)

**Done when**:
- 5 个 expvar counter 接线(wiring contract 测试覆盖)
- `/health.eventlog` 字段完整(blocker 3 回归)
- `doctor` 子命令检查 writer_alive / fs_supported / disk_free / replay_leak
- DESIGN.md / CLAUDE.md 状态目录章节同步
- dashboard banner(NFS/overlayfs 检测)

### Phase 6 — GA blocker:`attachment-refcount.md` 子 RFC 落地(5-8 天)

**Done when**:
- 子 RFC 评审通过
- attachment meta schema 扩展 + 迁移兼容
- event log Append 触发 refcount 登记
- GC 策略切换到双过期
- session 删除 → refcount 清理
- 端到端:dashboard 发图 → 等 > GC TTL → 大图仍可见(不降级到缩略图)

### 总计

| 阶段 | 本 RFC 工作量 |
| - | - |
| Phase 0 (独立) | 0.5-1 天 |
| Phase 1 (基建 + 测试) | 6-8 天 |
| Phase 2 (UUID + hooks) | 2 天 |
| Phase 3 (Source + 合并) | 3-4 天 |
| Phase 4 (Router) | 2-3 天 |
| Phase 5 (observability) | 1-2 天 |
| **MVP 发布前合计** | **14-20 天** |
| Phase 6 (子 RFC, GA 前置) | 5-8 天 |
| **GA 发布合计** | **19-28 天** |

---

## 8. 简化 PoC 的边界(内部验证用,非 release 路径)

若资源紧,最小化分支:
- 仅持久化 Images/ImagePaths/UUID/Time/Type/Summary/Detail,其他字段砍
- 不做 rotate,单文件上限后停止 Append + slog.Error
- 不做 idx,load 时全文件扫
- 合并简化为 "local 空才 fallback"(不 dedup)
- 不做 race / crash 测试基建,仅 -race

**工作量 3-5 天**,标签 "PoC / 内部验证"。缺点:
- ⚠️ 与完整版 schema 不兼容,升级需迁移
- ⚠️ 升级期合并断层真实出现,需用户手动 `rm -rf events/`
- ⚠️ 无法承接 attachment-refcount 子 RFC

**不进正式 release**。

---

## 9. 仍 Open 的 follow-up(已 vetted,不 blocker)

1. **EntryView 具体形态**:v3 定接口骨架,实现细节(是否支持 flyweight 复用、多字段提取是否合并为 `Snapshot` 返回 struct)留给 Phase 1 实装
2. **Rotate 阈值 100 MiB 的 ops 验证**:不同租户场景需实测后微调,RFC 不定死
3. **UUID 字段加入对老测试的冲击**:`cli.EventEntry` 现有单测若硬编码 JSON fixture 可能需更新;Phase 2 时扫一遍
4. **dashboard banner 是否默认出现**:NFS 不一定是误用(家用 NAS 挂载),需运维决策是否默认弹

---

## 10. 验收标准(GA blocker 硬化)

### MVP(Phase 0-5 完成)

1. **功能**:dashboard 发图 → 切 session / 刷新 / 短期重启,缩略图可见
2. **降级**:attachment GC 过期后大图点击降级到缩略图(不 broken image)
3. **大图存活期**:≥ 7 天(当前 attachment GC TTL 背书)
4. **兼容**:`rm -rf ~/.naozhi/events/` 服务继续,老会话历史可见(退回 Claude JSONL)
5. **可观测**:5 个 counter + /health.eventlog 段 + doctor 子命令
6. **性能**:BenchmarkEventLogAppend 持久化 on < 2× off
7. **并发**:-race 套件含 reader-during-write / rotate-during-read
8. **崩溃**:SIGKILL 前后数据截断但完整,无半行被当真;replay_leak_total = 0

### GA(Phase 6 完成)

9. **大图永存**:attachment-refcount 子 RFC 落地;大图在有效引用期内不过期(含 TTL + 引用 双重条件)
10. **产品验收**:用户发图 / 切换 / 重启 / 再回来 / 再点击大图,全流程可用,不降级到缩略图

**MVP 不含验收 #9,GA 必含**。在子 RFC 落地前宣布"图片跨重启可见"GA 就是虚假承诺。

---

## 11. 附录:现状代码锚点

| 事项 | 文件 / 符号 |
| - | - |
| EventEntry 定义 | `internal/cli/eventlog.go:91-139` |
| `persistedHistory` 内存 slice | `internal/session/managed.go:169-171` |
| Claude JSONL 读 | `internal/discovery/history.go` + `history_tail.go` |
| history.Source 接口 | `internal/history/source.go` |
| claudejsonl.Source | `internal/history/claudejsonl/source.go` |
| attachment 写入 | `internal/attachment/store.go:98` |
| attachment GC | `internal/attachment/store.go:205` |
| dashboard 渲染 | `internal/server/static/dashboard.js:2501-2522` |
| dashboard attachment 路由 | `internal/server/dashboard_send.go:853` |
| sessions.json 持久化范式 | `internal/session/store.go` |
| cron_jobs.json 持久化范式 | `internal/cron/scheduler.go` |
| atomic file write | `internal/osutil/atomicfile.go` |
