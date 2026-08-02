# Security Policy

## 上报渠道

**请不要公开开 issue 上报安全问题。** naozhi 会 spawn 本机 AI CLI 并持有
IM 平台凭据、dashboard token，公开的漏洞细节会直接把自托管用户暴露出去。

请用 GitHub 的私密漏洞上报：

**<https://github.com/KevinZhao/naozhi/security/advisories/new>**

该入口只有仓库维护者可见，可以在里面附 PoC、日志和补丁草案。

## 支持范围

本项目是单人自托管定位、`v0.x` 阶段，只维护**最新 release**
（当前 `v0.0.70`）。请先在最新版复现再上报 —— 旧 tag 不做回溯修复。

## 上报请包含

- 版本（`naozhi --version` 或 commit SHA）
- 部署形态：本地运行 / systemd / 反向节点（`upstream`）/ 是否挂公网 ALB
- 相关配置（**脱敏**：不要贴真实 token、`app_secret`、cookie）
- 复现步骤，能给失败的测试用例或 curl 更好

## 威胁模型速览

有助于判断某个行为算不算漏洞：

- naozhi 的定位是**单用户自托管**，dashboard 不是多租户系统。"登录后的用户能
  读到本机文件"不算越权 —— 被包裹的 CLI 本身就有 shell 权限。
- 相对地，**未认证**就能触达的状态变更 / 数据读取、认证绕过、token 泄漏、
  路径逃出 workspace 根、`/ws` 与 `/ws-node` 的鉴权缺口、把 dashboard 暴露给
  非预期来源，这些都算。
- `cli.args` 里的 `--dangerously-skip-permissions` 是刻意的部署选择，不是漏洞；
  但任何让**远端输入**注入到 CLI argv / 宿主命令行的路径算。

## 处理方式

修复会随常规 release 发出。BSL 1.1 授权下商用部署的用户如需提前知会，
在私密上报里说明。
