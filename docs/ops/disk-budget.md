# naozhi 磁盘预算 (Disk Budget)

naozhi 把所有状态放在 `~/.naozhi/` 下，不依赖外部数据库。下列 7 类路径会随
时间膨胀，operator 需要了解它们的规模和清理方式。启动时如果整个状态目录超过
**500 MiB**，naozhi 会打一条 `state directory large` warn log，提示阅读本文。
（500 MiB 阈值见 `cmd/naozhi/main.go` 的 `stateDirWarnMB`。）

## 各路径上限估算

| 路径 | 用途 | 典型规模 | 上限 / 增长因素 |
| --- | --- | --- | --- |
| `sessions.json` | Session store (所有活动会话 metadata) | 10 KB – 几 MB | 与 `session.max_procs` 成线性，通常 < 5 MB |
| `cron.json` | Cron 调度表 + 执行历史 | 几十 KB – 几 MB | 与 `cron.max_jobs` 和执行次数成线性 |
| `shims/*.json` | 每个 shim 子进程的重连状态 | 每文件 1–5 KB | 活跃 shim 数 × 约 2 KB |
| `attachments/` | 用户上传的 PDF / 图片 / 语音 | 未封顶 | **最大增长源**; 单个附件可达几十 MB |
| `events/*.log` + `events/*.idx` | Event log (Session 事件持久化) | 每 session 几 MB | 与 session 数 × 每 session turn 数 |
| `run/` | 运行时 PID / socket 文件 | < 1 KB | 常量级，随进程启停翻转 |
| `env` | 启动时写入的 secrets snapshot | < 4 KB | 常量 |

正常单节点稳态在 50–200 MiB 之间。超过 500 MiB 大概率是 `attachments/` 或
`events/*.log` 失控。

## 安全清理 (先停 naozhi)

```bash
sudo systemctl stop naozhi

# 清 attachments（按需保留最近 30 天）
find ~/.naozhi/attachments -type f -mtime +30 -delete

# 清老旧 event log（session 已关闭的 .log 可以删）
# 不要删当前活跃 session 对应的 .log / .idx — 启动后会重放失败
find ~/.naozhi/events -type f -mtime +90 -delete

sudo systemctl start naozhi
```

`sessions.json` / `cron.json` / `shims/` / `run/` / `env` **不要手工删除** —
会导致活动 session 丢失、cron 任务遗忘、shim 重连失败。

## 跟进

当前只做启动时一次性扫描 + warn。真正的配额执行 (quota)、log rotation、
按目录类型的独立上限在 TODO `RNEW-OPS-415` 跟踪。
