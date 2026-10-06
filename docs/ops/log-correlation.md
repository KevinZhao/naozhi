# 日志关联字段：trace_id / run_id / session_key

一条 IM 消息从 platform → dispatch → turn → session → cli 跨五层，排查「这条
result 属于哪一轮」时靠时间戳对齐很痛苦（#3322 / #3401 / #3411 一类 cost 归属
问题都是这样查的）。#3436 起，三个关联字段随 `context.Context` 传递，并由
`ctxutil.Handler` 自动附到每条 **带 ctx** 的日志记录上（`slog.InfoContext` 等；
包级 `slog.Info` 不带 ctx，不会有这些字段）。

| 字段 | 产生位置 | 含义 |
|---|---|---|
| `trace_id` | `dispatch.BuildHandler`（IM 入口） | 一条入站消息。有平台 event id 时直接用它（重投递共享同一 trace），否则随机 16 hex |
| `run_id` | `turn.Orchestrator.runTurn` | 一轮 turn。**与 `/api/sessions/runs` 里的 `run_id` 相同**：session 的 run record 采用 ctx 里的这个值 |
| `session_key` | 同上 | 这轮跑在哪个会话（`feishu:direct:<chat>:<agent>` 等） |

## 用法

```bash
# 从 dashboard 的 run 列表拿到 run_id，拉这一轮所有日志
journalctl -u naozhi -o cat | jq -c 'select(.run_id=="0123456789abcdef")'

# 一条消息从收到到回复
journalctl -u naozhi -o cat | jq -c 'select(.trace_id=="om_xxx")'

# 某个会话今天的全部轮次（按 run_id 分组）
journalctl -u naozhi --since today -o cat \
  | jq -r 'select(.session_key=="feishu:direct:ou_1:general") | .run_id' | sort | uniq -c
```

`log.format: text` 时字段以 `key=value` 形式出现在行尾，`grep run_id=...` 即可。

## 覆盖范围（当前）

已带 ctx 的热路径日志：`message received` / `message received (passthrough)` /
`/urgent dispatched` / `message dropped: session busy` / `message replied` /
`im access denied` / `im message rate limited` / `turn: processing queued
messages`。其余包级 `slog.*` 调用逐步迁移到 `*Context` 变体即可获得字段，不需要
改 handler。
