# Phase 4 Smoke Test Checklist

> **目的**：取代"55 次手工冒烟"非确定性。每个 server-split phase PR 必须按本 checklist 执行 + PR description 附截图链接（gist 或 PR comment 上传）。
>
> **执行者**：PR author 自己跑（不依赖 reviewer）。
>
> **环境**：本地 dev（`bin/naozhi --config config.yaml`）或 staging EC2。生产环境**不**做冒烟。

---

## 1. 准备

```bash
# 编译当前 PR 分支
make build

# 启动（背景）
bin/naozhi --config config.yaml > /tmp/naozhi-smoke.log 2>&1 &
NAOZHI_PID=$!

# 等待 ready（最多 5s）
for i in {1..10}; do
  curl -sf http://localhost:8080/health >/dev/null && break
  sleep 0.5
done

# 截图保存目录
mkdir -p ~/.naozhi/smoke/<phase-N>/
```

---

## 2. Dashboard checklist

每条都要：① 操作 → ② 期望 → ③ 截图保存。

### 2.1 登录

- [ ] **Op**: 浏览器访问 `http://localhost:8080/dashboard`
- [ ] **Expect**: 200，cookie 设置（开发者面板 Network 面板 cookie 列有 `naozhi_session`）
- [ ] **Save**: `~/.naozhi/smoke/<phase-N>/01-login.png`

### 2.2 sessions 列表

- [ ] **Op**: 登录后默认进入 sessions 视图
- [ ] **Expect**: 列表加载 < 1s（Network → `/api/sessions` 200 响应时长）
- [ ] **Save**: `02-sessions.png` (Network 面板含响应时长)

### 2.3 cron 面板 CRUD

- [ ] **Op-1**: 切到 cron 面板，点"新建"，填入 `* * * * *` + agent + workspace
- [ ] **Expect**: 创建成功，列表新增一条
- [ ] **Save**: `03a-cron-create.png`

- [ ] **Op-2**: 点新建 job 的"暂停"
- [ ] **Expect**: 状态变 paused
- [ ] **Save**: `03b-cron-pause.png`

- [ ] **Op-3**: 点"恢复"
- [ ] **Expect**: 状态变 active
- [ ] **Save**: `03c-cron-resume.png`

- [ ] **Op-4**: 点"立即触发"
- [ ] **Expect**: 触发后 cron job 出现 run record（< 5s 内可见）
- [ ] **Save**: `03d-cron-trigger.png`

### 2.4 WebSocket 订阅

- [ ] **Op**: 浏览器开发者面板 Network → WS 面板，看 `/ws` 连接
- [ ] **Expect**: 1 个 ws 连接 alive，看到 `subscribed` / `event` / `pong` 帧
- [ ] **Save**: `04-ws-frames.png`

### 2.5 interrupt + 关闭 session

- [ ] **Op-1**: sessions 列表里点一个活跃 session 的"interrupt" 按钮
- [ ] **Expect**: 200，session 显示 interrupted 状态
- [ ] **Save**: `05a-interrupt.png`

- [ ] **Op-2**: 点关闭 session
- [ ] **Expect**: session 从列表消失（被移到 retired）
- [ ] **Save**: `05b-close.png`

---

## 3. IM checklist（如配置了飞书）

### 3.1 普通消息

- [ ] **Op**: 飞书发"hello"
- [ ] **Expect**: 收到 AI 回复
- [ ] **Save**: `06-feishu-reply.png`

### 3.2 cron 命令

- [ ] **Op**: 飞书发 `/cron list`
- [ ] **Expect**: 收到 cron job 列表
- [ ] **Save**: `07-cron-list.png`

### 3.3 project 命令

- [ ] **Op**: 飞书发 `/project naozhi`
- [ ] **Expect**: 收到 workspace 切换确认
- [ ] **Save**: `08-project-switch.png`

---

## 4. 收尾

```bash
# 关闭 naozhi
kill $NAOZHI_PID
wait $NAOZHI_PID

# 检查日志没有 panic / fatal
grep -iE "panic|fatal|FATAL" /tmp/naozhi-smoke.log && echo "❌ 发现错误日志，附 /tmp/naozhi-smoke.log 到 PR" || echo "✅ 日志清洁"

# 截图汇总到 PR
ls -la ~/.naozhi/smoke/<phase-N>/
```

PR description 模板：

```markdown
## 冒烟测试

按 [docs/ops/phase4-smoke-test.md](docs/ops/phase4-smoke-test.md) 跑：
- [x] 2.1 登录 → ![01](https://gist.github.com/.../01-login.png)
- [x] 2.2 sessions 列表 → ![02](https://gist.github.com/.../02-sessions.png)
- [x] 2.3 cron CRUD (4 步) → ![03a-d](https://gist.github.com/.../03-cron.png)
- [x] 2.4 WS 订阅 → ![04](https://gist.github.com/.../04-ws-frames.png)
- [x] 2.5 interrupt + 关闭 → ![05a-b](https://gist.github.com/.../05-session.png)
- [x] 3.* IM 三步 → ![06-08](https://gist.github.com/.../06-08-im.png)
- [x] 日志无 panic / fatal
```

---

## 5. 失败处理

任一步骤失败：

1. **不要**强行 push PR
2. 把失败截图 + naozhi 日志贴到 PR comment
3. 修复或回退（按 [server-split-phase4-design.md §七](server-split-phase4-design.md#七回滚预案v02-新增) 流程）
4. 重新跑完整 checklist

---

## 6. 维护

- 新增 dashboard 路由 / IM 命令时：本文件加对应 checklist 条目
- 截图老旧（UI 改版）时：phase 推进 PR 同时更新对应截图
