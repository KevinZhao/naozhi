# RFC: cli/EventLog 文件分拆(ARCH-EVENTLOG-SPLIT)

> **状态**: Implemented (v1.1 — 单 commit move-only 落地,build/vet/test-race 全绿,符号集 63 项零增减)
> **作者**: naozhi team
> **创建**: 2026-05-29
> **修订**: 2026-05-29(v1.1: 落地实施记录,见 §0)
> **范围**: `internal/cli/eventlog.go`(2289 行) → 按职责拆成 6 个文件;**零语义改动**
> **关联代码**: `internal/cli/eventlog.go`、`internal/cli/event.go`、`internal/session/eventlog_bridge.go`、`internal/eventlog/persist/persister.go`
> **先例**: `docs/rfc/process-split.md`(ARCH-PROCESS-SPLIT,Implemented v3)、`docs/rfc/managed-session-split.md`(ARCH-MANAGED-SPLIT,Implemented v2.3) —— **同一手法,同一约束清单**
> **解锁**: R67-PERF-1(ReadEvent/Append alloc 热路径优化的阅读瓶颈)、未来 `EventReader` / `EventWriter` facet 接口拆分

---

## 0. 修订历史

### v1.1(2026-05-29)— 落地实施记录(Implemented)

`internal/cli/eventlog.go` 2289 行 → 6 文件,**单 commit move-only**(对齐 `674055fa` managed.go 拆分的 `[move-only]` 手法,避免 process-split v3 的"中间 commit 不可 build"问题)。落地后行数:eventlog.go 287 / agents 693 / append 456 / query 365 / persist 300 / subscribe 260。

- **符号集零增减**:`grep -hoE '^(func|type|const|var) …'` 拼接 6 个生产文件与原文件 diff 为空(63 项一致),func 总数稳定 47、`*EventLog` 方法稳定 39。
- **测试零改动**:eventlog 无 source-introspecting 测试(§3 预判正确),7 个 `eventlog_*_test.go` 全走导出 API,搬迁后不改一行。这是本拆分比 managed-split(被 2 个 `os.ReadFile` 测试证伪)更干净之处。
- **import 自动收窄**:`goimports -w` 逐文件裁剪;留守 eventlog.go 从 6 import 降到 3(`sync`/`sync/atomic`/`clievent`)。
- **验证**:`go build ./...` + `go vet ./internal/cli/...` + `go test -race ./internal/cli/...` 全绿;下游消费者 `session` / `eventlog/persist` / `server` build+vet+test-race 全绿。
- **实施手法**:用 Python splitter 按"anchor 行 + 前导注释块"切分,先 assert 无损重建原文件再写出,规避 managed-split 教训②(sed 行范围错位漏搬闭合括号)。

---

## 1. 动机

`eventlog.go` 是当前主仓库**全仓最大的单一非测试 Go 文件**(2289 行),且仍是单体:

```bash
$ find internal cmd -name '*.go' -not -name '*_test.go' | xargs wc -l | sort -rn | head -3
   2289 internal/cli/eventlog.go     # ← 本 RFC 目标
   1949 internal/cron/runstore.go
   1873 internal/dashboard/session/handlers.go
```

它在一个文件里承载了 6 条相互独立的职责:

1. **核心结构** —— `EventLog` struct(双锁 + 9 个 atomic 字段)、ring buffer、构造器
2. **写入路径** —— `Append` / `AppendBatch` / `appendBatch` / ring buffer 驱逐 + summary 缓存更新
3. **持久化挂钩** —— `PersistSink` / `PersistSinkOne` 契约、`SetPersistSink*`、replay-phase 守卫
4. **subagent 追踪** —— `applyEntryStateLocked`(304 行,全文件最大方法)、task_done 回调、O(1) sidecar 索引
5. **订阅广播** —— `subscriber` / `EventSubscription`、`Subscribe*` / `notifySubscribers` / `CloseSubscribers`
6. **读取查询** —— `Entries*` / `EntriesSince*` / `EntriesBefore*` / `Last*Summary` / `Count`

这 6 块的演进节奏完全不同(持久化跟 RFC event-log-persistence 走;subagent 追踪跟 agent-team RFC 走;订阅跟 dashboard WS 走),却被焊在同一文件里,导致:

- 任何一块的 diff 都要在 2289 行里定位,review 噪声大
- 性能项(R67-PERF-1 的 Append alloc、R220/R233B/R239 的多处 PERF 注释)阅读上下文被淹没
- 后续 `EventReader`/`EventWriter` facet 接口拆分缺少清晰的方法分组锚点

**与 process.go 的关系**:`process.go` 的同类拆分(ARCH-PROCESS-SPLIT)已于 2026-05-11 落地为 7 文件(`process.go` 932 + `process_readloop.go` 889 + `process_send.go` 496 + …),本 RFC 是其姊妹工作,沿用完全相同的手法与 phase 纪律。

---

## 2. 目标与非目标

**目标**:
- 把 `eventlog.go` 按上述 6 职责轴拆成 6 个文件,**零方法体内容改动**(纯搬迁 + import 收窄)
- 每个 phase commit 独立 `go build` + `go vet` + `go test -race ./internal/cli/...` 全绿
- 顶层 func 计数前后稳定(见 §6 可机械复现的核对命令)

**非目标**(本 RFC 明确不做,留给后续轮次):
- **不**改任何方法签名 / receiver / 锁粒度
- **不**抽接口(`EventReader` 等 facet 拆分是 §9 的后续项)
- **不**动 `EventLog` struct 字段布局(ring buffer 预分配 maxSize 的内存契约不变)
- **不**改 `EventEntry`(它是 `clievent.EventEntry` 的 alias,真身在 `event.go`)

---

## 3. 现状盘点(可机械复现)

```bash
$ grep -c '^func ' internal/cli/eventlog.go
47                       # 顶层 func 总数
$ grep -cE '^func \((l \*EventLog|s EventSubscription)\)' internal/cli/eventlog.go
41                       # 方法(EventLog 40 + EventSubscription 2 = 42? 见下)
```

精确分解:**47 个顶层 func = 41 个方法 + 6 个自由函数**。

41 个方法中 `EventSubscription` 占 2 个(`Notify` / `Cancel`),其余 39 个挂在 `*EventLog`。
6 个自由函数:`sanitizeImagesAligned`、`stampUUID`、`NewEventLog`、`entryAffectsAgentState`、`loadAtomicString`、`storeAtomicString`。

**关键利好(对比 managed-split 的 §6 教训)**:eventlog **没有** source-introspecting 测试。

```bash
$ grep -rln '"eventlog.go"' internal/cli/*_test.go
# (空) —— 无测试按文件名读源码断言符号位置
```

managed-split v2.3 落地时被 2 个 `os.ReadFile("managed.go")` 测试证伪"测试零改动";eventlog 的 7 个相关测试(`eventlog_*_test.go` 共 ~3.3K 行)全部走导出 API,搬迁后**无需改任何测试**。这是本 RFC 比前两个拆分更干净的地方。

---

## 4. 目标文件布局

| 文件 | 职责 | 主要符号 | 估算行数 |
|------|------|---------|---------|
| `eventlog.go`(留守) | 核心结构 + 构造 | `EventLog` struct、`EventEntry` alias、全部常量、`NewEventLog` | ~350 |
| `eventlog_append.go` | 写入路径 + ring buffer | `Append`、`AppendBatch`、`AppendBatchReplay`、`appendBatch`、`stampUUID`、`sanitizeImagesAligned` | ~500 |
| `eventlog_agents.go` | subagent 追踪 | `applyEntryStateLocked`、`entryAffectsAgentState`、`recordAgentRingPosLocked`、`SetAgentInternalID`、`TurnAgents`/`Subagents`/`BgSubagents`、task_done 回调五件套、`SubagentInfo`/`subagentRef`/`agentRingPos`/`pendingTaskDone` | ~650 |
| `eventlog_persist.go` | 持久化挂钩 | `PersistSink`/`PersistSinkOne` 类型、`SetPersistSink`/`SetPersistSinkPair`、`invokePersistSink`/`invokePersistSinkOne`、`ReplayInvokeTotal`、`SinkReady` | ~250 |
| `eventlog_subscribe.go` | 订阅广播 | `subscriber`、`EventSubscription`、`eventLogClosedCh`、`Subscribe`/`SubscribeNew`、`notifySubscribers`、`CloseSubscribers`、`Notify`/`Cancel` | ~280 |
| `eventlog_query.go` | 读取查询 | `Entries*`、`LastN*`、`EntriesSince*`、`EntriesBefore*`、`Count`、`Last{Prompt,Activity,Response}Summary`、`LastEventAt`、`UserTurnCount`、`loadAtomicString`/`storeAtomicString` | ~400 |

合计 ≈ 2289(误差来自注释块归属),与原文件持平。

**导出类型搬迁说明**:`SubagentInfo`、`EventSubscription`、`PersistSink`、`PersistSinkOne` 是导出类型,被 cli 包外消费(见 §5)。同包内跨文件移动**不影响**外部引用(它们仍是 `cli.SubagentInfo` 等),无 import 变化。

---

## 5. 跨文件耦合与锁不变性(本 RFC 的核心约束)

拆分后这 6 个文件**同属 `package cli`**,函数调用无 import 问题。但有两类隐式耦合必须在评审时盯死,**它们决定 phase 顺序**:

### 5.1 写路径的扇出调用(append → agents/subscribe/persist)

`appendBatch`(留在 `eventlog_append.go`)在持 `l.mu` 期间调用:

- `applyEntryStateLocked`(→ `eventlog_agents.go`)—— 更新 turnAgents/bgAgents/各 sidecar 索引
- `notifySubscribers`(→ `eventlog_subscribe.go`)—— 在释放 `l.mu` 后、持 `subMu.RLock` 广播
- `invokePersistSink` / `invokePersistSinkOne`(→ `eventlog_persist.go`)—— 持久化扇出

```bash
# 核对写路径的跨文件调用点
$ grep -n 'applyEntryStateLocked\|notifySubscribers\|invokePersistSink' internal/cli/eventlog.go
```

这是**纯函数调用耦合**(同包可见),搬迁后照常工作。但意味着:**`eventlog_append.go` 是其余三个文件的下游**,Phase 排序上应先搬被调用方、后搬 append,或同轮 review。

### 5.2 共享字段的锁不变性(append ↔ agents,经 `l.mu` + atomic)

这是比函数调用更隐蔽的耦合,**与 managed-split §5.2 的 `persistedHistory*` 跨文件不变性同型**:

| 字段 | 写于 | 读于 | 锁 | 不变性 |
|------|------|------|----|--------|
| `entries`/`head`/`count` | append | query | `l.mu` | ring buffer 单调推进 |
| `turnAgents`/`bgAgents`/`taskIndex`/`toolUseIndex`/`agentRingByToolUse` | agents(`applyEntryStateLocked`) | agents(`TurnAgents` 等) | `l.mu` | turn 边界(result/user)整体 reset |
| `turnAgentCount` atomic | agents | query(Snapshot 热路径无锁读) | atomic | 镜像 `len(turnAgents)+len(bgAgents)` |
| `lastPromptSummary`/`lastActivitySummary`/`lastResponseSummary` atomic | append(持 `l.mu`) | query(无锁读) | atomic | last-writer-wins,store 顺序受 `l.mu` 保护 |
| `userTurnCount`/`lastEventAt` atomic | append | query | atomic | 仅 live Append 递增;replay(`isReplay=true`)不动 |
| `sinkReady`/`persistSinkPtr`/`persistSinkOnePtr`/`replayInvokeTotal` | persist(`SetPersistSink*`) | append(`appendBatch` 热路径 Load) | atomic | `SetPersistSink` 必须晚于 `InjectHistory`(replay 守卫) |
| `subscribers`/`subsClosed`/`subCount` | subscribe | subscribe(`notifySubscribers`) | `subMu` | "channel closed exactly once" |

**结论**:`append`(写) 与 `agents`/`persist`/`query`(读) 经 `l.mu` + atomic 字段共享状态,**不是函数调用可见的**。因此:

> **Phase 2/3/4(append / agents / persist)建议同一人连续做或同轮 review**,不承诺这三者可任意并行(沿用 managed-split v2 W4 修正后的保守口径)。subscribe(独立 `subMu`)、query(纯读)与它们解耦,可独立 phase。

### 5.3 cli 包外消费端(确认无 API 改动)

```bash
$ grep -rl 'cli\.EventLog\|cli\.EventEntry\|cli\.SubagentInfo\|cli\.PersistSink\|cli\.EventSubscription' internal/ --include='*.go' | grep -v '_test.go' | grep -v 'internal/cli/'
```

消费端横跨 session / server / dashboard / node / history / sysession / eventlog/persist 等 ~20 个文件(如 `session/eventlog_bridge.go`、`server/wshub_eventpush.go`、`eventlog/persist/persister.go`)。**本 RFC 零 API 改动**,这些消费方一律不受影响(它们引用的是 `cli.X` 包级符号,与符号定义在哪个文件无关)。

---

## 6. 验收核对(凡计数必附可复现命令)

前两个拆分 RFC 的系统性弱点都是"手数表出错"(process-split B5、managed-split D1/D2)。本 RFC 强制每项断言带机械命令:

```bash
# (A) 拆分前后顶层 func 总数必须一致 = 47
for f in eventlog eventlog_append eventlog_agents eventlog_persist eventlog_subscribe eventlog_query; do
  printf '%s: ' "$f"; grep -c '^func ' internal/cli/$f.go 2>/dev/null || echo 0
done | awk -F': ' '{s+=$2; print} END{print "TOTAL="s}'    # 期望 TOTAL=47

# (B) EventLog 方法数稳定 = 39(EventSubscription 2 个单独计)
grep -hcE '^func \(l \*EventLog\)' internal/cli/eventlog*.go | paste -sd+ | bc   # 期望 39

# (C) 每 phase 后三件套全绿
go build ./internal/cli/... && go vet ./internal/cli/... && go test -race ./internal/cli/...

# (D) 确认无测试硬编码文件名(拆分安全前提)
grep -rln '"eventlog.go"\|ParseFile.*eventlog' internal/cli/*_test.go   # 期望空
```

---

## 7. Phase 计划(每 phase 一个 commit,独立 build 绿)

按"先搬叶子、后搬扇出根"的依赖序,且把 §5.2 的强耦合三件套压在相邻 phase:

| Phase | 动作 | 文件产出 | 风险 |
|-------|------|---------|------|
| 1 | 抽订阅广播(独立 `subMu`,与 ring buffer 解耦) | `eventlog_subscribe.go` | 低 |
| 2 | 抽读取查询(纯读,只 RLock `l.mu` + atomic load) | `eventlog_query.go` | 低 |
| 3 | 抽持久化挂钩(atomic 字段,append 是其消费者) | `eventlog_persist.go` | 中 |
| 4 | 抽 subagent 追踪(`applyEntryStateLocked` 大块) | `eventlog_agents.go` | 中 |
| 5 | 抽写入路径(扇出调用 3/4 phase 的方法) | `eventlog_append.go` | 中-高 |
| 6 | 留守清理:`eventlog.go` 只剩 struct + 常量 + `NewEventLog`,清 unused import | `eventlog.go` | 低 |

> **Phase 3/4/5 建议同轮 review**(§5.2 强耦合)。Phase 1/2 可独立先行,作为低风险预热。
> 每个 cut 后几乎必有 unused import,`go build` 即时暴露后逐个清(managed-split 实施教训②③)。
> **不要**用 `git checkout <单文件>` 回退已 cut 的 `eventlog.go`(会恢复成 HEAD 全量、与已抽出文件重复 —— managed-split 教训①);botched cut 须手动重删。

---

## 8. 风险与缓解

| 风险 | 缓解 |
|------|------|
| `appendBatch` 漏搬尾随闭合括号(sed 范围错位) | sed 提取与删除行范围严格一致含尾随空行;每 phase `go build` 兜底 |
| atomic 字段读写跨文件,误判可并行 | §5.2 表 + Phase 3/4/5 同轮 review 约束 |
| `sanitizeImagesAligned` 归属(append 还是留守?) | 它仅服务 Append 的图片对齐,grep 确认无其它调用方 → 归 `eventlog_append.go` |
| commit 中间点不可 build(process-split v3 踩过) | 跨 phase 依赖修复压在同一 commit 内,不留悬空引用 |

---

## 9. 后续解锁(本 RFC 之外)

拆分后形成的清晰方法分组,为以下后续轮次提供锚点:

- **`EventReader` / `EventWriter` facet 接口**:`eventlog_query.go` 的只读方法集天然构成 `EventReader`,`eventlog_append.go` 构成 `EventWriter`,可据此收敛 server/session 对 `*cli.EventLog` 具体指针的依赖(对应 ARCH-CONSUMER-IF 方向)。
- **R67-PERF-1(Append alloc)**:写路径独立成文件后,Append 热路径的 alloc 优化 review 上下文不再被 2289 行淹没。
- **subagent 追踪独立演进**:agent-team RFC 后续改动只动 `eventlog_agents.go`。

---

## 10. 附录 A:方法 → 目标文件完整映射

(按 `grep -n '^func ' internal/cli/eventlog.go` 行号序,供搬迁逐条核对)

```
留守 eventlog.go:     NewEventLog
eventlog_append.go:   sanitizeImagesAligned, stampUUID, Append, AppendBatch,
                      AppendBatchReplay, appendBatch
eventlog_agents.go:   entryAffectsAgentState, applyEntryStateLocked,
                      SetOnAgentTaskDone, OnAgentTaskDone, loadAgentTaskDoneFn,
                      fireTaskDoneCallbacks, fireOneTaskDoneCallback,
                      recordAgentRingPosLocked, SetAgentInternalID,
                      TurnAgents, Subagents, BgSubagents
eventlog_persist.go:  SetPersistSink, SetPersistSinkPair, invokePersistSink,
                      invokePersistSinkOne, ReplayInvokeTotal, SinkReady
eventlog_subscribe.go: Notify, Cancel, notifySubscribers, SubscribeNew,
                      Subscribe, CloseSubscribers
eventlog_query.go:    Entries, LastN, EntriesAppend, LastNAppend, Count,
                      EntriesSince, EntriesSinceAppend, EntriesBefore,
                      EntriesBeforeAppend, LastPromptSummary,
                      LastActivitySummary, LastResponseSummary, LastEventAt,
                      UserTurnCount, loadAtomicString, storeAtomicString
```

类型搬迁:`subagentRef`/`agentRingPos`/`noAgentRingPos`/`SubagentInfo`/`pendingTaskDone` → `eventlog_agents.go`;`subscriber`/`eventLogClosedCh`/`EventSubscription` → `eventlog_subscribe.go`;`PersistSink`/`PersistSinkOne` → `eventlog_persist.go`;`EventEntry` alias + `EventLog` struct + 全部常量 → 留守 `eventlog.go`。
