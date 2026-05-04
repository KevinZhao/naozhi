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

### 当前 5 个 naozhi 计数器

| 名称 | 语义 | 什么时候值得警觉 |
|---|---|---|
| `naozhi_session_create_total` | spawnSession 成功（exempt 会话**不计**） | 突增常伴 IM spam |
| `naozhi_session_evict_total` | LRU evictOldest 成功释放一个 slot | 长期线性上升 = `session.max_procs` 太低 |
| `naozhi_cli_spawn_total` | `wrapper.Spawn` 成功（新 CLI 子进程出生；reconnect **不计**） | 通常 ≥ session_create_total，大幅超出 = exempt（planner/scratch）churn |
| `naozhi_ws_auth_fail_total` | WS auth_fail 回包（rate-limit 和 invalid-token 两种都计） | 每分钟 >10 = 很可能在被 brute-force |
| `naozhi_shim_restart_total` | `shim.StartShimWithBackend` 成功（Reconnect **不计**） | 两次重启间持续增长 = shim 在 crash→respawn |

### 拉取

```bash
ssh ec2-user@prod-host 'curl -s -H "Authorization: Bearer $TOK" http://127.0.0.1:8180/api/debug/vars' | jq '{
  session_create: .naozhi_session_create_total,
  session_evict: .naozhi_session_evict_total,
  cli_spawn: .naozhi_cli_spawn_total,
  ws_auth_fail: .naozhi_ws_auth_fail_total,
  shim_restart: .naozhi_shim_restart_total,
  uptime: .memstats.uptime
}'
```

### 与 `/health` 的分工

- `/health`: 少量高层状态（status / uptime / watchdog kills），前端 dashboard 会持续 poll
- `/api/debug/vars`: 运维场景下按需拉取的完整计数器 + stdlib 的 memstats/cmdline

两者不重复 counter；watchdog kills 暂时保留在 `/health` 兼容既有 dashboard，未来若升级 Prometheus 会迁到 metrics 包。

### 回归契约

- `internal/metrics/metrics_test.go`: 锁 expvar 名 / Add 语义 / JSON shape
- `internal/metrics/counter_wiring_contract_test.go`: source-grep 锁 5 个 call site + WSAuthFail 两分支 ≥2 次
- `internal/server/debug_expvar_test.go`: 锁 auth 401 / 非 loopback 403 / loopback+auth 返 JSON 含全部 5 个 counter + stdlib memstats
- `cmd/naozhi/doctor_test.go`: `checkExpvar` 覆盖 pass/fail/warn/no-token 4 档

任何改动 call sites 的 PR 都会动这些测试。

### 升级路径

若未来部署进入有 Prometheus scraper 的环境：
1. `internal/metrics/metrics.go` 把 `expvar.NewInt` 换成 `prometheus.NewCounter`，保留 `*expvar.Int` 变量名别名（或定义 `type Counter interface { Add(int64) }`）
2. 新增 `/metrics` 端点挂 `promhttp.Handler()`（同样 auth + loopback 保护）
3. call sites 零改动
