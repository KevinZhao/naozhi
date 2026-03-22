## 待解决问题（代码审查汇总）

上次更新: 2026-03-22

### 已修复问题（v0.0.2 后）

- [x] Shutdown 竞争：main 提前退出导致 session/cron store 未持久化
- [x] WeChat API client 无 HTTP timeout，goroutine 无限堆积
- [x] processIface 类型断言绕过接口抽象
- [x] workspace 目录权限 0755→0700
- [x] image dedup key 使用 raw path 导致重复发送
- [x] cron 无 per-chat job 上限，单聊天可耗尽全局配额
- [x] Feishu started 字段无 mutex 保护
- [x] SupportsInterimMessages 默认 true（不安全）
- [x] Cron ID 碰撞无检测
- [x] safeImageDirs 含 os.TempDir() 冗余项
- [x] Discord MIME allowlist 过于宽泛（接受 image/svg+xml）
- [x] Feishu challenge encode 错误未检查
- [x] ACPProtocol 共享状态 bug（Clone 方法）
- [x] textBuf string→strings.Builder
- [x] Feishu token 常量时间比较
- [x] Feishu challenge 移到认证之后
- [x] splitText UTF-8 安全
- [x] Cron 错误信息脱敏
- [x] EvalSymlinks 防符号链接穿越
- [x] 目录权限 0755→0700（store）
- [x] 敏感 args 降为 Debug 日志

---

### C-1 [CRITICAL] 无用户授权校验

**文件**: `cli/protocol_claude.go`, `server/server.go`

`--dangerously-skip-permissions` 硬编码，任何 IM 用户发消息即触发完整 CLI 权限（读写文件、执行命令、网络请求）。多用户共享网关下风险更高。

**待决策**:
- 是否需要 `allowed_users` / `allowed_chats` 白名单？
- 白名单粒度：用户级 vs 频道级？
- 是否可用 `--allowedTools` 替代？

---

### H-1 [HIGH] 无用户/频道级限流

**文件**: 所有 platform transport

每条消息无条件触发 `router.GetOrCreate`，单用户可耗尽 `maxProcs`、产生无限 API 费用。

**待决策**:
- 限流策略：固定窗口 vs 令牌桶？
- 限流粒度：per-user / per-chat / both？
- 默认阈值：1 msg/5s per user？
- 超限行为：静默丢弃 vs 回复提示？

---

### H-2 [HIGH] MentionMe 未消费

**文件**: `server/server.go`

三个平台都设置了 `MentionMe`，但 handler 从未读取。群聊中 bot 回复所有消息而非仅 @bot。

**待决策**:
- 群聊是否只响应 @bot？
- DM 是否保持不检查 mention？
- 是否需要上下文缓冲（非 @bot 消息不回复但保留上下文）？

---

### M-1 [MEDIUM] Session store 仅关机时保存

**文件**: `session/router.go`

`saveStore` 只在 `Shutdown()` 中调用。SIGKILL、OOM 或 panic 导致所有 session ID 丢失。

**建议**: 新 session ID 捕获时触发保存，或后台 ticker 定期保存。

---

### M-2 [MEDIUM] Cleanup 不删除死条目

**文件**: `session/router.go`

`Cleanup()` 关闭过期进程但不从 `r.sessions` map 中移除。map 无限增长，`Stats().total` 只增不减。

**建议**: 进程死亡超过 TTL 后从 map 移除，session ID 保留在持久化 store 中供 resume。

---

### M-3 [MEDIUM] Cron 与 interactive 共享进程池

**文件**: `cron/scheduler.go`, `session/router.go`

Cron session 使用 `"cron:" + j.ID` key 占用 `maxProcs`（默认 3）。两个并发 cron job 可阻塞所有交互用户。

**建议**: cron 单独进程预算，或为 interactive 保留最低 slot。

---

### M-4 [MEDIUM] 所有 session 共享同一工作目录

**文件**: `session/router.go`

`spawnSession` 始终传 `WorkingDir: r.workspace`。不同用户/agent 的 session 共享文件系统。

**建议**: 按 session key 创建子目录 `{workspace}/{sessionKey}/`。

---

### M-5 [MEDIUM] Dedup 驱逐清空整个 map

**文件**: `platform/dedup.go`

10,000 条目满后一次性清空，重放窗口。

**建议**: 改为 LRU 或 TTL-based 过期。

---

### M-6 [MEDIUM] Config duration 字段双重解析

**文件**: `config/config.go`

`Load()` 验证 duration 字符串，但存为 string。消费者再次 `ParseDuration`。

**建议**: `Load()` 后直接存 `time.Duration`，删除 `Parse*` 方法。

---

### M-7 [MEDIUM] Feishu webhook 无时间戳新鲜度校验

**文件**: `feishu/transport_hook.go`

`X-Lark-Request-Timestamp` 参与签名计算但未校验与 `time.Now()` 的偏差。dedup 缓存老化后可重放。

**建议**: 拒绝时间戳偏差超过 5 分钟的请求。

---

### L-1 [LOW] Session key 未转义分隔符

**文件**: `session/managed.go`

`platform:chatType:id:agentID` — 若组件含 `:` 则 key 结构歧义。

**建议**: URL-encode 各组件或用 `\x00` 分隔。

---

### L-2 [LOW] WeChat contextTokens map 无限增长

**文件**: `weixin/weixin.go`

`sync.Map` 对每个用户永久保留条目，无 eviction。

**建议**: 替换为有界 LRU 缓存或加 TTL。

---

### L-3 [LOW] readLoop 丢弃解析错误仅 DEBUG 级别

**文件**: `cli/process.go`

CLI 输出格式变化时所有事件静默丢弃，用户只会触发 `noOutputTimeout`（2 min）。

**建议**: 改为 WARN + 计数器，便于早期发现格式变化。

---

### L-4 [LOW] parseCronAdd 要求双引号包裹 schedule

**文件**: `server/server.go`

`/cron add @hourly prompt` 不工作，必须 `/cron add "@hourly" prompt`。

**建议**: 对无空格的 schedule 免引号。

---

### L-5 [LOW] backend tag 无法关闭

**文件**: `server/server.go`

每条回复追加 `— cc`，无配置开关。

**建议**: 加 `reply_tag` 配置项，空值则不追加。
