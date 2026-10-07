# RFC: Outbound webhooks —— run 生命周期事件推送

- Issue: #3448（a 部分；b 部分「naozhi 作为 MCP server」另开 RFC）
- Status: Phase 1 随本 RFC 落地

## 1. 问题

naozhi 只有入站接口（IM / dashboard / REST）。cron job 失败、sysession daemon 跑完，
除了 dashboard WS 没有任何出站通知；CI、告警系统、下游自动化拿不到这些事件。而
`runtelemetry.Broadcaster` 已经是 cron / sysession run 生命周期的统一 seam，当前唯一
实现是 `server.hubBroadcaster`（推 dashboard WS）。

## 2. 目标 / 非目标

目标（Phase 1）：

1. 配置若干 webhook 端点，按事件类型订阅 `run.started` / `run.ended`
2. 事件来源 = Broadcaster 已承载的 cron + sysession run；HMAC-SHA256 签名；
   有界队列 + 指数退避重试；永不阻塞生产者
3. 不改 Broadcaster 接口、不改 cron/sysession；Hub 与 webhook 通过一个 `Tee` 并列

非目标：

- **session turn 结束事件**：IM / dashboard 的普通会话 turn 不走 Broadcaster（它只有
  cron/sysession 两个 Subsystem，`SubsystemSession` 常量存在但无生产者）。接入需要
  在 `turn.Orchestrator.deliver` 或 `ManagedSession.finishRun` 发事件，并决定 payload
  里能带什么（prompt / result 文本是用户内容，默认不应出站）。另开 slice
- 事件内容：payload 只带 run 元数据（ids、状态、时长、error_class），不带 prompt、
  result、`ErrorMsg`（它可能回显 prompt 片段，Broadcaster 的 SECURITY 注释已说明）
- 动态注册（API 加 webhook）、重放、死信持久化：Phase 1 内存队列，进程重启丢失未发出的事件

## 3. 设计

### 3.1 配置

```yaml
integrations:
  webhooks:
    - url: https://ci.example.com/hooks/naozhi
      events: [run.ended]            # 默认两者都订阅
      secret: ${NAOZHI_WEBHOOK_SECRET}   # 可选；设了就签名
      subsystems: [cron]             # 可选；默认 cron + sysession
      timeout: 10s                   # 默认 10s
```

校验：url 必须 http(s) 且非空；events ⊆ {run.started, run.ended}；subsystems ⊆
{cron, sysession}；secret 不得含未展开 `${VAR}`。`http://` 非 loopback 地址给 warn
diag（明文传签名无意义），不报错。

### 3.2 `internal/webhook` 叶子

```go
type Endpoint struct{ URL, Secret string; Events, Subsystems set; Timeout time.Duration }
type Event struct {
    Type, Subsystem, OwnerID, RunID string
    State, Trigger, ErrorClass string
    StartedAt, EndedAt time.Time; DurationMS int64
    SessionID string
    Node string          // workspace.id，接收方区分多节点
}
type Sender struct{ ... }
func New(eps []Endpoint, opts ...Option) *Sender
func (s *Sender) Deliver(ev Event)   // 非阻塞：入有界队列，满则计数丢弃
func (s *Sender) Close(ctx)           // 等队列排空或 ctx 到期
```

- 每个 endpoint 一个 goroutine + 一条 256 深队列：慢端点不拖慢快端点
- 请求：`POST`，`Content-Type: application/json`，头 `X-Naozhi-Event: run.ended`、
  `X-Naozhi-Delivery: <run_id>-<type>`（幂等键）、`X-Naozhi-Signature: sha256=<hex HMAC(body)>`
- 重试：2xx 成功；408/429/5xx 和网络错误重试，退避 1s → 2s → 4s（各加 0–25% 随机抖动，关停时中断），共 3 次；
  其它 4xx 不重试（接收方拒绝）
- 指标：`naozhi_webhook_delivered_total{key=endpoint#}`、`_failed_total`、`_dropped_total`
  （endpoint 用序号不用 URL，URL 可能含 token）
- 日志只记 `scheme://host` 与完整 URL 的短哈希（`url_id`），不记 userinfo、path、query

### 3.3 接线

`runtelemetry.Tee(a, b Broadcaster) Broadcaster` 把两个 Broadcaster 串成一个。
`server.buildDashboard` 处 `runTelemetry.Bind(hubBroadcaster)` 改为
`Bind(Tee(hubBroadcaster, webhookBroadcaster))`，webhook 为 nil 时退回原样。
`webhookBroadcaster` 把 `RunStartedEvent` / `RunEndedEvent` 映射到 `webhook.Event`
并调 `Deliver`。Sender 由 main 构造并挂进 `ServerOptions.Webhooks`；shutdown 序列里
在 `scheduler.Stop` 之后、HTTP drain 之前 `Close`（最后一批 run.ended 还要发出去）。

## 4. 不变量

- `Deliver` 永不阻塞调用方（Broadcaster 契约："MUST NOT call back into the producer"，
  且在 cron 的锁外调用，但仍不能让 HTTP 延迟进入调度器）
- payload 不含用户内容字段
- 未配置 `integrations.webhooks` 时，`Bind` 的参数与现在完全相同

## 5. 测试

- webhook：httptest 服务端收到签名正确的 body；5xx 后重试成功；4xx 不重试；
  队列满丢弃并计数；Close 排空；慢端点不阻塞另一端点
- runtelemetry：Tee 两边都收到；一边 nil 等价于另一边
- config：校验矩阵
- server：配置两个 endpoint 时 Bind 的是 Tee；无配置时仍是 hubBroadcaster（类型断言）
