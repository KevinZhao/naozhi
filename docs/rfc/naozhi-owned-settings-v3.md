# RFC: naozhi 独立 settings（彻底隔离本地 ~/.claude/settings.json）

> **状态**: Draft v1（待评审）
> **作者**: naozhi team
> **创建**: 2026-07-11
> **前身讨论**: `docs/rfc/project-access-profile.md`（v1 auth overlay，已落地）· `docs/rfc/profile-config-v2.md`（v2 配置分层探讨，本 RFC 取代其 thinking/settings 部分）
> **一句话**: 今天 naozhi spawn 的 claude 通过 `--setting-sources user` **直接读操作者的 `~/.claude/settings.json`**（`direct-user-settings.md` 的决策）。本 RFC 反转该决策：让 naozhi 拥有**一份自己的、与本地彻底隔离的 settings**（首次从本地拷贝作种子，之后脱钩），使 naozhi 能配成与本地不同的档位（如 naozhi=thinking max、本地=high），且本地配错不波及 naozhi。UI 显式告知"naozhi 使用独立配置"并可查看 diff。

---

## 0. TL;DR

**现状**（已核实 `protocol_claude.go:106`）：naozhi 的 claude 子进程 `--setting-sources user`，即和命令行 `claude` **共用同一份** `~/.claude/settings.json`，单一配置源、零 naozhi 侧配置——这是 `direct-user-settings.md` PR1 的刻意设计（当时删掉了历史的 `writeClaudeSettingsOverride` 拷贝机制）。

**问题**：这份共享意味着 (1) naozhi 无法配成与本地不同——想让后台任务用更深的 thinking，会连带改操作者交互式 claude 的行为；(2) 本地 settings 被改坏，naozhi 下一个 session 立刻跟着崩，无隔离、无测试缓冲。

**本 RFC（方案 C，彻底隔离）**：

- naozhi 维护**一份自己拥有的 settings 文件**（如 `<dataDir>/naozhi-settings.json`），spawn claude 时走 `--setting-sources "" --settings <naozhi-settings.json>`（复活已被删除但 CLI 仍支持的 `--settings` 通路，`protocol_claude.go:229` 确认该 flag 仍在 allowlist）。
- 该文件**首次一次性从 `~/.claude/settings.json` bootstrap**（拷贝作种子，**剥掉 hook 段**防死循环），之后与本地**彻底脱钩**：本地再改不影响 naozhi，naozhi 改不影响本地。
- naozhi 只对**少数关键旋钮**（首个是 thinking effort）暴露 dashboard UI；整份文件其余部分是文本，高级用户自行编辑，naozhi 只负责加载。
- UI **显式告知**："naozhi 使用独立配置，与本地 ~/.claude/settings.json 不同步"，并提供只读 diff 与"用本地重新 bootstrap（覆盖，带二次确认）"。
- **作用域：全 naozhi 单份**（本轮）。per-profile 不同档是后续增强，不在本轮。

**明确不做**：不合并本地（不是 overlay）、不运行时重拷（bootstrap 是一次性）、不做全套 settings 编辑器、不自动同步本地。

---

## 1. 背景：为什么现在是"共享本地"，为什么要反转

### 1.1 现状链路（已核实）

```
naozhi spawn claude:
  BuildArgs (protocol_claude.go:89) → "--setting-sources", "user"
     └─ claude 自己读 ~/.claude/settings.json（与命令行 claude 完全一致）
```

`direct-user-settings.md` PR1 的目标原话："naozhi 内的 cc 与命令行 cc 行为**完全一致**、单一配置源、零额外配置"，为此**删除**了历史的 `writeClaudeSettingsOverride`（逐字拷贝本地 settings→过滤→`--settings <override>` 加载）。

> 例外（保留不动）：`internal/sysession/runner.go:277` / `vision_runner.go:61` 的内部 Runner 仍用 `--setting-sources ""`（无 entry-auth，AutoTitler 可能触发本地 hook 死循环）。本 RFC 只改**面向用户会话**的 claude spawn，不碰 sysession Runner。

### 1.2 为什么反转（三个诉求）

1. **naozhi 要能与本地不同**：后台 / 批量 session 想用 `thinking: max`，但操作者本地交互 claude 想保持 `high`。共享一份做不到。
2. **隔离容错**：本地 settings 被误改（语法错、错误的 hook、错误的 model pin），naozhi 不该跟着崩——它是常驻服务，应有自己稳定、可测的配置面。
3. **thinking effort 的正确落点**：thinking 深度在 claude 上**本就是 settings.json 的功能开关，不是环境变量**（核实：shim allowlist CONTRACT `manager.go:1283` 明令禁止为让 settings 功能开关生效而加 env key）。因此 per-naozhi 覆盖它，本就该走 settings 层，而非 access-profile 的 env overlay。**这条把 v2 纠结的"thinking 进不进 env 白名单"问题彻底消解**。

### 1.3 反转要重新扛起的两个历史代价（正视）

`direct-user-settings` 删拷贝不是随意的。方案 C 要主动接受：

| 当年的问题 | 方案 C 的处置 |
|---|---|
| **hook 死循环**（naozhi→claude→hook 回调 naozhi） | bootstrap 时**剥掉 hook 段**，naozhi 这份是"干净执行配置"；面向用户会话已有 HTTP 入口鉴权作第二道防线 |
| **配置漂移**（本地加了新键，拷贝副本漏掉，naozhi≠本地） | **这是 C 的刻意语义，不是 bug**：naozhi 就是要独立。用 UI 横幅 + diff **显式暴露**差异，而非假装同步 |

关键区别于历史实现：当年 `writeClaudeSettingsOverride` **每次 spawn 都重拷 + 带 env 白名单过滤**，漂移是隐性的、且过滤逻辑要持续维护。方案 C 的拷贝是**一次性 bootstrap**（仅剥 hook，不做 env 白名单过滤——env 由 shim allowlist 那条独立通路管，与本文件无关），此后是一份 naozhi 拥有的静态文件，无重拷、无持续过滤负担。

---

## 2. 数据流

```
安装 / 首次启用：
  bootstrap: cp ~/.claude/settings.json → <dataDir>/naozhi-settings.json
             ├─ 剥掉 "hooks" 段
             └─ schema 校验；不合法则种子为最小默认（不 fatal）
  之后：naozhi-settings.json 由 naozhi 拥有，本地变更不再流入

每次 spawn 面向用户的 claude：
  BuildArgs: "--setting-sources", "" , "--settings", <naozhi-settings.json>
  thinking 旋钮（若设）→ 合并进 <naozhi-settings.json> 的对应键（见 §4）
     └─ claude 只读这一份，行为由 naozhi 完全掌控
```

- `--settings` flag 仍在 claude CLI 且仍在 naozhi 的 arg allowlist（`protocol_claude.go:229`，只是当前无人注入）——**无需改 CLI，只是重新启用**。
- 注入点：`SpawnOptions`（`wrapper.go:24`）加一个 `SettingsFile string` 字段，`ClaudeProtocol.BuildArgs` 据其在 `--setting-sources user`（现状）与 `--setting-sources "" --settings <file>`（本 RFC）之间二选一。空 = 现状行为，**向后兼容**。

---

## 3. 文件的所有权与编辑边界

| 内容 | 谁改 | 怎么改 |
|---|---|---|
| **thinking effort**（本轮唯一暴露旋钮） | 操作者 | dashboard 控件 → naozhi 合并进文件（§4） |
| **其余 settings**（mcp / 权限 / model pin / 其它 env…） | 高级操作者 | 直接编辑 `naozhi-settings.json` 文本，naozhi 只加载不代管 |
| **hooks 段** | —— | bootstrap 时剥除；不通过 UI 加回（防死循环）；高级用户若手动加，自负其责 |

**明确不做全套 settings 编辑器**：naozhi 只给"人话旋钮"（thinking），避免 UI 膨胀成 settings IDE。整份文件对高级用户透明可编辑。

---

## 4. thinking effort：naozhi 侧的表达

thinking 是本轮唯一暴露的旋钮，落在这份独立 settings 里，**不经 env、不经 access-profile overlay**。

- **档位（人话）**：`标准 / 深入 / 深度`（对应内部 `""` / `high` / `max`；`low` 暂不放，很少用）。
- **落地键**：写入 `naozhi-settings.json` 的对应 settings 键（claude settings 里控制 thinking 预算的字段——**具体键名与数值待一次实测校准**，作为 §9 决策点；naozhi 不硬编码 `MAX_THINKING_TOKENS` env，而是写 settings 文件里 claude 认的那个键）。
- **作用域**：全 naozhi 单份 → 一次设置，所有面向用户会话生效。这正是"naozhi=max，本地=high"诉求的语义单位（naozhi 整体 vs 本地整体）。

> per-session / per-profile 不同档是**后续增强**：那时才需要"基线文件 + per-spawn 覆盖片段"，本轮不做，避免过早背上多档存储 + per-profile UI 的复杂度。

---

## 5. UI 设计：显式告知"这和本地不一样"

用户对这份独立配置的心智必须是**"naozhi 有自己一套，我知道它和本地不同"**，绝不能误以为改本地会同步过来。

### 5.1 设置面板顶部常驻横幅

```
┌────────────────────────────────────────────────────────────┐
│ ⚠  naozhi 使用独立配置，与本地 ~/.claude/settings.json 不同步 │
│    [查看差异]     [用本地重新初始化…]                          │
└────────────────────────────────────────────────────────────┘
```

- 常驻（不可关闭），因为"独立"是需要用户持续知晓的事实，不是一次性提示。

### 5.2 thinking 旋钮

```
naozhi 思考深度   [ 深度 ▾ ]   ← 标准 / 深入 / 深度
                  当前本地为「深入」，naozhi 为「深度」   ← 差异时才显示这行小字
```

差异行只在两者不同时出现，让"naozhi 更深"这件事一眼可见——正是本 RFC 的核心卖点。

### 5.3 「查看差异」

- 弹出**只读** diff：`naozhi-settings.json` vs `~/.claude/settings.json`，高亮不同键。
- 只读——本面板不做双向编辑；引导用户理解差异，而非在此消除差异。

### 5.4 「用本地重新初始化」

- 重新跑一次 bootstrap（拷贝本地 → 剥 hook → 覆盖当前 naozhi 文件）。
- **二次确认**：明确文案"这将用本地配置覆盖 naozhi 当前的独立配置（含你设的 thinking 档位），不可撤销"。
- **不提供自动/持续同步**——那会诱导"naozhi 跟随本地"的错误心智，违背 C 的隔离前提。同步只有这一个显式、破坏性、需确认的动作。

### 5.5 反例（明确不做）

- ❌ 不做本地→naozhi 的自动同步 / 后台合并。
- ❌ 不在面板里做整份 settings 的字段编辑器。
- ❌ 不通过 UI 加 hook。
- ❌ 不静默 bootstrap 覆盖用户已改过的 naozhi 文件（bootstrap 只在文件不存在时自动跑；已存在则只能经 5.4 显式触发）。

---

## 6. 兼容 & 迁移

- **spawn 向后兼容**：`SpawnOptions.SettingsFile` 空 → 保持 `--setting-sources user`（现状，bit-identical）。仅当 naozhi 启用独立 settings 时才切到 `--settings` 路径。
- **首次启用**：bootstrap 自动生成 `naozhi-settings.json`。生成前的部署行为不变。
- **sysession Runner 不动**：仍 `--setting-sources ""`（§1.1 例外）。
- **可回退**：删除 `naozhi-settings.json` + 关闭开关 → 回到 `--setting-sources user` 共享本地的现状。整个特性 opt-in。
- **access-profile / env overlay 正交且不变**：auth 仍走 access-profile 的 env overlay（v1 机制）；本 RFC 只管 settings 文件这条独立通路。两者互不影响——一个管"连哪个账号"，一个管"claude 的行为配置"。

---

## 7. 安全

- **hook 死循环**：bootstrap 剥 hook 段是主防线；面向用户会话的 HTTP 入口鉴权（webhook 签名 + dashboard token）是既有第二防线。二者叠加，风险不高于现状。
- **文件权限**：`naozhi-settings.json` 0600（可能含 mcp token 等），原子写（复用 `osutil.WriteFileAtomic`），置于 dataDir 下与其它 naozhi 状态同级。
- **不引入新的 env 泄漏面**：本文件是 settings，不经 shim env allowlist；claude 自己读文件，Bash 工具读回的是 settings（本就是 claude 自己的配置），非额外 credential 暴露。
- **bootstrap 拷贝的 secret**：若本地 settings 的 env 段含明文 token，拷贝会带过来——bootstrap 应对 env 段做与既有 `filterClaudeEnv` 一致的处理，或在 diff/日志中不回显 env 值（待 §9 决策）。

---

## 8. 测试策略

1. **BuildArgs 分支**：`SettingsFile` 空 → `--setting-sources user`（现状）；非空 → `--setting-sources "" --settings <file>`。契约测试锁两条路径。
2. **bootstrap**：拷贝本地 → hook 段被剥除；本地不合法 → 种子为最小默认且不 fatal；文件已存在 → 不自动覆盖。
3. **thinking 合并**：设 `深度` → 文件对应键为预期值；改档位 → 幂等重写、不破坏文件其余键（yaml/json node 保序，参照 `write_access_profile.go` 的 node surgery 手法）。
4. **diff**：naozhi vs 本地键差异计算正确；env 值不回显。
5. **sysession Runner 不受影响**：仍 `--setting-sources ""`。
6. **回退**：删文件 + 关开关 → BuildArgs 回到 `user`。

---

## 9. 决策点（待 owner 确认）

1. **thinking settings 的确切键名 + 三档数值**：需一次实测校准 claude settings.json 里控制思考预算的字段（是 `env.MAX_THINKING_TOKENS`，还是顶层专用键？）。这决定 §4 落地键。
2. **bootstrap 对本地 env 段的处理**：整段拷贝（可能带明文 token）、按 `filterClaudeEnv` 过滤、还是整段丢弃（naozhi 的 auth 走 access-profile overlay，settings 文件里的 env 或许根本不需要）？倾向**丢弃 env 段**——auth 已由独立通路负责，settings 文件只留行为配置。
3. **文件路径**：`<dataDir>/naozhi-settings.json` 是否合适，是否要可配。
4. **启用方式**：默认开（所有部署自动 bootstrap 走独立 settings）还是 opt-in 开关（保守，默认仍共享本地，操作者显式启用）？倾向 **opt-in**，降低对现有部署的意外行为改变。

---

## 10. Rollout

- **PR-1**：`SpawnOptions.SettingsFile` + BuildArgs 双路径 + 开关（默认 off = 现状）。零行为变化。
- **PR-2**：bootstrap（拷贝 + 剥 hook + 校验）+ 文件加载。启用后走独立 settings。
- **PR-3**：thinking 旋钮（settings 键合并，幂等 node surgery）。
- **PR-4**：UI（横幅 + thinking 控件 + 只读 diff + 重新初始化确认）。

PR-1/2 立起"独立 settings"骨架，PR-3/4 交付 thinking + 可视化。可按需在 PR-2 后暂停（已获得隔离能力，thinking 稍后）。
