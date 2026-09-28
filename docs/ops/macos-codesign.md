# macOS：让文件夹授权跨升级保留

## 现象

在 macOS 上用 launchd 跑 naozhi 时，系统频繁弹出「naozhi 想访问文稿/桌面/下载文件夹」或「由 OneDrive 管理的文件」之类的窗口。点了"允许"，升级后又会再弹。

## 原因

- **弹窗算在 naozhi 头上。** launchd 启动的是 naozhi，claude 会话是它的子进程。claude 执行的 `find ~`、`rg`、`ls ~/Library/CloudStorage` 等命令，TCC 都记作"naozhi 想访问"。
- **授权绑定签名身份。** TCC 按 binary 的 designated requirement 保存授权。release binary 是 ad-hoc 签名，requirement 是 `cdhash H"…"`，也就是内容哈希，每个新版本都会变，所以升级一次，所有授权就失效一次。

## 解决：固定签名身份（一次性）

```bash
scripts/macos-codesign-setup.sh            # 默认 ~/.local/bin/naozhi
```

脚本做两件事：

1. 在登录钥匙串里创建自签名的代码签名证书 `naozhi-local`（已存在则跳过），私钥只存在钥匙串里。
2. 用这个证书重签 binary，签名身份变成 `identifier "…" and certificate leaf = H"…"`，和内容无关。

然后手动完成：

1. 重启服务：`launchctl bootout` + `launchctl bootstrap`，不要用 `kickstart`。
2. 系统设置 → 隐私与安全性 → 完全磁盘访问权限 → `+` → 选中 naozhi binary。这一项覆盖文稿、桌面、下载、网络卷、可移动卷和其他 App 的数据。云盘（File Provider）按域单独授权，签名固定后每个域最多点一次允许。
3. `naozhi doctor` 的 `codesign` 一行显示 ✓。

证书显示 `CSSMERR_TP_NOT_TRUSTED` 不影响使用：TCC 只比对 requirement，不校验证书链。

## 之后的升级

`naozhi upgrade` 和 dashboard 的一键安装会读取当前 binary 的签名身份（identifier + leaf），在新 binary 替换上线前用同一身份重签，签完核对 requirement 一致（`internal/selfupdate/codesign.go`）。只处理 leaf 形式；ad-hoc、Developer ID 等其他形式原样保留。

签名失败（钥匙串里没有这个证书、超时等）时不会中断升级：新 binary 保持 ad-hoc 签名上线，日志里会有一条 WARN 并附带修复命令，这时授权会再丢一次。

手动替换 binary 时，要用同一身份签名，不要用 `codesign -s -`：

```bash
codesign -f -s naozhi-local --identifier <原 identifier> <binary>
```
