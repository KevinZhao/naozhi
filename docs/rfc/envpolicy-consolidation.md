# RFC: envpolicy — 统一分散的 Claude env allow/deny 策略

> **状态**: Draft v1（待评审）
> **作者**: naozhi team (cron-cr)
> **创建**: 2026-06-04
> **范围**: 把分散在三处的 Claude 子进程环境变量过滤策略中**已被验证可安全共享的叶子校验器与 per-backend 凭据矩阵**抽取到一个新的 `internal/envpolicy` 包，先消除重复实现、统一安全不变量的单一来源；**不**在本轮统一三处的完整 allow/deny 列表语义。
> **关联 issue**: #891（R239-ARCH-E；owner 已 reopen，确认与 #634/R244 无关）
> **关联代码**:
> - `internal/sysession/env.go`（`validateBaseURLValue` :39、`isSafeProfileValue` :73、`envAlwaysPassthrough` :102、`envCredsForBackend` :212、`detectBackendFromEnv` :190、`filterEnv` :252）
> - `cmd/naozhi/main_claude_settings.go`（`validateClaudeBaseURLEnv` :252、`filterClaudeEnv` :185、`claudeEnvAllowedPrefixes` :44、`awsEnvDenyList` :85、`claudeEnvDenyList` :75、`proxyEnvKeys` :61、`claudeBaseURLEnvKeys` :241）
> - `internal/shim/manager.go`（`shimEnvAllowedPrefixes` :1163、`filterShimEnv` :1303、`shimEndpointEnvDropped` :1476、`shimProfileEnvDropped` :1390、`shimCredPathEnvDropped` :1422）
> - 安全不变量：R214-SEC-3 / R219-SEC-3（shim 拒绝 `ANTHROPIC_`/`CLAUDE_` 通配前缀）、R040034-SEC-4（#1400，per-backend 凭据矩阵）、R20260603-SEC-8（#1660，`CLAUDE_` kill-switch 拒绝）、R090031-SEC-1（#1687，base-URL SSRF guard）、R20260602-SEC-1（#1576）、R20260603-SEC-1（#1617，profile 校验）

---

## 1. Background & problem

naozhi 在三个独立边界上各自实现了"哪些环境变量可以流入 Claude CLI / 其 Bash 工具子进程"的策略。这些子进程具备 `--dangerously-skip-permissions` 级别的 Bash 访问，攻击者可控的 prompt 内容能驱动 `env | grep SECRET` 之类的读取，因此每个边界都是一个真实的凭据外泄面。

三处实现（已用 codegraph + Read 在 2026-06-04 的 HEAD 上核实，行号准确）：

1. **`cmd/naozhi/main_claude_settings.go`（父进程 env 注入）**
   `applyClaudeEnvSettings`（:284）从 `~/.claude/settings.json` 的 `env` 段读取，经 `filterClaudeEnv`（:185）注入 naozhi 自身进程。策略 = **前缀 allowlist**（`claudeEnvAllowedPrefixes` :44）+ 两条 denylist（`awsEnvDenyList` :85、`claudeEnvDenyList` :75）+ 值校验（NUL/换行/4096B）+ base-URL SSRF guard（`validateClaudeBaseURLEnv` :252）+ proxy guard。

2. **`internal/shim/manager.go`（shim/CLI 子进程 env）**
   `filterShimEnv`（:1303）按 **`KEY=` 前缀**匹配（`shimEnvAllowedPrefixes` :1163，注意条目带尾随 `=`），叠加单条目 4KiB 上限、endpoint/profile/cred-path 三个 drop 判定（`shimEndpointEnvDropped` :1476、`shimProfileEnvDropped` :1390、`shimCredPathEnvDropped` :1422）。这里刻意**不**用 `ANTHROPIC_`/`CLAUDE_`/`AWS_` 通配前缀（R214-SEC-3 / R219-SEC-3），改为逐键显式列举。

3. **`internal/sysession/env.go`（sysession Runner 子进程 env）**
   `filterEnv`（:252）= **精确集合 allowlist**（`envAlwaysPassthrough` :102）+ **per-backend 凭据矩阵**（`detectBackendFromEnv` :190 → `envCredsForBackend` :212，R040034-SEC-4）+ profile 校验（`isSafeProfileValue` :73）+ base-URL SSRF guard（`validateBaseURLValue` :39）。

**为何是问题（可复现）**：

- **重复实现，已经漂移**。`validateBaseURLValue`（sysession :39）与 `validateClaudeBaseURLEnv`（cmd :252）目前**逐字节相同**（loopback http 放行、非 loopback http 拒绝、非 https 拒绝），各自注释里都写"Mirrors …"。这种"靠人工镜像维护"的 SSRF guard 已经是已知脆弱点：任何一处改 IMDS 拦截逻辑，另两处要靠 reviewer 记得同步。profile 正则 `^[A-Za-z0-9_-]{1,64}$` 同样在 sysession（:18 `reProfileValue`）和 shim（`shimProfileEnvDropped` 内）各写一份。
- **安全不变量没有单一来源**。R040034-SEC-4 的 per-backend 凭据矩阵（哪些 cred key 属于 Anthropic / Bedrock / Vertex）只活在 sysession；shim 与 cmd 用的是"逐键列举 / 前缀+denylist"近似，三者对"Bedrock 部署不得泄漏 `ANTHROPIC_API_KEY`"这一同一条不变量给出了**三种不同强度**的执行。审计时无法在一个文件里回答"某个 backend 下，最终允许哪些 cred 流入 CLI"。
- **新增一个 cred key / 一个危险 switch 要改三处**。例如未来 Anthropic 发新的 `ANTHROPIC_*` token，需要分别评估三处。issue #891（R239-ARCH-E，priority:p1）正是要求 `internal/envpolicy` 收敛此分散。

---

## 2. Goals & non-goals

**Goals**

- G1：建立 `internal/envpolicy` 包，作为**叶子级安全校验器**与 **per-backend 凭据矩阵**的单一权威来源。
- G2：把当前逐字节重复的 `validateBaseURLValue` / `validateClaudeBaseURLEnv` 合并为一个导出函数 `envpolicy.ValidateBaseURLValue`，sysession 与 cmd/naozhi 改为调用它。
- G3：把 profile 正则 / `isSafeProfileValue` 合并为 `envpolicy.IsSafeProfileValue`，sysession（以及后续 shim）调用它。
- G4：把 per-backend 凭据矩阵（`backendMode`、`envCredsForBackend`、`allCredKeys`、`detectBackendFromEnv`、`envTruthy`）上移到 `envpolicy`，sysession 调用，保持 R040034-SEC-4 行为逐位不变。
- G5：所有迁移**行为零变化**——现有 pin 测试（见 §4）必须不改一行断言即通过。

**Non-goals（防 scope creep）**

- NG1：**不**统一三处的完整 allow/deny *列表语义*。三个边界的匹配模型本质不同（精确集合 vs 前缀 vs `KEY=` 前缀）且各自承载了刻意的安全取舍（shim 拒绝通配前缀是 R214-SEC-3 的硬要求；sysession 精确集合是 R040034-SEC-4 的载体）。强行合并列表会把三条不同的安全姿态揉成一条，是净风险。完整列表统一留待后续 phase，且必须各自独立评审。
- NG2：**不**改变 shim 的 `KEY=`-前缀匹配机制或其 endpoint/profile/cred-path drop 判定的调用结构（本轮 shim 至多复用 `envpolicy.IsSafeProfileValue` 这一叶子，且仅在能证明行为等价时；若有任何风险则 shim 完全不动，留 phase 2）。
- NG3：**不**引入运行时 config flag / 环境变量来切换策略。
- NG4：**不**改任何 on-disk 格式、settings.json schema、session 文件布局。
- NG5：**不**改 `applyClaudeEnvSettings` 的"shell-set 优先 / 只补不覆盖"语义，也不动 `--setting-sources ""`（Runner）/ `--setting-sources user`（cc child）这条红线。

---

## 3. Alternatives considered

**方案 A（选中）：自底向上抽取叶子 + 凭据矩阵，三个 caller 保留各自的列表与匹配逻辑。**
新建 `internal/envpolicy`，仅放无副作用、无依赖的纯函数与数据：base-URL 校验、profile 校验、backend 检测 + per-backend cred 矩阵。三个边界继续各自持有 allow/deny 列表，只把"叶子判断"委托给新包。优点：每个迁移点都能用现有 pin 测试逐位验证等价；R214-SEC-3（shim 不用通配前缀）等边界特定不变量原地不动；增量可独立 land。缺点：列表仍三份（但这是 NG1 的有意决定）。

**方案 B（否决）：一次性把三处合并为单一 `envpolicy.Filter(env, profile)` 策略引擎，三个 caller 全部改为传 profile 参数调用。**
需要把"精确集合 / 前缀 / `KEY=`-前缀"三种匹配模型统一进一个引擎，并为每个 caller 注入差异（shim 的 4KiB 上限、cmd 的 settings.json 值校验、sysession 的 per-backend gate）。这是跨包接口重设计 + god-policy 对象，单 PR 远超 150 行，且任何等价性回归都会同时威胁三条安全不变量。属于"必须先评审"类，不在本轮落地。

**方案 C（否决）：仅文档化"三处必须手工同步"，加注释交叉引用，不动代码。**
成本最低但没解决 #891 的根因（漂移仍可能发生，注释不是编译期保证）。已有的"Mirrors …"注释正是 C 的现状，证明它不足。

A 胜出：它在"消除最危险的重复（逐字节镜像的 SSRF guard）"与"不动三条边界特定安全不变量"之间取得最优，且每一步都可被既有 pin 测试守住。

---

## 4. Test strategy

**新增 unit（`internal/envpolicy/*_test.go`）**：把校验器移过去时，连同其测试表一起迁移并扩充：

- `TestValidateBaseURLValue`：https 放行、loopback http（`localhost` / `127.0.0.1` / `::1`）放行、非 loopback http 拒绝、非 http(s) scheme 拒绝、空值放行、解析失败拒绝、IMDS `http://169.254.169.254` 拒绝（SSRF 关键用例）。
- `TestIsSafeProfileValue`：合法名、含 `/`、含 `..`、含 `;`/`$()`、空、>64 字符 各一例。
- `TestEnvCredsForBackend` / `TestDetectBackendFromEnv` / `TestEnvTruthy`：覆盖 Anthropic/Bedrock/Vertex 三态、Bedrock 优先于 Vertex、`0`/`false`/空 视为 off、`allCredKeys` 为三集合并集。

**Regression / 等价性（核心防回归手段）**：迁移后**不改**以下既有 pin 测试的任何断言，全部必须绿：
- `internal/sysession/env_test.go`：`TestFilterEnv_CredsGatedByBackend`（:190）、`TestFilterEnv_BaseURLSSRFGuard`（:458）、`TestFilterEnv_BaseURLGuardNotBypassedByPrefixAllowlist`（:492）、`TestFilterEnv_AWSProfileValidation`（:311）、`TestFilterEnv_DropsSecretsByDefault`（:367）等全 13 个用例。
- `cmd/naozhi/main_claude_baseurl_env_test.go`：`TestValidateClaudeBaseURLEnv`、两处 `filterClaudeEnv` 用例（:47/:98/:120）。
- `internal/shim/filter_env_*_test.go`（profile/endpoint/credpath/oversize 共 5 文件）：若本轮触及 shim 的 profile 叶子则必须全绿；若 shim 不动则它们天然不受影响。

**等价性专项**：在 `envpolicy` 包加一个 `TestValidateBaseURLValue_MatchesLegacyTable`，用与旧 sysession/cmd 测试**完全相同**的输入表跑新函数，断言逐例一致——这是把"两处曾逐字节相同"这一事实固化为编译期/测试期保证，防止合并后语义偷偷漂移。

**命令**：`go test -race ./internal/envpolicy/... ./internal/sysession/... ./cmd/naozhi/... ./internal/shim/...` 必须全绿；`go vet ./...` 与 gofmt/goimports（PostToolUse hook 自动）通过。覆盖率目标沿用项目 80%（新包纯函数易达 100%）。

---

## 5. Risk & rollback

**会 break 什么（按 caller）**：
- 若 `envpolicy.ValidateBaseURLValue` 与旧 `validateBaseURLValue` / `validateClaudeBaseURLEnv` 行为有任何细微差异（如 scheme 比较大小写、loopback 判定、空值处理），将直接改变 **SSRF guard** 的放行/拒绝边界——这是 R090031-SEC-1 / R20260602-SEC-1 / R20260603-SEC-1 守护的攻击面。缓解：§4 的等价性专项测试用旧输入表逐例比对；迁移采用"先复制实现进新包 + 跑等价测试，再让旧函数体改为转调"的两步法，每步独立验证。
- per-backend 凭据矩阵上移若改动了 `detectBackendFromEnv` 的 Bedrock>Vertex 优先级或 `allCredKeys` 并集，将破坏 R040034-SEC-4（Bedrock 部署泄漏 `ANTHROPIC_API_KEY`）。缓解：`TestFilterEnv_CredsGatedByBackend` 是该不变量的 pin 测试，迁移后不改断言。

**Load-bearing 不变量（点名）**：
- R214-SEC-3 / R219-SEC-3：shim **不得**用 `ANTHROPIC_`/`CLAUDE_`/`AWS_` 通配前缀。→ 本轮 shim 列表原地不动，envpolicy 不引入任何通配前缀辅助，避免给"通配很方便"留口子。
- R040034-SEC-4：per-backend cred 矩阵。pin 测试见上。
- R20260603-SEC-8：`claudeEnvDenyList` 的 `CLAUDE_` kill-switch 拒绝——本轮**不**迁移（仍是 cmd-only 语义），原地不动。
- 并发/锁：本改动只涉及纯函数与不可变 `map`/slice 全局，无新增锁、无新增 goroutine、无 on-disk 状态。`filterShimEnvOversizeWarnings`（atomic 计数）等并发结构不在迁移范围。

**回滚**：纯代码重构、无 schema/flag/on-disk 变更，`git revert` 单个 PR 即可完全回到迁移前状态，无数据迁移、无兼容窗口。

---

## 6. Observability

无新增 metric/dashboard。日志保持现状：三处 caller 的 `slog.Warn`（"base-URL env var rejected" / "AWS profile env var rejected" / "rejecting unsafe base_url" / shim oversized-entry 计数）**留在各 caller 内**，envpolicy 的叶子函数保持纯函数（返回 `error`/`bool`，不打日志），由 caller 决定日志措辞与 key 脱敏（`osutil.SanitizeForLog`）。这样既不改变现有日志输出（SLO 过滤器依赖的 message 文案不变），又保持新包可测、无 I/O 副作用。

---

## 7. Compatibility & migration

- **向后兼容**：完全兼容。无 on-disk 格式、无 settings.json schema、无 config flag 变更。`~/.claude/settings.json` 的读取与 `env` 段语义不变。
- **API/包边界**：`internal/` 包，无外部消费者，无 SemVer 约束。新增导出符号 `envpolicy.ValidateBaseURLValue` / `IsSafeProfileValue` / `BackendMode` / `EnvCredsForBackend` / `DetectBackendFromEnv` 等。
- **迁移路径**：源码级、对运维透明，无需重启外的任何操作；按既有 deploy playbook（build → test → `naozhi upgrade`）发布即可。
- **刻意保留的非对称**：cc child 走 `--setting-sources user` 直接读 settings.json，与父进程 `filterClaudeEnv` 视图不同（direct-user-settings RFC §7.1 已记录的有意非对称）——本 RFC 不改变该非对称，envpolicy 只服务父进程/Runner/shim 路径。

---

## 8. Rollout plan

单仓库纯重构，不分 flag、无灰度，一次性切换；但拆成可独立 land 的 Phase 1（本轮）与待评审的后续 phase。

### Phase 1（本轮，可独立安全落地）

**判定：可独立安全落地（hasLandablePhase1 = true）。** 理由：抽取的是无副作用纯函数 + 不可变数据，且每个迁移点都有既有 pin 测试逐位守住等价性；不触碰 god-object、不重设计跨包接口、不动 on-disk schema、不引入新基础设施。

**确切文件级改动清单**：

1. **新建 `internal/envpolicy/baseurl.go`**（约 30 行）
   `func ValidateBaseURLValue(v string) error`——把 `sysession/env.go:39` 的实现整体搬入（与 `cmd` :252 逐字节相同，合并为一份）。

2. **新建 `internal/envpolicy/profile.go`**（约 12 行）
   `reProfileValue` 正则 + `func IsSafeProfileValue(v string) bool`——搬自 `sysession/env.go:18/73`。

3. **新建 `internal/envpolicy/backend.go`**（约 55 行）
   `BackendMode` 枚举 + `BackendAnthropic/Bedrock/Vertex` 常量、`envCreds*` 三组 + `AllCredKeys` + `EnvTruthy` + `DetectBackendFromEnv([]string) BackendMode` + `EnvCredsForBackend(BackendMode) []string`——搬自 `sysession/env.go:142–221, 347–353`。

4. **改 `internal/sysession/env.go`**（净减约 −90 行，新增约 +15 行委托调用与类型别名）
   删除已上移的本地实现，改为调用 `envpolicy.*`；`filterEnv`（:252）内部逻辑与公开行为不变。可保留薄包装（如 `func isSafeProfileValue(v string) bool { return envpolicy.IsSafeProfileValue(v) }`）以最小化对 `filterEnv` 主体的改动。

5. **改 `cmd/naozhi/main_claude_settings.go`**（约 −20 行）
   `validateClaudeBaseURLEnv`（:252）改为转调 `envpolicy.ValidateBaseURLValue`（或直接替换调用点 :218/:228），删除重复实现。

6. **新建 `internal/envpolicy/{baseurl,profile,backend}_test.go`** + 等价性专项测试（测试代码，不计入生产行数预估）。

**不在 Phase 1**：`internal/shim/manager.go` 的 `filterShimEnv` / 三个 drop 判定原则上不动（NG2）；仅当能用既有 shim pin 测试证明 `shimProfileEnvDropped` 复用 `envpolicy.IsSafeProfileValue` 行为等价时，才在 Phase 1 末尾追加一处低风险委托（≤10 行），否则推迟到 Phase 2。

**Phase 1 生产代码净增预估**：新包约 +97 行（30+12+55），caller 净减约 −110 行，**净变化约 −13 行；新写生产代码约 100 行**（保守按"新增到新包的行数"计）。风险 **low**。

### Phase 2+（待独立评审，本轮不写代码）

- 评估 shim 列表与 sysession/cmd 列表的可共享子集（NG1 解禁需专门评审，因涉及 R214-SEC-3 通配前缀红线）。
- 评估是否把 cmd 的 `awsEnvDenyList` / `claudeEnvDenyList` / 值校验（NUL/换行/4096B）抽象为 envpolicy 可选规则。
- 若三处确需统一为策略引擎（方案 B），单独立 RFC。
