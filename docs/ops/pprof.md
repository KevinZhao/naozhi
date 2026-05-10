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

### 当前 13 个 naozhi 计数器

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

- `internal/metrics/metrics_test.go`: 锁 13 个 expvar 名 / Add 语义 / JSON shape
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
