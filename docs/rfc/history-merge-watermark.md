# RFC: 历史合并去重——水位切分为何不能照字面做

状态：Draft，**待决策**
关联：Epic F #2544 方向 3；事故 #2406（消息双显）；Epic C #2526（EventEntry 版本化）

## 1. 现状

`internal/history/merged` 把 local（naozhilog，naozhi 自己的 event log）与 fallback（claudejsonl，Claude CLI 的 transcript）合并。两层用**不同的 UUID** 描述同一个 turn——naozhi 盖 `crypto/rand` UUID，Claude JSONL 带 Claude 自己的 message uuid——所以除了 UUID 精确匹配，还要靠内容配对：

- `contentKey(e)` = `(Type, Detail)`（或图片的 SHA-256），**不含 Time**
- 三个经验毫秒常数（`source.go:135-144`）：
  - `contentSkewLeadMS = 3000`（user 方向，实测冷启动 ~1391ms，给 2× 余量）
  - `contentSkewLagMS = 500`（assistant 方向，实测 19ms）
  - `contentSkewEpsilonMS = 250`（逆因果方向的 ms 取整余量）
- `pairBucket` 是 O(N·M) DP，求"先最大基数、再最小总 |Δtime|"的一对一配对

Epic F 的方向 3 提议用**水位切分**替代：

> eventlog 对某 key 一旦有记录，`[minLocalTime, ∞)` 只信 local，fallback 仅填空窗

## 2. 为什么照字面做会丢消息

**前提不成立：local 层可以在其覆盖范围内部有洞，而且洞在磁盘上不可见。**

`internal/eventlog/persist/persister.go:485-495`：

```go
select {
case p.in <- job:
default:
    putEntryArena(arena)
    p.droppedCnt.Add(int64(len(entries)))
    p.opts.Observer.OnDrop(len(entries))
    slog.Warn("event log persist: channel full; dropping batch", …)
```

- ingest channel 满时**整批丢弃**，不阻塞（`DefaultChannelBuffer = 4096`）。触发条件是瞬时突发溢出或写者卡死（`channel_used` 字段就是为区分这两者加的，#1184）。
- 丢弃只记**内存**计数（`droppedCnt atomic.Int64`），经 `/health` 的 `Dropped` 字段与一条 `slog.Warn` 暴露。进程重启即归零。
- **日志文件里没有任何标记。** 读 `events/<key>/*.jsonl` 的人无法分辨"这个 key 在这段时间没有消息"和"这里丢了一批"。

于是：若某 key 在会话中途丢过一批，该范围**两侧都有 local 记录、中间是洞**。"`minLocalTime` 之上只信 local"会把那些消息**永久隐藏**——而 fallback（Claude JSONL）里它们是在的。

这正是 #2406 前三版的失败模式：**"修好显示但丢消息"**。当前的内容配对之所以啰嗦，恰恰因为它是**逐条**判定的：一条 fallback 记录只有在找到具体配对时才被丢弃，没配上就保留。水位切分是**按范围**判定的，范围内的未配对项一律丢弃——把"保守保留"换成了"乐观删除"。

## 3. 选项

### A. 落盘 gap marker（水位切分的前置条件）

丢弃发生时在日志里留痕，读者据此知道某段区间不可信。

实现要点：丢弃发生在**生产者**侧且必须不阻塞（channel 已满，往同一个 channel 塞 marker 只会一起被丢）。所以：生产者只置一个 atomic 计数；**写者**在下一次成功写入时先吐一条 gap 记录，内容为"自上次写入以来丢了 N 条"。时间范围因此是"上一条已写记录的 ts 到下一条的 ts"之间——这正好是读者需要的粒度。

- 代价：EventEntry schema 变更（属 Epic C #2526 的版本化范畴）；读侧三处要认识新记录类型。
- 收益：**唯一**能让水位切分安全的路，且顺带把一个今天只在 `/health` 上一闪而过的数据丢失事件变成可追溯的。
- 风险：写者若在吐 marker 前就死了，marker 丢失。可接受——那种情况下文件末尾本来就是截断的，读者已有 crash-recovery 路径。

### B. 保留内容配对，只用水位缩小它的输入

水位不用来决定"信谁"，只用来决定"哪里需要配对"：local 最大时间之上、最小时间之下都无需配对，只在重叠区跑 DP。

- 代价：≈0，行为逐字节不变。
- 收益：也≈0。DP 的输入本来就 ≤ 一页 `limit` 条，不是性能问题。三个常数一个都删不掉。
- 判定：**不值得单独做**。

### C. 让两层共享主键（Epic F 方向 3 的后半句）

评估 stream-json 能否回传 CLI 的 uuid，使 naozhi 不再盖 `crypto/rand` UUID。

- assistant 侧：CLI 的 stream-json 事件带 message uuid，可行性高。
- user 侧：naozhi 是发送方，消息在到达 CLI 之前就已入 local 日志——**这正是 1391ms 偏斜的来源**。没有 CLI 分配的 id 可用，除非等 CLI 回显。
- 收益：能把内容配对**从 assistant 方向彻底移除**（`contentSkewLagMS`、一半的 `Epsilon` 随之消失），只剩 user 方向。这是三个常数里唯一有实测双位数上界的方向。
- 判定：**独立于 A 且有价值**，可先做。

## 4. 建议

1. **先做 C**：assistant 侧改用 CLI uuid，删掉 `contentSkewLagMS`。这条不依赖任何 schema 变更，且把问题面积减半。
2. **A 是水位切分的前置**。在 gap marker 落盘之前，验收标准"删除 3 个经验毫秒常数"**不能安全达成**——`contentSkewLeadMS` 是 user 方向唯一的兜底。
3. **B 不做。**

因此 Epic F 方向 3 不是"3-4 人日的替换"，而是"C（小）+ A（schema 变更，属 Epic C）+ 之后才是水位切分"。这需要 scope 决策，不应在重构中顺手改掉——把逐条保守保留换成按范围乐观删除，是 #2406 已经教过一次的代价。

## 5. 若采纳，必须有的回归

- 构造 local 中途缺一批、fallback 完整的场景，断言合并结果**不丢**那批消息。今天没有这个测试；`source_timeskew_test.go`(741 行) 与 `source_test.go`(712 行) 覆盖的是偏斜与配对基数，不覆盖"local 有洞"。
- #2406 原场景（消息双显）继续不复现。
- gap marker 的写入不得让丢弃路径阻塞——丢弃路径不阻塞是它存在的全部理由。
