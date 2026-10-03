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
| `codesign` | 非 darwin；或签名身份固定（leaf / Developer ID），升级后 macOS 授权保留 | ad-hoc 签名，每次升级都会重新弹文件夹授权（见 [macos-codesign.md](macos-codesign.md)）/ 读不到签名 | - |
| `systemd` | `systemctl is-active = active` | 非 Linux 或 systemctl 不存在 | 服务不活跃 |
| `http /health` | 返回 200（有 token 时摘要 status/uptime/version） | - | 不可达 / 非 200 |
| `auth` | token 通过 `/api/sessions` 200 | 无 token / 响应码意外 | token 被 401/403 |
| `cli runtime` | 服务端找得到默认 CLI 二进制（`cli_available=true`，仅 stat） | - | `cli_available=false`，新会话起不来 |
| `platforms` | 列出已注册的 IM 平台（只是注册，不代表已连上） | 没有任何平台（dashboard-only） | - |
| `eventlog writer` | `writer_alive=true`；或该子系统未启用（skipped） | - | `writer_alive=false`，事件没落盘 |
| `attachment tracker` | 同上 | - | `writer_alive=false`，附件元数据没记录 |
| `dispatch` | 有成功回复（显示多久前）/ 还没消息或刚启动 | 自启动起只有失败没有成功；或配了平台、启动超 10 分钟仍零条 IM 入站（平台可能没连上） | - |
| `pprof` | `/api/debug/pprof/` 200 | 403（远端调用 / hardening 生效）或意外码 | - |
| `state dir` | `~/.naozhi` 可写 | 目录不存在（首次运行） | 存在但不可写 / 非目录 |
| `zero-downtime` | `naozhi-shim-*.scope` 有 ≥1 | 0 个 scope（sudoers hardening 未生效） | systemctl list-units 失败 |

`cli runtime` 到 `dispatch` 五项和 `config-drift` 读的是同一次带 token 的 `GET /health`（整次 doctor 只发一次）。没有 token、token 被拒或 `/health` 不可达时，这五项各输出一行 `skipped (…)`，不计 fail。`/health` 的 `platforms` 只是启动时注册的名字，没有连接状态，所以「平台没连上」只能从 `dispatch` 的入站计数推断。

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
✓ http /health           status=ok uptime=1h3m28s version=v0.0.3-31-g8b832fa-dirty
✓ auth                   token accepted (/api/sessions 200)
✓ cli runtime            the server finds its default CLI binary (cli_available=true)
✓ platforms              registered: feishu (registration only, not a connection state)
✓ eventlog writer        writer alive (queue 0/1024, dropped 0)
✓ attachment tracker     writer alive (queue 0/256, dropped 0)
✓ dispatch               last successful reply 4m12s ago · messages=37 reply_errors=0 send_fails=0
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

都没有 → `auth` / `pprof` 检查降级为 warn，`cli runtime` 等鉴权段检查输出 skipped（都不算 fail）。

## 非零停机场景的 `zero-downtime` 解读

`zero-downtime` 检查通过 `systemctl list-units --type=scope | grep naozhi-shim-` 数 scope 数量。

- **有 scope**：说明 sudoers hardening 生效（或曾经生效，scope 一旦建立就持续存在直到 shim 退出）。[`docs/ops/sudoers-hardening.md`](sudoers-hardening.md) 是否安装决定未来 restart 能否新增 scope
- **0 scope**：sudoers 没配 / 尚无任何活跃 shim。发一条消息让 shim spawn 一次再跑 doctor 即可分辨两者
- **`systemctl list-units` 失败**：通常非 Linux 或 systemd 用户空间不可达

## 不在本期

- 深度探测（JSON 结构校验 /api/sessions 响应形状、cron job 执行历史）—— doctor 保持"顶层快照"语义，深度分析走 pprof + journalctl
- 自动拉 pprof heap/goroutine snapshot —— 想要就用 [`docs/ops/pprof.md`](pprof.md)
- 监控级集成（Prometheus exporter）—— 单独立项
