# RFC: internal/wsproto — WS type 常量 + per-type frame，与 node RPC 分离（#2535 C1）

## 问题

浏览器 WS 与 node RPC 共用 `node.ServerMsg`：15 个字段全 `omitempty` 的
union，`Type` 合法值靠注释；server 侧 21 个 type、87 处裸字面量构造；4 个
cron/daemon 帧各自定义私有 struct 绕开信封；前端 `switch (msg.type)` 21 个
case 按字面量分发。#2474/#2476/#2432/#2406 全是"后端字段语义变了前端不知
道"。入向 `node.ClientMsg` 是同样的 union（12 字段 8 种 type）。

盘点修正两个 issue 前提：
- `test/e2e/mock-server.js` **没有 WS 实现**（/ws 一律 404 逼前端走
  polling），"mock-server 校验自己吐的帧"不可达 → 替换验收见下。
- `relay.go:412-427` 对帧做**原始字节透传**（只 injectNodeField 不重编
  码）：wire 形状本期必须逐字节不变。

## 方案（Phase 1：wire 逐字节不变）

### 1. `internal/wsproto` 包（新叶子，只依赖 internal/cli）

- `type MsgType string` + 全部出向常量（21 个）与入向常量（8 个：auth /
  subscribe / unsubscribe / send / interrupt / ping / agent_subscribe /
  agent_unsubscribe）。
- **每个出向 type 一个平铺 frame struct**（模式即现有 cronRunStartedMsg：
  自带 `Type MsgType \`json:"type"\`` + 该 type 实际使用的字段，json tag
  与现行 wire 完全一致）。构造函数 `wsproto.NewEventFrame(key, ev)` 式，
  Type 由构造函数固定，调用方不再写字面量。
- 4 个私有 `cronRun*/daemonRun*Msg` struct 原样迁入（字段不动——
  daemonRunEnded 特意没有 ErrorMsg 的防泄漏语义保留并写注释）。
- registry：`var Frames = map[MsgType]func() any{...}`（每 type 一个
  exemplar 构造），schema 生成与契约测试共用。

### 2. 与 node RPC 的分离边界

- node RPC 帧 = 既有 `node.ReverseMsg`（自有 ProtocolVersion 握手），不动。
- 浏览器帧 = wsproto frames。`internal/server` 与 `internal/node`
  （reverseconn.go:647-690 的 ReverseMsg→浏览器帧翻译点、relay 的 6 处
  构造）全部改用 wsproto 构造函数。
- `node.ServerMsg` 降级为 **decode-only 兼容视图**（relay 客户端侧解析、
  测试解码用），文档标注禁止新增构造点；彻底删除留给信封收拢
  `{type,payload}` 的后续阶段（前端切常量后，issue 过渡策略第 4 条）。
  `node.ClientMsg` 同理：本期只引 MsgType 常量，字段拆分为后续。

### 3. wire 兼容性证明

（逐字节相同不是 wire 需求——前端 JSON.parse 与 relay 字节透传都与键序
无关——而是工程手段：golden 逐字节对照能机械捕获 ~100 处替换中的任何
笔误。frame struct 字段声明顺序照抄 ServerMsg 即可达成。）

- 每个 type 一条 golden 对照测试（沿用 `TestWSPreMarshalledFrames`
  逐字节模式）：`json.Marshal(wsproto.XxxFrame{...})` ==
  `json.Marshal(node.ServerMsg{...同值})`（或 == 迁移前私有 struct 的
  字节）。填充值覆盖每个字段非零，omitempty 行为逐一钉死。
- relay 字节透传路径不动，天然兼容。

### 4. 裸字面量归零的守护

- 契约测试用 go/parser（AST 而非盲扫正则，否则 `clievent.EventEntry.Type`
  与 `node.ReverseMsg.Type` 的合法字面量会把基线顶到非零）：遍历
  `internal/server`、`internal/node` 非测试源码，凡 `ServerMsg` /
  `ClientMsg` 复合字面量携带 `Type:` 字段者计为违规（迁移完成后浏览器帧
  一律经 wsproto 构造）；`ReverseMsg`、`EventEntry` 显式豁免（不同协议，
  RFC §2 边界）。raw 预编码帧（wshub.go:28-34 的 **5 个** const：
  auth_ok / auth_fail_invalid / pong / not-auth error / rate-limited
  error）改由 wsproto 在包 init 生成 var（无 case 标签/数组长度依赖，
  已核实），golden 覆盖从现有 2/5 补到 5/5。基线 0，增即红。

### 5. schema 生成与两端一致性

- `go generate ./internal/wsproto` 跑包内零依赖 reflect 生成器，产出
  `internal/wsproto/wsproto.schema.json`：每 type 的 properties/required/
  Go 类型映射 + MsgType 枚举全集。生成结果进仓，drift 由契约测试对比
  再生成结果发现。
- 后端契约测试遍历 registry：marshal exemplar → key 集合 ⊆ schema
  properties、required 全出现。
- 递归边界：`AgentMetaPatch`（4 平铺字段）展开；`clievent.EventEntry` /
  `ToolCall` / `AskQuestion` 只输出 `$ref` 类型引用**不展开 25 字段**——
  EventEntry 的形状版本化是 #2496 C3 的范围，此处展开会与其冲突。
- **mock-server 验收替换**：e2e 新增 node 脚本
  `test/e2e/check-ws-contract.mjs`（进 lint-js job）：读同一份
  schema，抽 MsgType 枚举，与 dashboard.js `wsm.onMessage` switch 的
  case 字面量集合对账（正则钉死为 `case ' / case "` 引号紧邻形态，避免
  注释污染；前端消费的 type 必须 ∈ schema 枚举，schema 枚举缺 case 报
  警）。注：`unsubscribed` 前端已有显式 no-op case（dashboard.js
  onMessage switch），对账按 case 集合走即可，无需 optional 标注。

## 替代方案

- 直接收 `{type,payload}` 嵌套信封：一次性切换需前后端同发 + relay 重编
  码，回滚面大；分期后本期零 wire 风险。
- schema 用 invopop/jsonschema：引第三方依赖换标准 draft 支持，但消费方
  只有自家测试与生成器，手写 reflect（~150 行）够用且零依赖。
- 常量放 node 包：node 会成为 server 的协议上游，且 wsproto 需被
  node/server 双向消费，独立叶子包是唯一无环放法（盘点 §9：server→node
  单向，cli 为共同下游）。

## 迁移步骤

1. wsproto 包：常量 + frames + registry + 生成器 + schema + golden 对照。
2. server 21 type 构造点 + node 14 处（relay.go 5、reverseconn.go 直发
   4、经 `broadcastToSubs` 5）逐个换构造函数；
   `reverseconn.go` `broadcastToSubs(key string, out ServerMsg, …)` 形参
   放宽为 `any`（函数体本就 json.Marshal，任意 frame 均可）；4 个私有
   struct 迁入；5 个 raw 预编码帧改 init 生成。
3. 裸字面量契约测试上线（基线 0）。
4. e2e check-ws-contract.mjs 进 lint-js job。
5. （后续 issue/阶段）前端 contract.js（C2）→ 信封收拢 → ServerMsg 删除。

## 验收对照（相对 issue 原文的口径调整）

- "mock-server.js 加载同一 schema 校验" → 替换为 check-ws-contract.mjs
  的前后端 type 对账（已在 issue 评论说明，原因：mock-server 无 WS）。
- "static/*.js 字面量替换为常量" → C2 范畴，本期不做（issue 原文亦标注
  "C2 落地后"）。
- 其余逐条按 issue 验收执行。

## 风险

- ~100 处构造点机械替换的笔误 → golden 对照逐 type 钉字节 + 全量
  wshub/node 测试回归。实现前以当前 HEAD 重跑构造点清单（评审确认本
  RFC 引用的部分行号/计数基于旧盘点，落地以实测为准）。
- raw 预编码帧（auth_ok/pong/两个 error）改 init 生成后字节变化 →
  golden 里与旧字面量逐字节比对。
- reverseconn 翻译点行为漂移 → 现有 reverseconn/relay 测试全绿为过门。
