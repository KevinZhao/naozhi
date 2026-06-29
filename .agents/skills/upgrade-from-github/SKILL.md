---
name: upgrade-from-github
description: 从 GitHub release 拉取最新 naozhi binary 并部署。当用户要求升级、更新、拉新版本、naozhi upgrade 时触发。
disable-model-invocation: true
allowed-tools: Bash
---

# 从 GitHub release 升级 naozhi

## 当前状态

- 运行版本: !`curl -s http://localhost:8180/health 2>/dev/null | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("version","unknown"))' 2>/dev/null || echo "服务未运行"`
- launchd 状态: !`launchctl list com.naozhi.agent 2>/dev/null | head -3 || echo "未注册"`
- 磁盘 binary: !`ls -la /Users/zhaokm/workspace/naozhi/bin/naozhi 2>/dev/null || echo "缺失"`

## 操作前提

1. 这是 **macOS arm64**(Apple Silicon)。Intel Mac 改 `naozhi-darwin-amd64`,Linux 改对应 release 资源名
2. 服务由 launchd 管理(`com.naozhi.agent`),`naozhi upgrade` 内部会调 `launchctl kickstart -k` 自动重启
3. **不要本地 `go build`**。本 skill 只走 release 流程;本地编译产物 ldflags 没注入,version 会是 `dev`,导致 `naozhi upgrade` 必须 `--force` 才能升

## 执行步骤

### 1. 检查是否有新版本

```bash
/Users/zhaokm/workspace/naozhi/bin/naozhi upgrade --check-only
```

- 输出 `Already at the latest version (vX.Y.Z).` → 已最新,**停止并报告用户**
- 输出 `New version available: vA → vB` → 继续步骤 2
- 输出 `Running a dev build. Use --force...` → **当前是本地编译产物**,需要先讨论:
  - 用户确认要替换为 release → 步骤 2 加 `--force`
  - 否则停止报告

### 2. 执行升级

```bash
/Users/zhaokm/workspace/naozhi/bin/naozhi upgrade
```

内部依次:下载 `naozhi-darwin-arm64` → SHA-256 校验 → atomic rename `bin/naozhi` → `launchctl kickstart -k com.naozhi.agent`。

任何一步失败它会自动 rollback 备份,不会留下半残状态。

### 3. 验证升级生效

```bash
sleep 3 && curl -s http://localhost:8180/health | python3 -c 'import json,sys;d=json.load(sys.stdin);print("version:",d["version"]);print("uptime:",d["uptime"])'
```

uptime 应 < 10 秒(刚被 launchd 重启)、version 应是新 tag。

### 4. 验证进程映射的是新 binary

防止 launchd race(老进程 mmap 老 inode + mv 之后没生效的情况):

```bash
PID=$(launchctl list com.naozhi.agent | awk '/PID/{print $3}')
DISK_SIZE=$(stat -f%z /Users/zhaokm/workspace/naozhi/bin/naozhi)
PROC_SIZE=$(lsof -p "$PID" 2>/dev/null | awk '$4=="txt" && /bin\/naozhi$/{print $7}')
[ "$DISK_SIZE" = "$PROC_SIZE" ] && echo "OK: process maps new binary ($PROC_SIZE bytes)" || echo "MISMATCH: disk=$DISK_SIZE proc=$PROC_SIZE — run launchctl kickstart -k gui/$(id -u)/com.naozhi.agent"
```

`MISMATCH` 时执行提示里那条 kickstart 命令,再回到步骤 3 验证。

### 5. 提示用户清浏览器 Service Worker

dashboard 注册了 `/sw.js`,旧 SW 会劫持资源即使后端是新的也读老缓存。**升级后必须告诉用户手工清:**

```
Chrome 打开 http://localhost:8180/dashboard
Cmd+Option+I → Application → Service Workers → Unregister
        → Storage → Clear site data
关 DevTools,Cmd+Shift+R 硬刷新
```

PWA(MBP Naozhi.app)同步骤,在 PWA 窗口里开 DevTools。

## 失败处理

- **下载超时/网络错**: GitHub release 拉不动 — 让用户检查代理/VPN,不要回退到 `go build`
- **SHA-256 不匹配**: release 资源被改/坏。停止,让用户去 release 页面手动核对
- **launchctl kickstart 报 "Could not find service"**: launchd job 被卸载了 — 需要 `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.naozhi.agent.plist`,不在本 skill 范围,提示用户先恢复
- **`/health` 返回 connection refused**: 进程没起来。看 `~/.naozhi/logs/stderr.log` 报告错误,**不要**自动 rollback — 用户的 config 可能跟新 binary 不兼容(rare)
