# Naozhi 在线 Profile (pprof)

> 生产内存暴涨 / goroutine 泄漏 / CPU 热点时不用等 crash dump，可直接拉取实时 profile。

## 安全模型

`/api/debug/pprof/*` 暴露 Go stdlib 的 pprof handlers，受**两层独立防护**：

1. **Bearer token / signed cookie 认证**（同 `/api/*` 其它端点）
2. **Loopback-only**：只接受来自 `127.0.0.1` / `::1` 的请求。即使 token 泄漏，远端无法 profile

结果：要拉 profile，**必须**能 SSH 到宿主并持有 `NAOZHI_DASHBOARD_TOKEN`。生产 ALB / CloudFront 前置的任何请求都会被 403 拒绝（因为 RemoteAddr 是 ALB/代理的内网地址，不是 loopback）。

没有独立的 `debug_listen_addr` 配置 —— 复用主端口 + loopback gate 等效且运维心智负担更低。

## 用法

### 准备

```bash
# 登录宿主
ssh ec2-user@prod-host

# 读 token（按你的部署调整路径）
export TOK=$(sudo grep NAOZHI_DASHBOARD_TOKEN /home/ec2-user/.naozhi/env | cut -d= -f2-)
```

### 列出所有 profile

```bash
curl -s -H "Authorization: Bearer $TOK" http://127.0.0.1:8180/api/debug/pprof/ | \
  grep -oP 'pprof/\K\w+' | sort -u
# 常见输出：allocs block cmdline goroutine heap mutex profile threadcreate trace
```

### 堆（内存暴涨）

```bash
curl -s -H "Authorization: Bearer $TOK" \
  'http://127.0.0.1:8180/api/debug/pprof/heap' > /tmp/heap.pprof

# 本地分析（把 /tmp/heap.pprof scp 回你的开发机）
go tool pprof -http=:0 /tmp/heap.pprof
```

快速文本总览：

```bash
curl -s -H "Authorization: Bearer $TOK" \
  'http://127.0.0.1:8180/api/debug/pprof/heap?debug=1' | head -50
```

### Goroutine（泄漏 / 卡死）

```bash
# 带 stack trace 的全量 goroutine 列表
curl -s -H "Authorization: Bearer $TOK" \
  'http://127.0.0.1:8180/api/debug/pprof/goroutine?debug=2' > /tmp/goroutines.txt
wc -l /tmp/goroutines.txt   # 数量陡增即可能泄漏
grep -c "^goroutine " /tmp/goroutines.txt
```

配合 CPU profile 定位 hot path：

```bash
# 默认 30s 采样
curl -s -H "Authorization: Bearer $TOK" \
  'http://127.0.0.1:8180/api/debug/pprof/profile?seconds=30' > /tmp/cpu.pprof
go tool pprof -http=:0 /tmp/cpu.pprof
```

### Block / Mutex（锁争用）

这两类 profiler 默认采样率为 0（不采样，无开销）。naozhi 当前**没有**在启动时启用 `runtime.SetBlockProfileRate` / `SetMutexProfileFraction`，所以这两个端点返回空。要启用，改 `cmd/naozhi/main.go` 的 init 阶段并重启 —— 属侵入式修改，不建议常开。

### 进程元数据

```bash
curl -s -H "Authorization: Bearer $TOK" http://127.0.0.1:8180/api/debug/pprof/cmdline
curl -s -H "Authorization: Bearer $TOK" 'http://127.0.0.1:8180/api/debug/pprof/goroutine?debug=1' | head
```

## 常见陷阱

- **远程 curl 被拒**：这不是 bug，是 loopback-only 设计。`ssh host curl -s ...` 代替
- **ALB 路径也被拒**：同上 —— ALB RemoteAddr 是 ALB 自己的 IP，非 loopback
- **401 Unauthorized**：检查 `Authorization` header 拼写 / `$TOK` 是否被 shell 吞了特殊字符
- **CPU profile 采样期间服务反应略慢**：正常 —— pprof 每 100Hz 中断一次，30s 采样会有 ~1-3% overhead

## 回归契约

`internal/server/debug_pprof_test.go` 锁定 4 条不变量：
- `isLoopbackRemote` 对 12 种 addr 形状（含 IPv4/IPv6/hostname/garbage）正确分类
- 无 auth → 401，且 body 不泄漏 `pprof` 字样
- 有 auth 但非 loopback → 403，body 明确"loopback-only"
- 有 auth 且 loopback → 200，index body 含已知 profile 名

任何改动了 `registerPprof` 的 PR 都会动这些测试。

## 不在本期

- 启用 block/mutex profiling —— 可单独立项，加 `config.debug.enable_block_profile` 开关
- `go tool pprof -http=` 服务器侧直接托管 UI —— 避免把交互式 UI 暴露在生产
- 集成到 continuous profiling 服务（Pyroscope / Polar Signals）—— 需要更重的架构决策

---

## expvar 计数器（OBS2）

`/api/debug/vars` 暴露 stdlib `expvar.Handler()`，安全模型与 pprof **完全一致**（requireAuth + loopback-only + trustedProxy 不豁免）。

### 当前 naozhi 计数器

| 名称 | 语义 | 什么时候值得警觉 |
|---|---|---|
| `naozhi_session_create_total` | spawnSession 成功（exempt 会话**不计**） | 突增常伴 IM spam |
| `naozhi_session_evict_total` | LRU evictOldest 成功释放一个 slot | 长期线性上升 = `session.max_procs` 太低 |
| `naozhi_cli_spawn_total` | `wrapper.Spawn` 成功（新 CLI 子进程出生；reconnect **不计**） | 通常 ≥ session_create_total，大幅超出 = exempt（planner/scratch）churn |
| `naozhi_ws_auth_fail_total` | WS auth_fail 回包（rate-limit 和 invalid-token 两种都计） | 每分钟 >10 = 很可能在被 brute-force |
| `naozhi_ws_auth_fail_rate_limited_total` | WSAuthFail 的 rate-limit 分支子集 | 相对 invalid_token 占比高 = 单 IP 在 burst 撞墙（dashboard reconnect storm） |
| `naozhi_ws_auth_fail_invalid_token_total` | WSAuthFail 的 invalid-token 分支子集 | 相对 rate_limited 占比高 = 单 IP 在 pace 下撞密码（credential spray 特征） |
| `naozhi_shim_restart_total` | `shim.StartShimWithBackend` 成功（Reconnect **不计**） | 两次重启间持续增长 = shim 在 crash→respawn |
| `naozhi_spawn_panic_recovered_total` | `panicSafeSpawn` 吞掉的 wrapper.Spawn panic | 非零即需 grep journalctl 定位 root cause（进程活着但 bug 存在） |
| `naozhi_panic_recovered_total` | 全局 recover() 吞掉的 panic 数（wsclient readPump / wshub 远程 send+interrupt goroutine / dispatch ownerLoop / feishu cleanupNoncesTick 等高信号点）；`spawn_panic_recovered` 是其真子集 | 非零=有 panic 被 recover；按时间戳对齐 slog.Error stack dump 定位 root cause |
| `naozhi_shim_reconnect_grace_backfill_total` | shim 场景 `shimReconnectGraceDelay` 超时的 JSONL 延后 backfill（R53-ARCH-001 兜底路径） | 非零 = ReconnectShims 漏了某些 shim（shim 在 shimManagedKeys 与 Discover 间 die） |
| `naozhi_interrupt_sent_total` | InterruptViaControl 成功把 control_request 送到 CLI | dashboard interrupt 按钮的 happy path，pair Interrupt* 其它 3 个看用户效用 |
| `naozhi_interrupt_no_turn_total` | InterruptViaControl session 在但无 active turn | 相对 sent 占比高 = UI 该在 idle 状态禁用 interrupt 按钮 |
| `naozhi_interrupt_unsupported_total` | InterruptViaControl 当前协议无 stdin-level interrupt（ACP 等），router fallback SIGINT | 反映部署对 SIGINT fallback 的依赖度（SIGINT 语义更重，整 CLI kill） |
| `naozhi_interrupt_error_total` | InterruptViaControl transport write 失败（shim socket 死 / broken pipe） | 非零几乎肯定意味着 shim 僵尸，pair `naozhi_shim_restart_total` 看 reconcile 是否清理 |
| `naozhi_eventlog_persist_written_total` | 写到 `<keyhash>.log` 的 EventEntry 条数 | 对话量稳定但 written 停涨 = persist goroutine 卡死或 PersistSink channel 满（看 dropped） |
| `naozhi_eventlog_persist_dropped_total` | PersistSink channel 满时丢弃的 EventEntry 条数 | 持续非零 = 磁盘或 writer goroutine 不堪负载；持久化层丢事件（内存 ring 里仍在） |
| `naozhi_eventlog_persist_fsync_total` | Persister 发起的 fsync(log)+fsync(idx) 总次数 | 稳态 ~10/s；远超说明 debounce 没聚合（FlushInterval 配置太小 / 频繁 Flush()） |
| `naozhi_eventlog_persist_malformed_lines_total` | `schema.MarshalRecord` 拒绝的条数（oversize / 编码失败） | 稳态 0；非零 = 有上游产出畸形 entry，对应 slog.Warn 有 UUID / size 信息 |
| `naozhi_eventlog_persist_replay_leak_total` | replayPhase=true 的 batch 到达 sink 的累计条数 | **必须稳态 0**；非零 = 某调用路径在 InjectHistory 前挂了 SetPersistSink，违反 RFC §3.2.2 顺序契约 |
| `naozhi_attachment_ref_bump_total` | attachment 引用计数 .meta 重写次数(coalesce 后) | 跟"含图事件达盘次数 × 不同 attachment 数"同步涨 |
| `naozhi_attachment_ref_clear_total` | OnSessionRemoved 走 workspace 清 keyhash 的 .meta 重写次数 | 仅在 session 被删时短时涨,平时 0 |
| `naozhi_attachment_ref_meta_error_total` | tracker UpdateMetaFile 失败数(缺 sidecar / ENOSPC / perm) | 稳态 0;非零 = attachment 将回退到仅 uploaded_at TTL GC |
| `naozhi_attachment_ref_drop_total` | tracker 非阻塞 enqueue 满 channel 丢弃数 | 稳态 0;非零 = 调用方提交过快或磁盘 latency 异常,同 Persister 运维 |
| `naozhi_attachment_gc_reaped_total` | attachment-gc daemon 真删的附件 payload 数(dry-run 不计) | 随老附件被回收平稳涨;长期 0 而磁盘涨 = daemon 未开或枚举不到 workspace |
| `naozhi_attachment_gc_would_reap_legacy_total` | dry-run/live 拟删:无 .meta 走 date-dir TTL 判定(较安全) | 观察 dry-run 风险构成用;开真删前看占比 |
| `naozhi_attachment_gc_would_reap_no_refs_total` | dry-run/live 拟删:有 .meta 但无引用 —— **可能是 tracker 尚未 bump 的活跃引用** | 高风险桶;占比高时延长 dry-run 观察期再开真删 |
| `naozhi_attachment_gc_would_reap_expired_total` | dry-run/live 拟删:被引用过但最后引用超 refTTL(较安全) | 观察 dry-run 风险构成用 |
| `naozhi_attachment_gc_sweep_total` | attachment-gc daemon Tick 执行次数(成功+失败) | 按 tick 周期平稳涨;停滞 = daemon 未跑,核对 enabled / 进程是否重启过频 |
| `naozhi_attachment_gc_error_total` | workspace 级 GC 错误数(单 root 的 ReadDir 失败;**不含**文件级 remove 失败) | 非零 = 某 workspace 根权限/IO 异常,对照 slog.Warn "attachment-gc: sweep failed" 的 root |
| `naozhi_cron_execution_slow_total` | cron job 成功执行但耗时超过 `cronSlowThreshold`（当前 30s）的累计次数（R208-OBS1 的 MVP histogram 替身） | 持续增长 = 某些 job 长期压线超时；对照 job id（slog.Warn "cron execution slow"）确认是 prompt 设计问题还是 backend 退化 |
| `naozhi_cron_send_budget_doubled_total` | spawn 阶段已耗 >50% jobTimeout 后才进入 sendCtx 的次数（R240-GO-4 / R230B-GO-1 wall-clock 翻倍信号） | 持续增长 = GetOrCreate / Spawn 慢路径在挤压 Send 预算，单次 run 实际 wall clock 接近 2×jobTimeout；对照 slog.Warn "cron send budget exceeds job/2" 找具体 job_id 排查 spawn 慢因 |
| `naozhi_cron_stop_budget_exceeded_gc_total` | Scheduler.Stop() 冷启动 GC 等待超过 `gcWaitBudget`（5s）的累计次数（R250-GO-20 / #1083） | 非零 = trimAll 卡在文件系统层；接近 systemd TimeoutStopSec=30s 时报警，参考 slog.Warn "cron: gc goroutine wait timeout" |
| `naozhi_cron_stop_budget_exceeded_drain_total` | Scheduler.Stop() cron drain 阶段超过 `stopBudget`（30s）的累计次数（R250-GO-20 / #1083） | 非零 = 在途 cron tick 没在预算内退出；持续增长说明热点 job 在 shutdown 路径占用太久，参考 slog.Warn "cron scheduler: stop deadline exceeded before cron.Stop drained" |
| `naozhi_cron_stop_budget_exceeded_trigger_total` | Scheduler.Stop() triggerWG 等待阶段超过剩余预算的累计次数（R250-GO-20 / #1083） | 非零 = 手动 TriggerNow 引发的 goroutine 拖到 stopBudget 末尾；对照 slog.Warn "cron scheduler: stop deadline exceeded during triggerWG wait" 排查 webhook / notify 阻塞 |
| `naozhi_cron_notify_partial_total` | cron 完成通知未发完全部 chunk 的累计次数：要么 replyCtx 超时（cronNotifyTimeout 30s）中途中断，要么某 chunk ReplyWithRetry 失败后 abort（R249-CR-26 / #966） | 持续增长 = IM 收件端在看截断的 cron 输出（webhook 慢/失败）；对照 slog.Warn "cron notify ... dropped" 找 platform/chat，建议引导用户改看 dashboard run-detail 面板 |
| `naozhi_cron_run_started_total` | cron run 开始计数（CAS gate 通过后；docs/rfc/cron-run-history.md P0） | 与 `_ended_total` 差值远大于 inflight gauge = 进程崩溃打断在途 run；查 panic 日志 |
| `naozhi_cron_run_ended_total` | cron run 终态计数（聚合 succeeded/failed/skipped/timed_out/canceled） | 与 `_started_total` 配合判断"开了但没收尾"的 run 数 |
| `naozhi_sysession_run_started_total` | sysession daemon run 开始计数（CAS gate 通过后；#1723 RFC §6 Phase 1.5，对称于 cron 的 `_started_total`） | 与 `naozhi_sysession_run_ended_total` 差值远大于在途 = 进程崩溃打断在途 run；查 panic 日志 |
| `naozhi_sysession_run_ended_total` | sysession daemon run 终态计数（聚合所有终态） | 与 `naozhi_sysession_run_started_total` 配合判断"开了但没收尾"的 run 数 |
| `naozhi_cron_run_succeeded_total` | succeeded 终态计数 | 比例骤降 = backend / prompt 退化；对比 failed/timed_out 看根因 |
| `naozhi_cron_run_failed_total` | failed 终态计数（session_error / send_error / workdir_* 等非超时错误） | 持续涨 = job 配置或目标不可达；按 LastErrorClass 分组排查 |
| `naozhi_cron_run_skipped_total` | skipped 终态计数（overlap_skipped / paused_concurrent） | 持续涨 = 上一轮没跑完下一轮就来了；调长 schedule 或缩 prompt |
| `naozhi_cron_run_timed_out_total` | timed_out 终态计数（DeadlineExceeded） | 涨 = 接近 jobTimeout 边界；对比 cron_execution_slow_total 看是否同因 |
| `naozhi_cron_run_canceled_total` | canceled 终态计数（context.Canceled，shutdown / job 删除中途） | 重启高峰短时涨正常；稳态非零 = job 频繁被删/recreate |
| `naozhi_cron_watchdog_interrupt_timeout_total` | cron deadline-watchdog 触发后 `InterruptViaControl` 在 `watchdogInterruptTimeoutDefault`（3s）内未返回的累计次数（R20260527122801-SEC-3 / #1327） | 非零 = stdin 写入 wedged，inner goroutine 卡到下次 `session.Reset` 才放行；和 `naozhi_shim_restart_total` 对照判断 reconcile 是否清理；持续涨需要排查 shim 健康 |
| `naozhi_auto_chain_origins_length_mismatch_total` | `prev_session_origins` 与 `prev_session_ids` 长度漂移检测命中（自动兜底重建） | **必须稳态 0**；非零 = 某写路径违反 prev_session_ids append-only 不变量（RFC §4.6） |
| `naozhi_auto_chain_retired_on_startup_total` | 启动时一次性剥离 auto-spawn / auto-backfill 脏 chain 段的 session 数（RFC docs/rfc/project-stable-session-key.md §9.2，替代旧 backfill） | 首次升级到本版本后会一次性上涨（清理历史脏数据）；之后**稳态 0**，重启幂等不再涨。持续非零 = 仍有路径写入 auto-* origin（不应发生，auto-chain 已下线） |

这个表的"完整性"由 `internal/metrics/metrics_doc_sync_test.go` 锁定：metrics.go 新增 counter 但未同步文档会在 CI 红。

### 启动阶段 gauge（RNEW-OPS-414）

以下 7 个指标记录冷启动每个阶段完成时 `time.Since(t0).Milliseconds()`（t0 在 `cmd/naozhi/main.go` 顶部一次性抓取），每进程**只 Set 一次**。值**单调递增**（累积），相邻行相减即该阶段耗时。命名用 `_ms` 后缀区别于 counter 的 `_total`——这是 gauge 而非 counter，Prometheus scraper 不应 rate()。

| 名称 | 语义（完成时刻） | 什么时候值得警觉 |
|---|---|---|
| `naozhi_startup_phase_config_ms` | `config.Load` 返回 | >500ms = YAML 巨大或 fs 慢 |
| `naozhi_startup_phase_router_ms` | `session.NewRouter` 返回（含 sessions.json 加载 + eventlog 目录扫描 + backend 版本探测） | 温启动中通常最大的一段 |
| `naozhi_startup_phase_shim_reconnect_ms` | `router.ReconnectShimsCtx` 返回 | 接近 `N_shims × 15s` = shim socket 僵住 |
| `naozhi_startup_phase_platforms_ms` | platforms 注册 + 并行 init WG（transcribe / project scan）drain 完 | 比 router 延迟大 = transcribe.New 或 project scan 慢 |
| `naozhi_startup_phase_scheduler_ms` | `scheduler.Start` 返回 | 慢 = cron store 文件过大 |
| `naozhi_startup_phase_server_ms` | `server.NewWithOptions` 返回（路由注册 / WS hub wire / dashboard 资源挂载） | 不含 `srv.Start` (后台 listen loop) |
| `naozhi_startup_phase_ready_ms` | 主 goroutine 进入 shutdown select 前一行 | 对比 systemd `START_USEC` 验证 `TimeoutStartSec` margin |

用法示例：

```bash
curl -s -H "Authorization: Bearer $TOK" http://127.0.0.1:8180/api/debug/vars | jq '{
  config: .naozhi_startup_phase_config_ms,
  router_delta: (.naozhi_startup_phase_router_ms - .naozhi_startup_phase_config_ms),
  shim_reconnect_delta: (.naozhi_startup_phase_shim_reconnect_ms - .naozhi_startup_phase_router_ms),
  platforms_delta: (.naozhi_startup_phase_platforms_ms - .naozhi_startup_phase_shim_reconnect_ms),
  scheduler_delta: (.naozhi_startup_phase_scheduler_ms - .naozhi_startup_phase_platforms_ms),
  server_delta: (.naozhi_startup_phase_server_ms - .naozhi_startup_phase_scheduler_ms),
  ready_delta: (.naozhi_startup_phase_ready_ms - .naozhi_startup_phase_server_ms),
  ready_total: .naozhi_startup_phase_ready_ms
}'
```

`metrics_doc_sync_test.go` 的正则只匹配 `*_total`，所以新增 gauge 不会强制文档同步；但保持本表跟 `metrics.go` 齐整对操作员仍然有价值。

### 运行时 gauge（R208-OBS1）

| 名称 | 语义 | 什么时候值得警觉 |
|---|---|---|
| `goroutines` | `runtime.NumGoroutine()` 在 scrape 时刻的实时值（`expvar.Func` 动态求值，无后台采样） | 稳态取决于 session 并发数（每 session ~2-4 goroutine）。突增且不回落 = wsclient / wshub / dispatch / shim readLoop 某处泄漏，对照 `/api/debug/pprof/goroutine?debug=2` stack dump 定位 |

拉取：
```bash
curl -s -H "Authorization: Bearer $TOK" http://127.0.0.1:8180/api/debug/vars | jq '.goroutines'
```

注意：键名不带 `naozhi_` 前缀，因为这是进程级 runtime 指标，不是业务 counter；跟 stdlib 的 `cmdline` / `memstats` 同层。

### 拉取

```bash
ssh ec2-user@prod-host 'curl -s -H "Authorization: Bearer $TOK" http://127.0.0.1:8180/api/debug/vars' | jq '{
  session_create: .naozhi_session_create_total,
  session_evict: .naozhi_session_evict_total,
  cli_spawn: .naozhi_cli_spawn_total,
  ws_auth_fail: .naozhi_ws_auth_fail_total,
  ws_auth_fail_rate_limited: .naozhi_ws_auth_fail_rate_limited_total,
  ws_auth_fail_invalid_token: .naozhi_ws_auth_fail_invalid_token_total,
  shim_restart: .naozhi_shim_restart_total,
  spawn_panic_recovered: .naozhi_spawn_panic_recovered_total,
  panic_recovered: .naozhi_panic_recovered_total,
  shim_reconnect_grace_backfill: .naozhi_shim_reconnect_grace_backfill_total,
  interrupt_sent: .naozhi_interrupt_sent_total,
  interrupt_no_turn: .naozhi_interrupt_no_turn_total,
  interrupt_unsupported: .naozhi_interrupt_unsupported_total,
  interrupt_error: .naozhi_interrupt_error_total,
  eventlog_written: .naozhi_eventlog_persist_written_total,
  eventlog_dropped: .naozhi_eventlog_persist_dropped_total,
  eventlog_fsync: .naozhi_eventlog_persist_fsync_total,
  eventlog_malformed: .naozhi_eventlog_persist_malformed_lines_total,
  eventlog_replay_leak: .naozhi_eventlog_persist_replay_leak_total,
  attachment_ref_bump: .naozhi_attachment_ref_bump_total,
  attachment_ref_clear: .naozhi_attachment_ref_clear_total,
  attachment_ref_meta_error: .naozhi_attachment_ref_meta_error_total,
  attachment_ref_drop: .naozhi_attachment_ref_drop_total,
  cron_execution_slow: .naozhi_cron_execution_slow_total,
  uptime: .memstats.uptime
}'
```

### OpenMetrics / Prometheus 兼容度（RNEW-OPS-416）

切到 Prometheus scraper 前需知道 expvar 层面的几条已知限制：

1. **`*_total` 后缀下 int 值不保证单调**：每次进程重启会归零；Prometheus `rate()`/`increase()` 在重启 window 会看到"倒退"。scraper 端通常由 `resets()` 补偿，naozhi 侧无额外保证。
2. **只发 `int64`，不发 `float64`**：counter 值永远是整数；未来若有需要 fractional 累加（如 latency sum）要改用 `expvar.Float` 或升级到 Prometheus `CounterVec`。
3. **无 labels**：按 IM 平台 / node_id / backend 等维度拆分**只能**通过新增独立 counter 实现（如 `ws_auth_fail_*` 已按 rate_limited / invalid_token 拆了两个子 counter）。这是刻意设计（防 label cardinality 爆炸），但迁移 Prometheus 时需要重新定义 CounterVec 的 label schema。
4. **无 OpenMetrics 元数据**：expvar JSON 不发 `# HELP` / `# TYPE counter` 标签，scrape 方需要知道"`*_total` 是 counter 而非 gauge"才能正确聚合。本表（docs/ops/pprof.md）是当前唯一的语义来源。
5. **命名规范**：`*_total` 后缀在 OpenMetrics 规范里是强约定（counter 语义），现有名字可直接保留；但未来 gauge 类指标（如 R172-ARCH-D10 追踪的当前 backoff 值 —— 函数在 `internal/upstream/backoff.go::jitterBackoff`，被 connector 调用）不能再套 `_total` 后缀，应用 `_seconds` / `_ratio` 等。

### 与 `/health` 的分工

- `/health`: 少量高层状态（status / uptime / watchdog kills），前端 dashboard 会持续 poll
- `/api/debug/vars`: 运维场景下按需拉取的完整计数器 + stdlib 的 memstats/cmdline

两者不重复 counter；watchdog kills 暂时保留在 `/health` 兼容既有 dashboard，未来若升级 Prometheus 会迁到 metrics 包。

### 回归契约

- `internal/metrics/metrics_test.go`: 锁 expvar 名 / Add 语义 / JSON shape
- `internal/metrics/metrics_doc_sync_test.go`: 对比 `docs/ops/pprof.md` 表中的 counter 名与 `metrics.go` 中 `expvar.NewInt` 的实际集合，漏/多均失败
- `internal/metrics/counter_wiring_contract_test.go`: source-grep 锁 call site + WSAuthFail 两分支 ≥2 次
- `internal/server/debug_expvar_test.go`: 锁 auth 401 / 非 loopback 403 / loopback+auth 返 JSON 含已注册 counter + stdlib memstats
- `cmd/naozhi/doctor_test.go`: `checkExpvar` 覆盖 pass/fail/warn/no-token 4 档

任何改动 call sites 的 PR 都会动这些测试。

### 升级路径

若未来部署进入有 Prometheus scraper 的环境：
1. `internal/metrics/metrics.go` 把 `expvar.NewInt` 换成 `prometheus.NewCounter`，保留 `*expvar.Int` 变量名别名（或定义 `type Counter interface { Add(int64) }`）
2. 新增 `/metrics` 端点挂 `promhttp.Handler()`（同样 auth + loopback 保护）
3. call sites 零改动

---

## Multi-Backend 标签维度（Sprint 6a）

> Multi-Backend RFC §10 落地。multi-backend (claude + kiro) 启用后，"100 次 spawn"看不出哪些是 claude / 哪些是 kiro，引入 4 项带 `backend` 维度的指标。

### 设计选择：expvar.Map（保持零依赖）

仓库 `go.mod` 不含 prometheus（实测：`git grep prometheus go.sum go.mod` 无匹配），且 `internal/metrics/metrics.go` 顶部 docstring 明确"zero dependencies, stdlib-stable"。引入 prometheus 仅为加 label 与现有设计哲学冲突。`expvar.Map` 原生支持 keyed counter，JSON shape 友好，零成本。

每个 labeled 指标在 `/api/debug/vars` 暴露为 JSON 对象，key 为 `|` 拼接的 label tuple；`metrics.LabeledCounter.Add(delta, labels...)` 是单一写入入口。详见 `internal/metrics/labeled.go`。

### 双写迁移期（4 周）

为不破坏现存 dashboard / jq 模板，每个有"legacy 无 label" counterpart 的指标走双写：调一次 `metrics.RecordCLISpawn(backend)` 同时 +1 legacy `expvar.Int` 和新 `expvar.Map`。预期 `Sum(by_backend) == legacy_total` 始终成立，`internal/metrics/multibackend_test.go::TestCLISpawnTotal_RecordsLabeledAndLegacy` 锁这一不变量。

迁移期结束后（2026-06-15），可单 PR 移除 `CLISpawnTotal` 等 legacy `expvar.Int`，TODO 标记 `R222-OBS-MULTIBACKEND-LEGACY`。

### 新指标列表

| expvar 名称 | 类型 | Labels | 语义 | 何时警觉 |
|---|---|---|---|---|
| `naozhi_cli_spawn_total_by_backend` | counter | `backend` | `wrapper.Spawn` 成功（per-backend），与 legacy `naozhi_cli_spawn_total` 双写 | 单 backend 涨 / 另一持平 = 用户偏一边；某 backend 突 0 = 该 binary 不可用 |
| `naozhi_session_active` | gauge (legacy mirror) | — | 当前活动 session 总数（exempt 不计） | 总数 vs `r.activeCount` 校验 |
| `naozhi_session_active_by_backend` | gauge | `backend` | per-backend 活动 session 数 | 与 `_active` 总和应相等；不等 = 簿记漂移（`reconcileSessionActiveByBackendLocked` 兜底） |
| `naozhi_protocol_rpc_error_total` | counter | `backend, method, code` | JSON-RPC 错误（仅 ACP 协议；stream-json 不上报） | 单 (backend, code) 突增 = agent 端某类 RPC 出问题；method=`""` 表示 ReadEvent 路径无法关联请求 |
| `naozhi_acp_cancel_total` | counter | `backend` | `ACPProtocol.WriteInterrupt` 成功送出 `session/cancel` notification（pre-handshake 失败不计） | 与 `naozhi_interrupt_*` 一族 cross-check：`acp_cancel_total ≈ interrupt_sent_total` 中 ACP 部分；不等 = 路由 fallback SIGINT |
| `naozhi_metrics_label_overflow_total` | counter | — | label tuple 长度超过 `maxLabelKeyLen=256` 折叠到 `_overflow_` 桶的累计次数 | **必须稳态 0**；非零 = 某 caller 未做 label sanitize（agent 注入的 method/code 字符串过长） |

### 拉取示例

```bash
ssh ec2-user@prod-host 'curl -s -H "Authorization: Bearer $TOK" http://127.0.0.1:8180/api/debug/vars' | jq '{
  cli_spawn_total: .naozhi_cli_spawn_total,
  cli_spawn_by_backend: .naozhi_cli_spawn_total_by_backend,
  session_active: .naozhi_session_active,
  session_active_by_backend: .naozhi_session_active_by_backend,
  rpc_errors: .naozhi_protocol_rpc_error_total,
  acp_cancels: .naozhi_acp_cancel_total,
  label_overflow: .naozhi_metrics_label_overflow_total
}'
```

输出（典型 multi-backend 部署）：

```json
{
  "cli_spawn_total": 412,
  "cli_spawn_by_backend": {"claude": 380, "kiro": 32},
  "session_active": 14,
  "session_active_by_backend": {"claude": 11, "kiro": 3},
  "rpc_errors": {"kiro|session/prompt|-32601": 2},
  "acp_cancels": {"kiro": 5},
  "label_overflow": 0
}
```

### 双写期解除（follow-up 任务）

**任务 R222-OBS-MULTIBACKEND-LEGACY**（建议 4 周后执行，2026-06-15 前后）：
- 删除 `internal/metrics/metrics.go` 的 legacy `CLISpawnTotal`（保留 `_by_backend` vector）
- 删除 `RecordCLISpawn` 中的 legacy 双写
- 同步更新本文上方"当前 naozhi 计数器"表，删除 legacy 行
- `metrics_test.go::TestCountersRegisteredUnderStableNames` 删除 `naozhi_cli_spawn_total` 项
- 操作员 changelog: 通知 `_by_backend` 是新规范名，老 dashboard 需迁移 jq 表达式

`naozhi_session_active` legacy mirror 的删除决策延到 R222 后续：因为这是 gauge 不是 counter，迁移成本更高（dashboards 通常直接读 gauge 值，不算 rate），保留双写更长期可接受。

### 回归契约

- `internal/metrics/multibackend_test.go::TestCLISpawnTotal_RecordsLabeledAndLegacy`: 锁双写不变量
- `multibackend_test.go::TestProtocolRPCErrorTotal_LabelsRoundTrip`: 锁 (backend, method, code) tuple 不互相覆盖
- `multibackend_test.go::TestACPCancelTotal_BackendLabel`: 锁 backend label 维度
- `multibackend_test.go::TestRecordSessionActive_GaugeBookkeeping`: 锁 +1/-1 mirror 同步
- `multibackend_test.go::TestLabelKey_OverlongCollapsesToOverflow`: 锁 cardinality bound
- `counter_wiring_contract_test.go`: 锁 `metrics.RecordCLISpawn` source-grep 在 `wrapper.go`
