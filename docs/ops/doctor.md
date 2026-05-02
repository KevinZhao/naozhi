# naozhi doctor

一条命令拉取 naozhi 实例的健康快照，覆盖从二进制、systemd、HTTP、认证到零停机重启（sudoers hardening）各层。适用于：

- **第一现场排障**：SSH 到宿主打 `naozhi doctor`，20 秒看完所有常见故障点
- **部署回归**：`make deploy` 后自动跑，任一项 `fail` 就退出非零
- **监控探活**：`--json` 每行一个 finding，drop 到 Datadog/Loki

## 用法

```bash
# 最简：用默认 127.0.0.1:8180 + 自动发现 token
naozhi doctor

# 自定义端口（多实例 / 端口转发）
naozhi doctor --addr http://127.0.0.1:9180

# 显式 token（避免从 ~/.naozhi/env 读）
NAOZHI_DASHBOARD_TOKEN=xxx naozhi doctor

# JSON 一行一条，monitoring-friendly
naozhi doctor --json | jq -r 'select(.level=="fail") | .detail'

# 短超时（CI smoke）
naozhi doctor --timeout 2s
```

## 检查项

| 类别 | 通过 | 警告 | 失败 |
|---|---|---|---|
| `binary` | 能解析自身路径 | 路径不可读 | - |
| `systemd` | `systemctl is-active = active` | 非 Linux 或 systemctl 不存在 | 服务不活跃 |
| `http /health` | 返回 200 + JSON | - | 不可达 / 非 200 |
| `auth` | token 通过 `/api/sessions` 200 | 无 token / 响应码意外 | token 被 401/403 |
| `pprof` | `/api/debug/pprof/` 200 | 403（远端调用 / hardening 生效）或意外码 | - |
| `state dir` | `~/.naozhi` 可写 | 目录不存在（首次运行） | 存在但不可写 / 非目录 |
| `zero-downtime` | `naozhi-shim-*.scope` 有 ≥1 | 0 个 scope（sudoers hardening 未生效） | systemctl list-units 失败 |

## 退出码

- `0` — 全部 pass 或仅 warn
- `1` — 至少 1 个 fail（CI 友好，`|| exit 1` 直接传播）
- `2` — flag 解析错误

## 示例

生产正常态：

```
$ naozhi doctor
✓ binary                 /home/ec2-user/naozhi/bin/naozhi · version=v0.0.3-31-g8b832fa · linux/arm64
✓ systemd                active · MainPID=2730834 · NRestarts=0 · ActiveEnterTimestamp=Wed 2026-04-29 19:32:51 UTC
✓ http /health           {"status":"ok","uptime":"1h3m28s","version":"v0.0.3-31-g8b832fa-dirty"}
✓ auth                   token accepted (/api/sessions 200)
✓ pprof                  reachable at http://127.0.0.1:8180/api/debug/pprof/
✓ state dir              /home/ec2-user/.naozhi writable
✓ zero-downtime          2 shim scope(s) active (sudoers hardening is working)
```

服务 down：

```
$ naozhi doctor
✓ binary                 /home/ec2-user/naozhi/bin/naozhi · version=dev · linux/arm64
✗ systemd                naozhi.service is "inactive" (expected active)
✗ http /health           http://127.0.0.1:8180/health unreachable: dial tcp 127.0.0.1:8180: connect: connection refused
⚠ auth                   no token; auth-scoped checks skipped
...
```

退出码 1 —— `make deploy && naozhi doctor || exit 1` 直接失败出局。

## Token 查找顺序

1. `--token` flag
2. `NAOZHI_DASHBOARD_TOKEN` 环境变量
3. `DASHBOARD_TOKEN` 环境变量（legacy 别名）
4. `~/.naozhi/env` 文件扫描 `NAOZHI_DASHBOARD_TOKEN=` 或 `DASHBOARD_TOKEN=` 行

都没有 → `auth` / `pprof` 检查降级为 warn（不算 fail）。

## 非零停机场景的 `zero-downtime` 解读

`zero-downtime` 检查通过 `systemctl list-units --type=scope | grep naozhi-shim-` 数 scope 数量。

- **有 scope**：说明 sudoers hardening 生效（或曾经生效，scope 一旦建立就持续存在直到 shim 退出）。[`docs/ops/sudoers-hardening.md`](sudoers-hardening.md) 是否安装决定未来 restart 能否新增 scope
- **0 scope**：sudoers 没配 / 尚无任何活跃 shim。发一条消息让 shim spawn 一次再跑 doctor 即可分辨两者
- **`systemctl list-units` 失败**：通常非 Linux 或 systemd 用户空间不可达

## 不在本期

- 深度探测（JSON 结构校验 /api/sessions 响应形状、cron job 执行历史）—— doctor 保持"顶层快照"语义，深度分析走 pprof + journalctl
- 自动拉 pprof heap/goroutine snapshot —— 想要就用 [`docs/ops/pprof.md`](pprof.md)
- 监控级集成（Prometheus exporter）—— 单独立项
