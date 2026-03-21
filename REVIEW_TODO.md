## 待讨论问题（来自 2026-03-21 代码审查）

以下问题方案不明确，需产品/架构决策后再修复。

---

### C-1 [CRITICAL] 无用户授权校验

**文件**: `cli/protocol_claude.go:20`, `server/server.go`

`--dangerously-skip-permissions` 硬编码在每个子进程调用中，任何 IM 平台用户发送消息就能触发完整 CLI 权限（读写文件、执行命令、网络请求），无白名单、无角色检查。

**待讨论**:
- 是否需要 `allowed_users` / `allowed_chats` 配置字段？
- 白名单粒度：用户级 vs 频道级 vs 两者？
- 是否可用 `--allowedTools` 替代 `--dangerously-skip-permissions`？

---

### H-3 [HIGH] 无用户/频道级限流

**文件**: 所有 platform transport

每条消息无条件触发 `router.GetOrCreate`，单用户可：
- 耗尽 `maxProcs` session 池
- 产生无限 API 费用
- 反复填满 dedup map

**待讨论**:
- 限流策略：固定窗口 vs 令牌桶 vs 滑动窗口？
- 限流粒度：per-user, per-chat, or both？
- 默认阈值建议：1 msg/5s per user？
- 超限行为：静默丢弃 vs 回复提示？

---

### H-5 [HIGH] MentionMe 字段未消费

**文件**: `server/server.go`, `slack/slack.go:205`, `discord/discord.go:162`, `feishu/transport_hook.go:153`

三个平台 adapter 都设置了 `MentionMe`，但 server handler 从未读取。群聊中 bot 回复所有消息而非仅回复 @bot 的消息。

**待讨论**:
- 群聊中是否应只响应 @bot 消息？（当前行为 = 回复全部）
- 如果是，是否保留 DM 不检查 mention 的行为？
- 是否需要配置项控制此行为？

---

### M-5 [MEDIUM] Dedup 驱逐清空整个 map

**文件**: `platform/dedup.go:37`

10,000 条目满后一次性清空整个 map，攻击者可利用此窗口 replay 已处理事件。

**待讨论**:
- 改为 LRU 驱逐（evict oldest N entries）？
- 或改为 TTL-based 过期（每条目带时间戳）？
- 是否值得引入额外复杂度（当前规模下风险较低）？

---

### M-6 [MEDIUM] Session key 未转义分隔符

**文件**: `session/managed.go:62`

`platform + ":" + chatType + ":" + id + ":" + agentID` — 如果 ChatID 包含 `:`，key 结构歧义，可能导致跨用户 session 冲突。

**待讨论**:
- 使用 `\x00` 作分隔符？
- 或 URL-encode 每个组件？
- 实际风险评估：IM 平台的 ChatID 是否可能包含 `:`？

---

### M-7 [MEDIUM] Kiro 模型无法通过 IM 动态切换

**文件**: `config.yaml`, `cli/protocol_acp.go`

当前 kiro backend 的模型在 config.yaml 中静态配置，无法通过 IM 指令动态切换。用户需要通过 IM 命令（如 `/model opus`）在运行时变更 kiro 使用的模型。

**待讨论**:
- 新增 `/model <name>` IM 指令？作用域：per-session vs per-agent vs 全局？
- kiro ACP 协议是否支持在 session 内切换模型？（需确认 `session/prompt` params）
- 如不支持 session 内切换，是否需要 reset session 后以新模型重建？
