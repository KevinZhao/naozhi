# RFC: naozhi 直接加载 user settings(移除 settings override 副本)

- 状态: Draft v3(architect + security 双评审通过;阻断项均已修订,残余风险记录在案)
- 日期: 2026-05-30
- 范围: 让 naozhi spawn 的 claude 子进程直接加载 `~/.claude/settings.json`(`--setting-sources user`),删除 `writeClaudeSettingsOverride` 副本生成与 env 白名单过滤,使 naozhi 内的 cc 与命令行 cc 行为完全一致、单一配置源、零额外配置。

## 1. 背景与问题(Background & problem)

### 1.1 现状机制

naozhi 启动 claude 时**不是**裸 `-p`,而是:

```
claude -p --setting-sources "" --settings ~/.naozhi/claude-settings.json ...
            ① 禁用标准来源           ② 改用一份过滤副本
```

`~/.naozhi/claude-settings.json` 由 `cmd/naozhi/main_claude_settings.go: writeClaudeSettingsOverride()` 生成 —— 它逐字复制 `~/.claude/settings.json`,但做两处过滤:

- **`filterHooks`**(`main_claude_settings.go:319`):剥掉"回调 naozhi HTTP 端口/名字"的 hook,防 `naozhi → cc → hook → naozhi` 死循环。
- **`filterClaudeEnv`**(`main_claude_settings.go:152`):env 块只放行 `ANTHROPIC_` / `CLAUDE_` / `AWS_`(再减 `awsEnvDenyList`)/ `*_PROXY` 前缀的键,**其余全部丢弃**。

并行还有 `applyClaudeEnvSettings`(`main_claude_settings.go:184`)把 settings.json 的 env 注入 naozhi 父进程(同样走 `filterClaudeEnv` 白名单)。

### 1.2 可复现症状

`~/.claude/settings.json` 里的 `ECC_HOOK_PROFILE=minimal`(以及任何非白名单前缀的 env)被 `filterClaudeEnv` 剥掉,导致 naozhi spawn 的 cc 子进程收不到它。实测证据:

- naozhi 实际生成的 `~/.naozhi/claude-settings.json` 的 `env` 块里 **ECC_\* 全部消失**(只剩 `ANTHROPIC_*`/`CLAUDE_*`/`AWS_REGION`)。
- naozhi 主进程 `/proc/<pid>/environ` 无任何 `ECC_*`。
- 后果:ecc plugin 的 hook 在命令行 cc 里走 `minimal` profile(4 个轻量 hook),但在 naozhi cc 里退回**默认 `standard` profile = 全量 hook**(含 `pre:config-protection` 拦 linter 配置编辑、`gateguard-fact-force` 阻挡每文件首次编辑、`post:quality-gate` 等),且每会话 +约 20k token。

更广义的问题:**naozhi cc 与命令行 cc 行为不一致**,根因是 env 白名单。任何用户在 settings.json 里新增的配置项(新 env、未来新顶层键)都可能因白名单/复制逻辑而在 naozhi 侧丢失,需要手动维护白名单。这违背用户诉求:"行为一致 + 单一配置源 + 零额外配置"。

### 1.3 为什么 override 当初存在

`--setting-sources ""` + override 的**唯一设计目的**是防 hook 死循环(`protocol_claude.go:90` 注释 "disable standard settings to avoid hook loops"; 见 `passthrough-mode.md:396`)。env 白名单是其搭载的安全加固,不是核心目的。

## 2. 目标与非目标(Goals & non-goals)

### 目标
- naozhi spawn 的 cc 直接加载 `~/.claude/settings.json`,与命令行 cc 行为一致。
- 单一配置源:用户只维护 `~/.claude/settings.json`,naozhi 零额外配置项、零白名单维护。
- 删除 `writeClaudeSettingsOverride` / `RefreshSettings` / override 文件 / env 白名单复制路径。

### 非目标
- 不改变 naozhi 的 IM webhook / dashboard API 鉴权(它们是死循环的真正防线,见 §5)。
- 不引入新的"naozhi 专属"配置项(与目标相悖)。
- 不动 kiro/ACP backend 的 settings 语义(它本就忽略 SettingsFile,见 `profile_kiro.go:11`)。
- **不改 `internal/sysession` Runner 的 `--setting-sources ""`**(决议,见 §5.4 / §7.1):AutoTitler 等 sysession daemon 由内部 Tick 驱动,**不经过 §5 的 HTTP 入口鉴权**,加载宿主 hook 有真实死循环风险(`runner.go:259-262` 记录过历史 dead-loop)。Runner 靠继承父进程 env 拿 Bedrock 鉴权,无加载 settings.json 的需求。仅 cc 主对话路径改 `user`。
- **不删 `applyClaudeEnvSettings`**(决议,见 §7.1):naozhi 父进程自身(transcribe 调 Bedrock、sysession Runner env 透传)依赖它把 settings.json env 注入 `os.Environ`,与 cc 子进程的 `--setting-sources user` 不重叠。

## 3. 备选方案(Alternatives considered)

### 方案 A(本 RFC 选定)— `--setting-sources user`,删除 override
naozhi 把 `--setting-sources ""` 改为 `--setting-sources user`,移除 `--settings <override>`。cc 像命令行一样加载 user 级 settings.json,自己应用 env 块,hook 作为 cc 子进程继承环境。
- 优点:彻底单一源、行为一致、零维护、删代码(净减)。
- 代价:不再有 `filterHooks` 这道源头护栏,死循环防御依赖 naozhi 入口鉴权(见 §5 论证其充分性)。

### 方案 B — 保留 override,env 白名单改黑名单
override 仍生成,但 env 默认全复制,只拦 AWS 认证源 + 密钥。
- 优点:保留 filterHooks 双保险。
- 缺点:仍有"副本/单一源"的概念裂隙;未来新顶层键仍可能漏复制;不满足用户"彻底"的诉求。

### 为何选 A
用户明确要"彻底"。经代码审计(§5),`filterHooks` 防的死循环在当前架构下已被入口鉴权切断,filterHooks 属历史双保险而非唯一防线。删除它不引入新的可达死循环路径,换来架构更干净(防御归网关入口,而非篡改用户配置)。

## 4. 测试策略(Test strategy)

### 4.1 单元测试(必须新增,Bucket A/C 回归门槛)
核心改动点是 `internal/cli/protocol_claude.go:90` 的**硬编码字面量** `"--setting-sources", ""`(这条独立于 `SettingsFile`/`RefreshSettings` 分支,必须显式改,见 §8 PR1)。

- **回归测试(必须先红后绿)**: 新增 `TestBuildArgs_SettingSourcesUser` —— 断言 `BuildArgs` 输出包含 `--setting-sources user` 且**不含** `--settings`。在旧代码上该测试失败(旧为 `""` + `--settings <file>`),新代码通过。
- **待更新的现有断言(architect 评审补全清单,不可遗漏)**:
  - `internal/session/router_test.go:1814`(固定 `--setting-sources ""`)
  - `cmd/naozhi/main_settings_test.go`(7+ 处直接调 `writeClaudeSettingsOverride`,如 :222/240/258/291/336/351/416;:330-331 断言 RefreshSettings-on-every-BuildArgs 契约失效)
  - `internal/cli/cli_test.go:407/440/465`(`TestClaudeProtocol_BuildArgs_RefreshSettings` 系列)
  - `internal/cli/backend/profile_test.go:256-298`(SettingsFile/RefreshSettings 透传断言)
  - 处理原则:这些测试改为断言**新 argv 行为**,而非继续测已停用的旧函数(否则"测试绿但生产已变"的盲区)。
- **sysession Runner 不改**(见 §2 非目标 / §5.4):`runnerImplBaseArgs` 保持 `--setting-sources ""`,其 `env_test.go` / runner 测试**不动**。RFC v1 曾误写"同步改 user",已据评审纠正。

### 4.2 死循环防御回归(关键,见 §5)
- **入口鉴权验证测试**:构造一个"hook 回调"形态的请求打 naozhi 端点,断言:
  - `POST /webhook/feishu` 无有效签名 → 401(已有 `transport_hook.go` 测试覆盖,复用/补强)。
  - `POST /api/sessions/send` 无 auth token → 401(dashboard auth 中间件,已有覆盖)。
- 这些测试证明:即使 cc 加载了一个回调 naozhi 的 hook,也无法驱动新对话 → 死循环不可达。

### 4.3 手动 / 集成验证
- build + `sudo systemctl restart naozhi`,从 dashboard 起一个会话,确认:
  - cc 子进程 `/proc/<pid>/environ` 含 `ECC_HOOK_PROFILE=minimal`(来自直接加载的 settings.json env)。
  - ecc hook 走 minimal(观察不再触发 `gateguard-fact-force` / `config-protection`)。
  - Bedrock 鉴权仍正常(`ANTHROPIC_BEDROCK_BASE_URL` 经 settings.json env 生效)。
- **`--setting-sources user` 行为契约验证(security 评审发现 4)**:用当前 cc 版本确认 `user` 值**确实且仅**加载 `~/.claude/settings.json`,**不加载** naozhi 工作目录下的 project `.claude/settings.json`(可在工作目录放一个写 echo 标记的 project hook,确认 cc 不触发它)。在 §7.3 记录验证时的 cc 版本号作为已知稳定起点(本机 cc 2.1.158)。

### 4.4 全量回归
`go build ./...` / `go test -race ./...` / `go vet ./...` 全绿;覆盖率不降。

## 5. 风险与回滚(Risk & rollback)

### 5.1 核心风险:hook 死循环
删除 `filterHooks` 后,若 settings.json 含回调 naozhi 的 hook,理论上可能 `naozhi → cc → hook → POST naozhi → 新对话 → cc → ...`。

**审计结论:此路径在当前架构下已被入口鉴权切断(死循环不可达)。** naozhi 所有能驱动 cc spawn 的入口:

| 入口 | 鉴权 | 裸 hook 回调能否触发新对话 |
|---|---|---|
| Feishu webhook `POST /webhook/feishu` | 验签 + 时间戳新鲜度 + nonce 防重放 + 零凭证拒绝(`transport_hook.go`),且仅 `connection_mode: webhook` 才注册 | 否(无 EncryptKey 签名 → 401) |
| Feishu 默认 websocket 模式(`feishu.go:283`) | 出站长连,不监听入站 HTTP | 否(无入站端点) |
| Dashboard API `/api/sessions/send` 等 | 全部 `auth(...)` 包裹(`dashboard.go:209`) | 否(配了 `dashboard_token` 时无 token → 401);**⚠️ no-token 模式例外见下** |
| 未鉴权端点 | 仅 `/health` `/livez` `/readyz`(`server.go:604`) | 否(不 spawn cc) |

因此 `filterHooks` 是历史双保险,真正的死循环防线是入口鉴权。删除 filterHooks 不新增可达的死循环路径。

> **⚠️ no-token 模式缺口(security 评审发现 3)**:当 `dashboard_token` 未配置时,`auth/handlers.go:258` 的 `IsAuthenticated` 直接返回 `true`(passthrough,`server.go:641` 启动有 warn)。此时 dashboard API 对任意本地调用者开放,一个回调 naozhi 的 hook **能**构造 JSON POST 到 `/api/sessions/send` 触发新对话 → 死循环防线失效。
> - 本项目实际部署**已配** `dashboard_token`(`config.yaml: dashboard_token: "${NAOZHI_DASHBOARD_TOKEN}"`),不受影响。
> - 但作为通用方案,**§5.2 的 `NAOZHI_CHILD` 自引用兜底对 no-token 部署从"可选"升级为"建议"**;或在文档强制要求配置 `dashboard_token`。

### 5.4 非 HTTP-入口的内部驱动路径(architect 评审补充)
§5.1 表格覆盖的是 HTTP 入口。naozhi 还有**内部定时驱动**的 cc spawn 路径,不经任何 HTTP 鉴权:
- `internal/sysession` daemon 的 Tick(AutoTitler 等);
- cron dispatcher 触发的会话。

这些路径**不靠入口鉴权规避死循环,而靠保持 `--setting-sources ""` 不加载宿主 hook**。`runner.go:259-262` 记录过 AutoTitler 因宿主 hook dead-loop 的历史事故。因此本 RFC 的决议(§2 非目标)是:**只有 cc 主对话路径改 `--setting-sources user`,sysession Runner 保持 `""`**。如此死循环全称论证闭合——主路径靠入口鉴权,内部驱动路径靠不加载 hook。

### 5.2 残余风险与缓解
- **webhook 模式 + 弱 EncryptKey**:若运营用 webhook 模式且密钥薄弱,理论缝隙。缓解:本 RFC 不依赖它,但建议**纵深防御兜底**(见 §8)——给 cc 子进程注入 `NAOZHI_CHILD` 环境标记,naozhi 入口识别自引用回调直接拒绝;或 per-session spawn 深度/速率限制。
- **no-token 部署**(见 §5.1 警告框):`NAOZHI_CHILD` 兜底升级为"建议",或文档强制 `dashboard_token`。
- **cc 写 settings.json 篡改 env(security 评审发现 2,P1→存疑)**:删 override 后 cc 子进程经 `--setting-sources user` 直读 settings.json env,失去 `awsEnvDenyList` 对 cc 的过滤。cc 有 Bash 工具 + `--dangerously-skip-permissions`,理论上能写 `~/.claude/settings.json` 注入 `AWS_ROLE_ARN`/`AWS_CONFIG_FILE` 等,影响下一次 cc spawn 的 AWS 鉴权链(横向持久化)。**缓解条件**:(a) IMDS/instance-role 部署下这些静态 AWS 变量本不在环境中,风险不可达;(b) 注意此攻击假设 cc 已被攻陆——而 cc 本就有完整 Bash 能力,该面非本 RFC 新增的"代码执行"面,只是 env 持久化通道。**记录为已知残余风险**,PR1 review checklist 须确认部署的 AWS 凭据来源(IMDS vs 静态 env)。
- **文档约束**:在 `CLAUDE.md` / 部署文档写明"`~/.claude/settings.json` 不应包含回调 naozhi 端口/名字的 hook",把隐性约束显式化。

### 5.3 回滚
改动集中在 `BuildArgs` 的 `--setting-sources` 取值 + 删除 override 调用。回滚 = revert 单个 commit,恢复 `--setting-sources ""` + override 生成。override 函数若一并删除,回滚需恢复 `main_claude_settings.go` 相关段落 —— 故**建议分两步 PR**:PR1 仅切换 `--setting-sources user` 并 stop 调用 override(保留函数体,标 deprecated);PR2 删死代码。这样 PR1 可快速回滚。

## 6. 可观测性(Observability)
- naozhi 启动日志增加一行:`slog.Info("claude settings: loading user settings directly", "mode", "user")`,替代原 override 生成日志。
- 移除 `writeClaudeSettingsOverride` 的 "dropping hook to prevent naozhi callback loop" / "keeping previous override" 日志(随函数删除)。
- 保留 `applyClaudeEnvSettings` 的父进程 env 注入日志(若该路径保留,见 §7)。

## 7. 兼容性与迁移(Compatibility & migration)

### 7.1 `applyClaudeEnvSettings`(父进程 env 注入)— 决议:保留(architect 评审定稿)
现状:naozhi 父进程读 settings.json env 并 `os.Setenv` 注入自身。这条路径**独立于** `--setting-sources`,删 override 不影响它。

**决议:必须保留**,证据:
1. `internal/transcribe/transcribe.go:59` `awsconfig.LoadDefaultConfig(...)` —— naozhi **父进程自身**调 Bedrock Transcribe,AWS SDK 默认凭据链从**进程 env** 解析。region 有 yaml 兜底(`us-east-1`),但**凭据无兜底**;非 EC2(静态 key 写在 settings.json env)部署强依赖 `applyClaudeEnvSettings`。cc 的 `--setting-sources user` 惠及不到父进程。
2. `internal/sysession` Runner 靠继承 `os.Environ` 拿 Bedrock 鉴权(`env.go:14-21`),而 `os.Environ` 由 `applyClaudeEnvSettings` 在启动时预填(`main_helpers.go:265-272`)。删它 Runner 直接 "Not logged in"。

**已知非对称(显式记录,通常无害)**:父进程经 `filterClaudeEnv` 白名单(`AWS_PROFILE` 等在 `awsEnvDenyList` 中被拒);cc 子进程经 `--setting-sources user` 拿到**完整** env。二者对同一 settings.json env key 的可见性可能不同。这符合预期(cc 本应看到完整 user 配置),但未来 debug transcribe 凭据问题时需记得这点。

### 7.2 on-disk 兼容
- `~/.naozhi/claude-settings.json` 不再生成/读取。残留旧文件无害(不再被引用);可在启动时清理或留存。建议留存(回滚友好),follow-up 清理。

### 7.3 project/local settings
选 `user`(非 `user,project,local`)避免 naozhi 在某工作目录下被该目录的 `.claude/settings.json`(如 naozhi 仓库自身的 project settings)意外影响,保持与"加载用户全局配置"的语义一致。**待评审确认**:是否需要 project 级(若 naozhi 按 project 切换 cwd 且希望继承 project settings)。MVP 取 `user`。

## 8. 落地计划(Rollout plan)

architect 评审采纳:**单 PR 完成行为切换 + 死代码删除**(不保留 deprecated 函数体)。理由:回滚靠 `git revert <commit>` 即可恢复整段;保留 dead 函数 + dead 测试会触发 unused lint、被 cron 死代码巡检标记,且制造"测试绿但生产已变"盲区。

**PR1(本次实现,原子 commit)**:
1. `internal/cli/protocol_claude.go:90` —— 硬编码 `"--setting-sources", ""` 改为 `"--setting-sources", "user"`(**核心改动点**,独立于 SettingsFile 分支)。
2. `internal/cli/protocol_claude.go:103-104` —— 移除 `--settings <SettingsFile>` 注入分支(连同 `SettingsFile`/`RefreshSettings` 字段 :50/:61、Clone :68-69、BuildArgs 顶部 RefreshSettings 调用 :74-78)。
3. `internal/cli/protocol_claude.go:192` —— 更新 `argDenyList` 中 `--setting-sources` 项注释(从 "pins \"\"" 改 "pins user")。
4. `internal/cli/backend/profile_claude.go:27-28`、`profile.go:121-131` —— 删 SettingsFile/RefreshSettings 透传。
5. `cmd/naozhi/main.go:123,162-165`、`main_init.go:101-104` —— 不再调 `writeClaudeSettingsOverride`、不再传 settingsFile/refreshSettings。
6. 删 `cmd/naozhi/main_claude_settings.go` 中:`writeClaudeSettingsOverride`、`filterHooks`、`filterClaudeEnv` 在 override 路径的使用、`isNaozhiCallbackHook`/`sanitizeLogCmd`/`addrPort`/`loopbackV4Re`/`filterHooks` 整套。**保留** `applyClaudeEnvSettings` 及其依赖的 `filterClaudeEnv`/`readClaudeSettingsRaw`(父进程 env 注入仍需,见 §7.1)。
7. **sysession Runner 不动**(`runnerImplBaseArgs` 保持 `""`,见 §2/§5.4)。
8. 测试:新增 `TestBuildArgs_SettingSourcesUser`;更新 §4.1 清单全部断言。
9. 文档:`router_core.go:822-823` 注释改为"cc 经 --setting-sources user 每次 spawn 自读";`CLAUDE.md` 记录"settings.json 勿放回调 naozhi 端口/名字的 hook"约束。
10. on-disk:`~/.naozhi/claude-settings.json` 不再生成;残留旧文件无害,留存(回滚友好)。

**PR2(可选纵深防御,仅 §5.2 评审认为必要时)**: `NAOZHI_CHILD` 自引用标记 + 入口识别,或 spawn 深度/速率限制。

非 flag-gated(行为切换简单且可单 commit 回滚)。
