# RFC: EventEntry 显式 schema 版本（#2496 步骤 2）

## 问题

`clievent.EventEntry`（25 字段）同时是内存 ring、磁盘持久化、dashboard WS、
跨节点 RPC、四个 history source 的载体。持久化侧已有
`eventlog/schema.WireVersion`（Record.V + MinReadVersion），但：
- EventEntry 自身没有版本常量，WireVersion 的 bump policy 口头引用它；
- 跨节点 RPC 的 `ProtocolVersion` 只管 register 握手 framing，payload 里的
  EventEntry 语义变化无信号——v0.0.80 远端与 v0.0.82 主节点交换时静默。

（步骤 1——删 `cli.EventEntry` alias——在本 RFC 前已完成：`cli.EventEntry`
引用为 0，`internal/cli` 扇入 192 处/22 包 → 20 包，消费方全部直接 import
`clievent`。）

## 方案

版本属于**载体/流**，不属于每条 entry（per-entry V 在 wire/磁盘上是纯冗余）：

1. `clievent.SchemaVersion = 1` + `SchemaCap = "evententry.v1"`：语义版本的
   单一定义点，bump policy 与 WireVersion 相同（additive omitempty 不 bump）。
2. 持久化：沿用既有 `Record.V`/`WireVersion`；新增 allowlist 联动测试
   `{WireVersion: SchemaVersion}`——任一 bump 未同步记录配对即红。
3. 跨节点：register 握手的 Capabilities（既有机制，unknown tag 只 WARN 不
   拒——天然混版本兼容）恒定携带 `SchemaCap`；主节点 `knownServerCaps`
   认识它。**判定是单向的**：EventEntry 的节点间流向只有远端→主
   （fetch_events / fetch_discovered_preview 的 RPC Result），主节点收
   register 后可用 `HasCap(SchemaCap)` 判定远端语义版本——这条方向今天
   够用。反向（远端判定主节点版本）没有信号：`registered` ack 不带
   Capabilities；如未来 v2 需要远端先降级，须给 ack 补对称字段（留作
   bump 时的一部分，本期不做）。
4. 联动测试三处：schema 配对、derivedCaps 恒含 tag、knownServerCaps 认识
   tag——三个常量互相锁死。

## 替代方案

- 每条 EventEntry 加 `V` 字段：25 字段 union 再 +1，全部磁盘行、全部 WS
  帧膨胀，而版本在一次连接/一个文件内恒定——放 header/handshake 更准确
  （持久化已经这么做了，Record.V）。
- ReverseMsg 加 per-frame entry_schema：同上，per-frame 冗余；capabilities
  是既有的连接级协商面。
- 现在就写 migrator：v2 尚不存在，migrator 无输入形态可写；本 RFC 先补
  "可判定"，migrator 随第一次真实 bump（策略：MinReadVersion 窗口 +
  fallback 到 CLI JSONL source，schema.WireVersion 注释已定此路线）。

## 验收

- clievent.SchemaVersion / SchemaCap 存在，三处联动测试全绿；
- 任一常量单独 bump → 至少一处测试红；
- 混版本兼容：旧节点（无 tag）注册照常（unknown-cap 仅 WARN 机制既有
  测试覆盖）。
