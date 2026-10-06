# 日志关联字段：trace_id / run_id / session_key

一条 IM 消息从 platform → dispatch → turn → session → cli 跨五层，排查「这条
result 属于哪一轮」时靠时间戳对齐很痛苦（#3322 / #3401 / #3411 一类 cost 归属
问题都是这样查的）。三个关联字段随 `context.Context` 传递，并由
`ctxutil.Handler` 自动附到每条 **带 ctx** 的日志记录上（`slog.InfoContext` 等；
包级 `slog.Info` 不带 ctx，不会有这些字段）。调用处或 `logger.With` 已经写了同名
字段时，Handler 不会重复追加。

| 字段 | 产生位置 | 含义 |
|---|---|---|
| `trace_id` | IM：`dispatch.BuildHandler`；dashboard HTTP：`X-Request-ID`（无则随机）；WS / relay：`turn.Submit` 随机生成 | 一条入站消息。IM 有平台 event id 时为 `<platform>:<event_id>`（重投递共享同一 trace，可直接对平台侧日志 grep），否则随机 16 hex |
| `run_id` | `turn` 每执行一轮生成一次（owner loop 与 detached 都是） | 一轮 turn。**与 `/api/sessions/runs` 和 cost ledger 里的 `run_id` 相同**：session 的 run record 采用 ctx 里的这个值 |
| `session_key` | 同上 | 这轮跑在哪个会话（`feishu:direct:<chat>:<agent>` 等） |

- 排队合并的多条消息跑成**一轮**：这一轮的 `trace_id` 是批次第一条消息的，
  每轮开头的 `turn: start` 行用 `trace_ids` 列出批次里每条消息的 trace（最多 8 个），
  按某条消息的 trace 找到它被合进了哪个 `run_id`。批次里每条消息的
  `message replied` 行也带头一条的 `trace_id`（回复跑在这一轮的 ctx 上），
  所以跟进消息的回复行要先由 `turn: start` 找到 `run_id`，再按 `run_id` 查。
- leaked-toolcall 的自动续发是一条**独立的** run record，拿新的 `run_id`；
  `leak-recovery: nudge run` 行的 `nudge_of` 是它续的那一轮。
- cli 层不带 ctx 的 readLoop 日志靠 `run_id` 字段对齐：passthrough 的 slot 记下发送方的
  `run_id`，`passthrough: fanout`（`run_id` 是 head，`merged_run_ids` 是合并进来的其余各轮）、
  `passthrough: slot orphaned` 等行带它；legacy `Send` 把它记在进程的 turn 状态里，
  `eventCh full, dropped result` 带当前那一轮。发送方放弃（ctx 取消、bail 超时、interrupt）之后
  才到的 result 按 `unowned:` 记账，账本里的 `RunID` 与这一轮不同，`cli: abandoned run's result
  booked as unowned` 行的 `run_id` 就是它属于的那一轮，据此把迟到的花费对回 run 记录。
- 不经过 turn 的入口（cron、sysession）由 session 自己生成 `run_id`，日志里没有
  这三个字段。

## 用法

```bash
# 从 dashboard 的 run 列表拿到 run_id，拉这一轮所有日志
journalctl -u naozhi -o cat | jq -c 'select(.run_id=="0123456789abcdef")'

# 一条消息从收到到它被合进的那一轮的 turn: start（跟进消息的回复行再按这里的 run_id 查）
journalctl -u naozhi -o cat | jq -c 'select(.trace_id=="feishu:om_xxx" or (.trace_ids // [] | index("feishu:om_xxx")))'

# 某个会话今天的全部轮次
journalctl -u naozhi --since today -o cat \
  | jq -r 'select(.session_key=="feishu:direct:ou_1:general" and .msg=="turn: start") | .run_id'
```

`log.format: text` 时字段以 `key=value` 形式出现在行尾，`grep run_id=...` 即可。

## 覆盖范围（当前）

已带 ctx 的日志：dispatch 的入站 / 拒绝 / 限流 / 预算 / 回复 / 出错各行（含
`turn ended in failure`、分片 / 图片发送失败、banner 编辑失败，以及 reply tracker 的
ask_question / todo / 状态编辑失败），turn 的 `turn: start` / `turn: processing
queued messages` / interrupt / `turn: panic recovered`，以及 leak-recovery 的
nudge 行。其余包级 `slog.*` 调用迁移到 `*Context` 变体即可获得字段，不需要改
handler。
