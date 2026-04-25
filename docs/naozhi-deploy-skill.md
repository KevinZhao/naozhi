---
name: naozhi-deploy
description: review → build → test → deploy → push 一体化工作流（naozhi 项目）。当用户要求部署/发布/上线/重启服务 naozhi 时启用。
---

# naozhi 部署工作流 skill

> **安装方法**：把这份文件另存为 `~/.claude/skills/naozhi-deploy/SKILL.md`
> （或项目本地 `.claude/skills/naozhi-deploy/SKILL.md`，但 `.claude/` 在 `.gitignore` 内不会随 repo 分发）。
> Claude Code 启动时会自动发现。

按固定顺序完成 review → 构建 → 测试 → 提交推送 → 部署 → 验证 六步。每步失败即停止并向用户报告，绝不跳过测试或用破坏性手段规避失败。

> 仅对项目 `/home/ec2-user/workspace/naozhi` 生效。其他项目不要套用此 skill。

## 项目约束（硬规则）

1. **部署只能用 `make deploy` 或 `sudo systemctl restart naozhi`**。绝不手动 `kill + 启动`、绝不 `pkill naozhi` 后重新运行 bin。
2. **不要自行重启正在运行的服务**，除非用户明确要求。本 skill 只在用户已经要求"部署/发布/重启"时触发。
3. Go 二进制入口是 `/usr/local/go/bin/go`（$PATH 里没有 go，必须显式路径或前缀 `PATH=/usr/local/go/bin:$PATH`）。
4. systemd unit 以 `--config /home/ec2-user/workspace/naozhi/config.yaml` 启动，`Type=notify` + `WatchdogSec=120`。
5. 分支策略：日常在 `dev` 开发 + 推送；`master` 是 PR 目标分支，不要直接在 master 部署。
6. `docs/TODO.md`、`.claude/`、`bin/` 均在 `.gitignore` — 不会被 `git add -A` 误提。

## 执行步骤

### 1. Review 未提交改动

```bash
git status
git diff HEAD --stat
git log --oneline -5    # 对齐 commit message 风格
```

- **< 5 文件**：Claude 自读 diff 过一遍。
- **≥ 5 文件 或 跨多包 或 > 500 行**：用 `Agent(subagent_type=ecc:go-reviewer)` 并行 review。明确告诉 reviewer：**不要修文件，只报问题**，500 字内给 go/no-go 判断。
- 发现 HIGH/CRITICAL bug：**停止**，报告用户，让用户决定先修还是继续。

### 2. 构建 + vet

```bash
/usr/local/go/bin/go build ./... 2>&1
/usr/local/go/bin/go vet ./... 2>&1
```

两步都必须 `(no output)`。有错误立即停止、报告、不继续。

### 3. 测试（带 race）

```bash
/usr/local/go/bin/go test -race -timeout 180s ./... 2>&1 | tail -30
```

- 21 个包全 `ok`。允许 `(cached)`。
- 任何 `FAIL` 立即停止，拉失败详情报告用户。**不要**用 `-run` 或 `-skip` 跳过失败测试凑绿。

### 4. 提交 + 推送

**先提交再部署**。部署失败也能从 git 回溯。

```bash
git add -A
git status  # 人眼核对 staging
git commit -m "$(cat <<'EOF'
<type>(<scope>): <短描述>

<正文：why 而非 what，与近期 commit 风格对齐>

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
git push origin dev
```

commit message 规范：
- 参考 `git log --oneline -5` 近期风格。常见前缀：`feat`、`fix`、`perf`、`chore`、`refactor`、`docs`。
- 多轮 review 批量修复用 `chore(harden):` 或 `refactor(<area>):`。
- 标题 ≤ 70 字符；正文说明 why/影响，不要罗列 what。

### 5. 部署

```bash
PATH=/usr/local/go/bin:$PATH make deploy
```

`make deploy` 会：
1. `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w -X main.version=$(git describe)' -o bin/naozhi ./cmd/naozhi/`
2. `sudo systemctl restart naozhi`
3. 等 1 秒 `systemctl is-active --quiet naozhi` 判活。失败自动打印 `journalctl -u naozhi --no-pager -n 10` 并 `exit 1`。

### 6. 验证

```bash
sleep 2
systemctl show naozhi --property=MainPID,ActiveEnterTimestamp,NRestarts --no-pager
curl -sS http://127.0.0.1:8180/health
```

核对：
- `MainPID` 相对 restart 前改变（确认真的重启了）。
- `/health` 返回 `{"status":"ok","uptime":"<几秒>"}`。
- `NRestarts=0`（systemd 未因启动失败自动重试）。

`/health` 超过 5 秒无响应：
```bash
sudo journalctl -u naozhi --no-pager -n 30
```
**不要**自行 kill 再起。向用户报告现状。

## 回滚

```bash
git revert HEAD --no-edit
git push origin dev
PATH=/usr/local/go/bin:$PATH make deploy
```

不要用 `git reset --hard` 覆盖远端。

## 典型失败模式

| 症状 | 根因 | 处理 |
|---|---|---|
| `go: command not found` | $PATH 没有 go | 前缀 `/usr/local/go/bin/` 或 `PATH=/usr/local/go/bin:$PATH` |
| `go test FAIL` | 真 bug 或测试依赖外部 | 报告测试名给用户，**不要**加 `-skip` |
| `make deploy` 的 `is-active` 检查失败 | 启动即崩溃，通常 config 错误 | 读 journalctl 末尾 30 行，定位配置 / 依赖 |
| `/health` 超时 | sd_notify 没发 READY=1 或 WatchdogSec 触发 | 先看 journalctl，不要盲目重启 |
| `git push` 被拒 (non-fast-forward) | 远端有更新 | `git pull --rebase origin dev` 后重推；有冲突先解决 |

## 何时不应启动该 skill

- 用户只要求 "review" / "跑测试" / "构建" / "看看 diff" —— 按字面意思做，**不要**擅自推进到部署。
- 改动涉及 `config.yaml`、`deploy/naozhi.service`、`cmd/naozhi/setup.go` 等部署配置自身 —— 先让用户确认配置意图。
- 用户明确说 "只推送不部署" / "改天再上线" —— 提交 + 推送后停止。
- 当前在 `master` 分支 —— 先和用户确认是否真要在 master 直接部署。

## 示例

**用户: "review 未提交代码，构建、测试、重启服务、推送"**

节奏：
1. 一句话："开始走一套部署流程。"
2. 并行 `git status` + `git diff HEAD --stat`。
3. 决定 review 方式：>5 文件 → 并行 subagent；否则自读。
4. 报告 review 结论（一两句话）。
5. 串行：build → vet → race test → commit → push → make deploy → health curl。
6. 每步通过后一句话更新。
7. 结束：一句话总结（版本号 + /health 输出 + 新 MainPID）。

**用户: "部署一下当前改动"**（无 review 要求）

跳过 review 步骤，但 build/test 不能省。
