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
| `expvar.Int` / `Float`，名字以 `_total` 结尾 | `# TYPE … counter` |
| 其它 `Int` / `Float` / 返回数字的 `Func` | `# TYPE … gauge` |
| `expvar.Map`（如 `naozhi_dispatch_denied_total`） | 每个 key 一条样本，`{key="feishu:not_allowed"}` |
| 非 `naozhi_` 前缀（memstats / cmdline）、非数字值 | 不导出 |

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

```bash
curl -s -H "Authorization: Bearer $TOK" https://naozhi.example.com/metrics | head
# TYPE naozhi_dispatch_message_total counter
naozhi_dispatch_message_total 1284
```
