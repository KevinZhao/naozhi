# Reverse-Connect Protocol (wire format)

> **Status: 已实现**
>
> 反向连接 (NAT 穿越) 的 WebSocket 帧格式。实现位于 `internal/node/protocol.go`
> (`ReverseMsg`)，服务端 dispatcher 位于 `internal/node/reverseserver.go`，
> 子节点 (发起方) 位于 `internal/node/reverseconn.go`。
>
> 架构层面的设计动机见 [../design/multi-node-design.md](../design/multi-node-design.md)。
> 本文档只描述 wire 层约定，供想新增 reverse-only 能力的实现者参考。

## 1. 当前 wire 版本

- `ReverseMsg.ProtocolVersion` 的**隐式当前版本是 1**。
- 仅在 `type=register` 握手帧上设置；其他帧省略即可（`omitempty`）。
- 对端不发 `protocol_version` 字段时，按 **v1 + 空能力集** 处理。
- 只有帧结构发生不兼容变动（重命名字段、调换语义）时才 bump；
  纯加法式演进（新 `type`、新 optional 字段）继续 **v1 + omitempty**。

## 2. Capabilities

`ReverseMsg.Capabilities []string` 由子节点在 register 握手上申报，形如：

```json
{
  "type": "register",
  "protocol_version": 1,
  "node_id": "ec2-dev",
  "capabilities": ["gemini", "acp", "askuser"]
}
```

服务端当前识别的 capability tag：

| tag        | 含义                                                             |
|------------|------------------------------------------------------------------|
| `acp`      | 节点侧支持 ACP (JSON-RPC) 后端，可以处理 kiro / gemini-cli session |
| `gemini`   | 节点侧配置了 gemini-cli 可执行文件                               |
| `askuser`  | 节点侧会生成 `AskUserQuestion` 富事件 (Round 208+)              |

新增 tag 只需同步更新本表和服务端的 known-set。

## 3. Unknown capability 处理 (RNEW-ARCH-402 consumer, Round 213)

服务端 **MUST NOT** fail-close 任何未识别的 capability 字符串。具体行为：

1. 把对端申报的 `Capabilities` 与服务端已知集做交集，得到"可启用"集。
2. 对差集 (对端有但服务端不认识) 记 **WARN** 级日志，一次即可：
   ```
   reverse: peer advertised unknown capability
     node_id=ec2-dev cap=future-feature
   ```
3. 未知 cap 一律当作"未启用"处理，**不影响** register 握手成功。

这样新子节点 ↔ 旧主节点 (未认识新 cap) / 旧子节点 ↔ 新主节点 (旧方没申报
新 cap) 都可以无感共存，不必 flag-day 升级。

## 4. 与 DESIGN.md 的关系

- `DESIGN.md` 里的 "Phase 6: Multi-Node" 描述**用户视角**的功能。
- `multi-node-design.md` 描述**组件关系**与 NodeClient / ReverseNodeConn 职责。
- 本文档只约束**帧字段**，是三者中唯一允许被其他实现 (如非-Go peer) 直接引用的层。
