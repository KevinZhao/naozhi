# RFC: IM 用量闸门 —— per-chat 费用预算 + per-user 消息限流

- Issue: #3447（索引 #3453）
- Status: Draft v1（Phase 1 随本 RFC 落地）
- 前置: #3442 已落地（`im_access` 决定「谁能用」）；本 RFC 决定「能用多少」

## 1. 问题

cost ledger（`docs/rfc/cost-ledger.md`）已经按天 append-only 记录每一笔花费，并有
`/api/cost/summary`，但没有任何闸门：

- `turn.Admission` 是唯一准入 seam，而 `dispatch/im_origin.go` 的 `imAdmission`
  注释明示 "never declines"
- `internal/ratelimit` 只用于 login / WS / upload / cron trigger；IM 消息路径没有限流
- `costledger.Query` 只有 `SessionKey / JobID / RunID / Workspace` 过滤，没有 chat 维度

一个白名单内的用户发 `/cron add "@every 1m" ...`，或群里一个人刷屏，就能把
Bedrock 账单打穿，而账本数据已经在手。

## 2. 目标 / 非目标

目标（Phase 1，本 RFC）：

1. 每个 IM chat 一个滚动 24h 的 USD 预算；超限后新消息不再进 CLI
2. 每个 IM 用户一个 token-bucket 消息限流
3. 两者都只在配置后生效；不配置 = 行为零变化
4. 不新增 `Router` 方法，不动 `turn.Admission` 语义

非目标（后续 issue）：

- per-user 费用预算：账本条目只有 `SessionKey`，群聊里一个 turn 可能是多个用户的合并
  prompt，要先把 `UserID` 归属穿过 `turn.Request` 写进 `Entry`，另开 RFC
- credits / tokens 单位（kiro metering）折算成 USD：`RateBook` 已存在，但折算规则需
  产品决策；Phase 1 只对 `UnitUSD` 求和
- cron job 的预算：cron 已有数量配额（每 chat 10 / 全局 50），花费配额另议
- dashboard 的 per-chat 费用页：`GroupByChat` 可以顺手加，但不在本 RFC 的闸门范围

## 3. 设计

### 3.1 chat 维度不需要改账本 schema

IM 会话 key 固定为 `{platform}:{chatType}:{chatID}:{agentId}`（`internal/sessionkey`），
chat key 是去掉最后一段的前缀。因此 `costledger.Query` 增加一个 `ChatKey` 过滤：
`e.SessionKey` 以 `chatKey + ":"` 开头即命中。纯字符串操作，costledger 仍是叶子。
它命中同一 chat 下所有 agent 的会话（general / review / planner 绑定的项目会话除外，
planner key 是 `project:<name>:planner`，不属于任何 chat —— 这是已知缺口，见 §6）。

### 3.2 `internal/imbudget` 叶子包

```go
type Policy struct {
    PerChatUSD float64      // 0 = 关闭
    WarnRatio  float64      // (0,1)，默认 0.8
    Action     Action       // "block" | "warn"
    Window     time.Duration // 默认 24h
}

type SpendFunc func(chatKey string, from, to time.Time) (usd float64, err error)

type Gate struct { ... }
func New(p Policy, spend SpendFunc, opts ...Option) *Gate

type Decision struct {
    Spent, Limit float64
    Block        bool // Action==block 且 Spent >= Limit
    Warn         bool // 本次首次越过 WarnRatio（每 window 只报一次）
}
func (g *Gate) Check(chatKey string, now time.Time) Decision
```

- `SpendFunc` 由 server 装配时用 `ledger.Summarize(Query{ChatKey, From, To, GroupBy: unit})`
  实现并取 USD 桶；imbudget 自身不 import costledger，测试用 fake
- **缓存**：每个 chat 的 spent 最多每 `refresh`（默认 15s）从账本刷一次。一个 turn 的
  花费只在 turn 结束后入账，所以闸门天然是「事后」的：超限用户最多再多跑 15s 内
  已经放进去的消息。这是接受的误差；把 refresh 设成 0 可以每条消息都查账本
- cache 有上限（1024 个 chat，LRU 按 asOf 淘汰），和 `denyThrottle` 同款防止被刷爆
- 账本不可用（`SpendFunc` 返回 err）→ fail-open 放行并计数，日志 Warn 一次/分钟。
  预算是成本控制，不是安全边界；安全边界是 `im_access`

### 3.3 per-user 限流

复用 `internal/ratelimit.Limiter`，key `platform + "\x00" + userID`。空 `UserID`
的消息不限流（它们已经被 `im_access` 的 `ReasonEmptyUser` 拒绝，除非平台完全开放）。

### 3.4 闸门位置

`dispatch.prepareInbound` 的顺序变为：

```
dedup → 群聊 @mention → authorize(im_access) → [user rate limit]
      → dispatchCommand → agent 解析 → [chat budget] → 记账/建 turn
```

- 限流在命令之前：它防的是刷屏，`/help` 刷 100 次也该被限
- 预算在命令之后：超限的用户必须还能 `/stop`、`/new`、`/model`，否则无法自救；
  预算只拦「会进 CLI 的消息」
- 都放在 `prepareInbound` 而不是 `turn.Admission`：Admission 的 decline 语义是
  `AckShuttingDown`，而且 queued 消息合并进下一轮时不再经过 Admit，在那里拦不住

### 3.5 回复与观测

- block：私聊回复「本会话 24 小时内的费用预算已用尽（$X / $Y），请联系管理员或等待
  额度恢复。」；群聊同样回复（这是名单内的人，不存在被刷的问题），但按 chat 每
  10 分钟最多一次，复用 `denyThrottle`
- warn：越过 `warn_ratio` 时发一条「本会话已使用 $X / $Y」，每个 window 一次
- 限流命中：静默丢弃 + Debug 日志（回复本身就是对刷屏的奖励）；每用户每 10 分钟
  最多回一次「发送过快，请稍候」
- 指标：`naozhi_dispatch_budget_block_total`、`naozhi_dispatch_budget_warn_total`、
  `naozhi_dispatch_ratelimit_total`、`naozhi_dispatch_budget_ledger_error_total`
- `im access denied` 同款 Info 日志行，带 `chat` / `spent` / `limit`

### 3.6 配置

```yaml
im_limits:
  per_chat_daily_usd: 20      # 滚动 24h；0 或省略 = 关闭
  warn_ratio: 0.8             # 默认 0.8
  action: block               # block | warn（warn = 只提醒不拦）
  user_rate:
    per_minute: 10            # 0 或省略 = 关闭
    burst: 5                  # 默认 = per_minute
```

校验：`per_chat_daily_usd >= 0`、`0 < warn_ratio < 1`、`action ∈ {block, warn}`、
`per_minute >= 0`、`burst >= 1`。配置了预算但 `cost.enabled: false`（账本关闭）→
`config check` 报错，因为闸门永远读到 0。

### 3.7 热重载

`Dispatcher.SetBudgetGate(*imbudget.Gate)` / `SetUserLimiter` 走 `atomic.Pointer`，
和 `SetAccessPolicy` 同款，为 #3437 预留替换点；本 RFC 不做重载入口。

## 4. 不变量

- 不配置 `im_limits` 时，`prepareInbound` 的调用图与现在逐字节相同（两个 nil 检查）
- 预算只读账本，不写；闸门拒绝的消息不产生任何 `Entry`
- 闸门拒绝不消耗 dedup 之外的任何状态（不建 session、不进 queue）
- costledger 仍是叶子（`TestPackageIsLeaf`）；imbudget 只 import stdlib

## 5. 测试

- imbudget：fake SpendFunc；block / warn-once / 缓存 TTL / fail-open / cache 上限
- costledger：`ChatKey` 过滤命中同 chat 所有 agent、不命中前缀相似的别的 chat
  （`feishu:direct:a` 不能命中 `feishu:direct:ab:general`）
- dispatch：预算满时 `/stop` `/help` 仍可用、普通消息被拒且 `messageCount` 不增；
  限流命中时 `/help` 也被拦；未配置时 prepareInbound 行为不变
- config：校验矩阵 + example 文件可加载

## 6. 已知缺口

- project-bound chat 的 planner 会话 key 是 `project:<name>:planner`，其花费不计入
  任何 chat 的预算。修法是在 `Entry` 上多记一个 `ChatKey`（owner 写账时从 origin
  带过来），与 per-user 预算一起做
- 并发：两条消息同时过闸门都看到 spent < limit，都放行。预算是软上限，接受
- 滚动 24h 而不是自然日：没有时区问题，也没有午夜 cliff；文案写「24 小时内」
