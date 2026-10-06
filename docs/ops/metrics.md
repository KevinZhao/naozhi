# /metrics：Prometheus 抓取

`/api/debug/vars` 只在 `debug_mode` + loopback 下可读，外部监控拿不到计数器。
`server.metrics_enabled: true` 后，`GET /metrics` 以 Prometheus 文本格式导出所有
`naozhi_*` expvar（#3436）。

## 鉴权

- 必须配置 `server.dashboard_token`，否则 403。
- 请求带 `Authorization: Bearer <dashboard_token>`；没有或错误 → 401 + `WWW-Authenticate`。
- **不限 loopback**（与 `/api/debug/vars` 不同）：抓取器通常在别的机器上。因此这个
  token 等于监控系统持有 dashboard 权限——用独立的 scrape 账号时请把它当机密管理。
- 只读 GET，不走 same-origin 检查。

## 映射规则

| expvar | Prometheus |
|---|---|
| 名字以 `_total` 结尾，或按计数器注册的 Map（`NewLabeledCounter` / `promexport.NewMap`，如 `naozhi_cli_spawn_total_by_backend`） | `# TYPE … counter` |
| 其它 `Int` / `Float` / Map / 返回数字的 `Func` | `# TYPE … gauge` |
| `expvar.Map`（如 `naozhi_spawn_diag_total`） | 每个 key 一条样本；label 名取自注册表（见下），key 里的 `\|` 按位置拆成各个 label |
| cron 延迟桶（`naozhi_cron_execution_duration_ms_bucket` + `_sum`） | `# TYPE naozhi_cron_execution_duration_ms histogram`：`_bucket{le=…}`、`_sum`、`_count`（= `+Inf` 桶）；单位仍是毫秒 |
| 非 `naozhi_` 前缀（memstats / cmdline）、非数字值 | 不导出 |

## Label 名

label 名在注册 map 时给出（`metrics.NewLabeledCounter(name, labels...)` /
`promexport.NewMap(name, labels...)`），`cmd/naozhi` 的契约测试保证每个 `naozhi_*`
Map 都登记过，没登记的会退回通用的 `key` 标签。空值是 `_empty_`，超长的整组值是
`_overflow_`（所有 label 都填它）；值的个数与登记的 label 数对不上的 key 也导出为
`_overflow_`，不补 `_empty_`。渲染成同一组 label 的多个 key 合并求和，一次抓取里不会出现重复序列。

| 指标 | label |
|---|---|
| `naozhi_cli_spawn_total_by_backend`、`naozhi_session_active_by_backend`、`naozhi_acp_cancel_total` | `backend` |
| `naozhi_protocol_rpc_error_total` | `backend`、`method`、`code` |
| `naozhi_spawn_diag_total` | `layer`、`action` |
| `naozhi_dispatch_turn_error_result_total` | `class` |
| `naozhi_dispatch_denied_total` | `platform_reason`（值是 `<platform>:<reason>`） |
| `naozhi_dispatch_budget_blocked_total` | `scope` |
| `naozhi_webhook_{delivered,failed,dropped}_total` | `endpoint`（配置里的下标） |
| `naozhi_config_reload_total` | `outcome` |

每个计数器的语义和告警线索见 [pprof.md](pprof.md) 的计数器表（doc-sync 测试保证
那张表覆盖全部已注册指标）。

## prometheus.yml 片段

```yaml
scrape_configs:
  - job_name: naozhi
    scheme: https
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/naozhi.token
    static_configs:
      - targets: ["naozhi.example.com"]
    metrics_path: /metrics
```

生产拓扑是 CloudFront → ALB → EC2，`/metrics` 会经过同一条链路；只想内网抓取时
把 ALB 监听规则或安全组限制到抓取器的地址。

## 验证

`naozhi doctor` 的 `metrics` 项做同样的探测（见 [doctor.md](doctor.md)）。

```bash
curl -s -H "Authorization: Bearer $TOK" https://naozhi.example.com/metrics | head
# TYPE naozhi_dispatch_message_total counter
naozhi_dispatch_message_total 1284
```
