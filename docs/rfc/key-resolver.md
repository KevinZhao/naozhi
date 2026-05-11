# ARCH3 — SessionKeyResolver(收敛 planner/agent session key 派生)

> **状态**: Proposal v2
> **作者**: naozhi team
> **创建**: 2026-05-10
> **修订**: 2026-05-11(v2: 按 review findings 修 6 个 Blocker + 5 个 Strong-Suggest)
> **关联代码**:
> - `internal/session/key.go`(现有 SessionKey / ChatKey / 保留前缀常量 / 判定函数)
> - `internal/session/router.go:1769`(`AgentOpts` 定义,ExtraArgs 下游已有 make+copy 但 aliasing 防护仍在调用方)
> - `internal/project/manager.go`(`ProjectForChat` / `EffectivePlannerModel` / `EffectivePlannerPrompt`)
> - `internal/project/project.go:63`(`PlannerSessionKey` / `PlannerKeyFor`)
> - `internal/dispatch/dispatch.go:214-252`(**规范参考实现**,含 `[:len:len]` aliasing 防护)
> - `internal/dispatch/commands.go:151-196, 252-275`(`/stop` / `/urgent` / `/new` 三处副本,部分漏掉 model/prompt 合并)
> - `internal/server/project_api.go:88-108, 320-333`(planner 列表 + restart 两份手写副本)
> - `internal/server/dashboard.go:458-485`(`buildSessionOpts` 走 **key 反推** 的另一套副本,包含 ExtraArgs aliasing 防护)
> - `internal/upstream/connector.go:943-958`(reverse-RPC restart_planner 副本,**ExtraArgs 直接赋值没走 append,aliasing 风险**)

---

## 0. 修订历史

### v2(2026-05-11)

按 2026-05-10 对 v1 的深度 review 修订:

- **B1 修复**:v1 的 §4.6 / §4.7 迁移会让 `#6 planner-restart` 和 `#7 reverse-RPC restart_planner` 从**字面量空 opts**(`session.AgentOpts{Model, Workspace, Exempt}`)悄悄变成**从 `defaults["general"]` 起步**,线上首次遇到 P1 风险(planner argv 会新增 defaults["general"] 的 Model / ExtraArgs / Backend)。v2 引入 `ResolveForPlannerKey(name)` 独立签名,**不读 defaults**,明确与 #1/#5 正向路径区分;同时 §3.1 的 ResolveForKey 在 planner 分支也走 ResolveForPlannerKey,保持单一真源。
- **B2 修复**:v1 §3.1 只给 `ResolveForChat` 伪代码,`ResolveForKey` / planner-missing / scratch+cron key 等 fallback 行为未定义。v2 §3.1a 新增 ResolveForKey 的完整 4 分支伪代码(planner+存在 / planner+缺失 / IM 4 段 / 非 4 段 fallback),并引入 `(AgentOpts, bool)` 返回值让 caller 能区分"正常默认"vs"project 查不到"(采纳 S2)。
- **B3 修复**:v1 §6.1 "aliasing 断言用 `reflect.DeepEqual(snapshot, defaults)`" 抓不到 write-past-len——`append(s, x)` 在 `cap>len` 时写入 backing array 的 len 位但 header 不变,DeepEqual 只比 header 内可见范围。v2 §6.1 改成 **canary write-past-len** 手法,复用 `planner_args_isolation_test.go:82` 的 `shared[:cap(shared)]` peek 模式。
- **B4 修复**:v1 §2.1 表格第 7 行写 "连 aliasing 防护没 → aliasing 风险"定性错;#6/#7 实际是 `[]string{...}` 字面量新 slice,无 aliasing。v2 正文和表格对齐为"不继承 defaults(与 #1/#5 不对齐) / 若被改成 `append` 会破"。
- **B5 修复**:v1 §3.1 bullet 3 只说"字符串复刻",未说明是否复用 `ProjectKeyPrefix` 常量,也没提 `isPlannerKey` 需要同样复刻。v2 §3.5 给出 session 包内 `plannerKeyFor` / `isPlannerKey` 完整实现 + 和 project 包 `PlannerKeyFor` / `IsPlannerKey` 的一致性测试隔离策略(双端各自硬编码字面量断言,不成 import cycle)。
- **B6 修复**:§3.1 non-general 分支显式 `base.Exempt = false`(defense-in-depth)+ §3.3 表格列改 "base 值"保持准确。
- **S1 采纳**:§4.5 补 ResolveForKey 对 scratch / cron / 非 4 段 key 的显式 fallback 文档。
- **S3 采纳**:§7 Phase 3 明确 `/urgent` 新增回归测试,锁住 planner prompt 继承行为。
- **S4 采纳**:§6.4 新增 "historical keys" 兼容测试。
- **S5 采纳**:§3.4 测试 `NewDataSource(nil) → untyped nil interface` 防 typed-nil 陷阱。
- **S6 采纳**:§6.1 测试矩阵加 `Backend` 字段断言。
- **NITs**:冒号全半角统一、§10 "TODO-changelog.md" 改 "docs/TODO.md"、§9 strings.HasPrefix 描述改为 exemptKeyPrefixes 迭代。

v1 原文的 §1/§2/§3/§4/§5/§6/§7/§8/§9/§10 对应章节已就地修订;历史版本 git log 可查。

---

## 1. 目标 & 非目标

### 1.1 目标(GA 验收)

1. **收敛派生逻辑**:planner/agent session key + AgentOpts(workspace / model / extraArgs / exempt)派生只保留一个权威入口,其余调用点全部变成一行函数调用
2. **把 aliasing 防护从调用方责任升级为接口内部不变量**:`ExtraArgs` 切片的三参数 `[:len:len]` 防护走入接口,任一调用方无法再漏写
3. **保留现有语义**:
   - project-bound chat + `agentID == "general"` → 走 planner key(`project:{name}:planner`)+ planner model/prompt 覆盖
   - project-bound chat + 非 general agent → 普通 `{platform}:{chatType}:{chatID}:{agentID}` key,**仅复用 workspace**,不继承 planner model/prompt
   - 未绑定 project → 普通 key,router defaults
4. **不改 wire format**:session key 字符串格式 / AgentOpts 字段 / ProjectConfig schema 全不动,纯重构

### 1.2 非目标

1. 不动 session key 字符串格式(`project:{name}:planner` / `{platform}:{chatType}:{id}:{agentID}` 保持原样)
2. 不引入 `SessionKey` 强类型 / newtype — 那是独立话题,本 RFC 仍收 `string`
3. 不改 `project.Manager` 对外 API — `ProjectForChat` / `EffectivePlannerModel` / `EffectivePlannerPrompt` / `PlannerSessionKey` 全部保留,Resolver 是**封装层**,不是替代品
4. 不改 `session.AgentOpts` 字段顺序或语义
5. 不迁移 scratch / cron 的 key 派生(它们已有各自的专用路径,语义不同)

---

## 2. 背景

### 2.1 现状盘点:七处独立副本

`ProjectForChat` + `AgentOpts` 合并这套逻辑在以下 7 个调用点独立出现,每处都手工拼装 key、判 agent、覆盖 workspace / model / prompt,写法略有差异:

| # | 文件:行 | 路径 | 完整度 | 备注 |
|---|---|---|---|---|
| 1 | `internal/dispatch/dispatch.go:214-252` | IM 主消息路径 | **完整**(含 `[:len:len]` 防护) | **规范参考实现** |
| 2 | `internal/dispatch/commands.go:151-158` | `/stop` 解析 key | 仅 key,无 opts | 只需要 key,不构造 opts |
| 3 | `internal/dispatch/commands.go:180-196` | `/urgent` 紧急消息 | **漏 model / prompt** | 只设 `Exempt + Workspace`,未覆盖 planner 配置 |
| 4 | `internal/dispatch/commands.go:252-275` | `/new` 重置 planner/agent | 仅 key + Reset | 只需要 key |
| 5 | `internal/server/dashboard.go:458-485` | `buildSessionOpts`(**从 key 反推 project**) | **完整** + aliasing 防护 | 唯一走 `project.IsPlannerKey` + `SplitN(key, ":", 3)` 反推 project 的副本,且有 `[:len:len]` |
| 6 | `internal/server/project_api.go:326-333` | `POST /api/projects/planner/restart` | **不继承 defaults(与 #1/#5 不对齐)** | `opts := session.AgentOpts{Model, Workspace, Exempt}` 字面量新 slice,当前无 aliasing,但一旦被改成 `append(opts.ExtraArgs, ...)` 或 defaults 继承就会破 |
| 7 | `internal/upstream/connector.go:951-958` | reverse-RPC `restart_planner` | **不继承 defaults(与 #1/#5 不对齐)** | 同 #6,`opts.ExtraArgs = []string{...}` 字面量直赋值;本 RFC 迁移需保持"不继承 defaults"语义,见 §3.1a `ResolveForPlannerKey` |

此外 `internal/server/project_api.go:88-108`(`GET /api/projects` 填充 `planner_model`)是 **opts 构造的子集**——只取 model 字段做响应,不走 router,不计入 7 处。

### 2.2 R37-CONCUR1 aliasing bug 作为真实动机

#1 `dispatch.go:242` 的 `opts.ExtraArgs = append(opts.ExtraArgs[:len(opts.ExtraArgs):len(opts.ExtraArgs)], ...)` 三参数切片形态是 **R37-CONCUR1** 修复留下的。根因:

- `opts := d.agents[agentID]` 是 struct 值拷贝,但 `ExtraArgs []string` 字段是**引用类型**(底层数组共享)
- 连续两条 IM 消息走进来,两个 goroutine 的 `opts` 虽然是独立 struct,底层 array 仍是 `d.agents[agentID].ExtraArgs` 的同一块内存
- 普通 `append(opts.ExtraArgs, "--append-system-prompt", prompt)` 当 `cap > len` 时会**就地写入**,直接污染 `d.agents` 里的原始切片,进而影响后续其他 goroutine 读到的 agent 配置
- 当前下游 `router.go` 的 `spawnSession` 确实又做了 `make+copy`,是**双层防护**;但"下游兜底"不稳定——任何一次重构把下游 copy 去掉,上游就会立刻炸

**调用点 #3 / #6 / #7 的非一致性**:
- #3(`/urgent`)根本没拼 `--append-system-prompt`,属于**功能漏实现**(bug that hasn't fired yet);Resolver 迁移会顺带补齐(见 §7 Phase 3 回归测试)
- #6 / #7 是 `opts.ExtraArgs = []string{...}` 字面量赋值,当前语义**正确且意图明确**:它们**不继承 `agents["general"]`** 的 Model / ExtraArgs / Backend。原因是 planner restart 的 spec 是 "从 project 配置 + planner 默认直接起 session",不走普通 chat 的 agent defaults。这是本 RFC 的**关键语义边界**:

  - #1(dispatch 主路径)+ #5(dashboard resume) = **"chat/agent 视角"**,从 `defaults[agentID]` 起步,project 绑定时覆盖 workspace/model/prompt。
  - #6(HTTP restart)+ #7(reverse-RPC restart)= **"planner 视角"**,从**空 opts**起步,只用 project 配置。
  - 这两条路径的 opts 起点**不同**是正确设计,不能一刀切合并。

- 因此本 RFC 的 Resolver 暴露**两个** planner-相关方法:`ResolveForChat(agentID="general")` 做 chat 视角(从 defaults 起步),`ResolveForPlannerKey(name)` 做 planner 视角(从空 opts 起步)。迁移 #6/#7 走 `ResolveForPlannerKey`,**禁止**走 `ResolveForKey`(后者对 planner key 内部也 delegate 到 `ResolveForPlannerKey`,对外只保证"给同一个 key 返回同样 opts")。

把 `ExtraArgs` 合并从"调用方责任"升级为"接口内部不变量":
- aliasing 防护(`[:len:len]`)唯一出现在 `ResolveForChat` 里(planner 视角不涉及 defaults 切片共享)
- #3 漏 `--append-system-prompt` 的 bug 在 Resolver 里不复现
- #6/#7 的"空 opts 起步"作为 `ResolveForPlannerKey` 的契约入文档,未来任何重构想改成"继承 defaults"必须改接口而不是偷偷改调用方

### 2.3 反向依赖的陷阱

`session` 包是底层,`project` 包引用 `session`(它用 `session.Router.GetOrCreate`)。如果 Resolver 放在 `session/routing.go` 里且**直接 import project**,会引入反向依赖,`session` 再也不能被 `project` 上游使用。必须用接口 + 回调让 session 保持下层。

### 2.4 现有封装尝试:`buildSessionOpts`

`server/dashboard.go:458` 的 `buildSessionOpts` 已经是一次**局部封装尝试**,但:
- 它只服务 dashboard 的 resume 路径(已有 key、反推 project)
- 签名 `(key string, agents map, projectMgr *project.Manager) AgentOpts` 和 dispatch 的 `(platform, chatType, chatID, agentID)` 不对等,形状不同所以无法复用
- 这恰好说明"派生"有两种起点(chat 信息正向、key 反向),Resolver 需要**两个入口**

---

## 3. 设计

### 3.1 接口与类型

在 **`internal/session/routing.go`**(新文件)定义:

```go
package session

// PlannerDataSource abstracts the project-layer data the key resolver needs.
// Concrete implementation lives in the project package; session never imports
// project directly. All methods return fully snapshot'd values so callers can
// treat them as pure reads (no hidden mutex interactions).
type PlannerDataSource interface {
    // ProjectBinding returns the project metadata for the given IM chat, or
    // zero-value ProjectBinding (Bound == false) if the chat is not bound.
    ProjectBinding(platform, chatType, chatID string) ProjectBinding

    // ProjectByName returns the project metadata for the given planner key's
    // embedded name. Used by the key-reverse resolution path.
    ProjectByName(name string) (ProjectBinding, bool)
}

// ProjectBinding is the minimal projection session needs — name, absolute
// workspace path, effective planner model, effective planner prompt. Populated
// by the project package via EffectivePlannerModel / EffectivePlannerPrompt so
// the resolver does NOT re-implement those precedence rules.
type ProjectBinding struct {
    Bound         bool
    Name          string
    WorkspaceDir  string
    PlannerModel  string // "" = inherit router / AgentDefaults
    PlannerPrompt string // "" = no --append-system-prompt
}

// KeyResolver derives a (session key, AgentOpts) pair for a given dispatch
// context. It encodes the project-binding precedence (general → planner,
// non-general → workspace-only) and the ExtraArgs aliasing-safe merge as
// internal invariants.
//
// The zero value is not usable; construct via NewKeyResolver.
type KeyResolver struct {
    defaults map[string]AgentOpts // agentID -> base opts (router defaults)
    data     PlannerDataSource    // may be nil → project feature disabled
}

func NewKeyResolver(defaults map[string]AgentOpts, data PlannerDataSource) *KeyResolver

// ResolveForChat is the "chat-view" path: from IM chat coordinates +
// agentID, return routed key and merged opts. Base = defaults[agentID];
// if chat is project-bound AND agentID == "general" overlay planner
// (workspace/model/prompt/Exempt=true); bound + non-general overlays only
// workspace. Replaces #1 (dispatch main) + #3 (/urgent).
func (r *KeyResolver) ResolveForChat(platform, chatType, chatID, agentID string) (key string, opts AgentOpts)

// ResolveForPlannerKey is the "planner-view" path: given a project name,
// return the planner key and opts WITHOUT reading defaults[agentID]. Used
// by administrative planner-restart flows (#6 HTTP + #7 reverse-RPC).
// Returns ok=false when the project cannot be found (caller decides 404
// vs 500; NOT a fallback to chat-view path).
func (r *KeyResolver) ResolveForPlannerKey(projectName string) (key string, opts AgentOpts, ok bool)

// ResolveForKey is the "key-resume" path: given an existing key (from
// sessions.json resume, dashboard WS subscribe), re-derive AgentOpts
// used for re-spawning. Internally delegates to ResolveForPlannerKey for
// "project:{name}:planner" keys; otherwise reads defaults[agentID] by
// parsing the 4-segment IM key. Replaces #5 (buildSessionOpts).
// Returns ok=false for scratch/cron keys or malformed keys — callers
// must route those to their dedicated resolution paths first.
func (r *KeyResolver) ResolveForKey(key string) (opts AgentOpts, ok bool)

// KeyForChat is a lightweight variant for callers that only need the
// key (e.g. /stop and /new). Does not compute opts. Kept separate so
// repeat key-only calls (/new {agent1} /new {agent2} ...) don't pay the
// opts merge cost.
func (r *KeyResolver) KeyForChat(platform, chatType, chatID, agentID string) string
```

**关键设计点**:

1. **依赖方向** — `session/routing.go` 定义 `PlannerDataSource` 接口,`project` 包里写一个 `managerAdapter` 满足它。`session` 不 import `project`,`project` 照旧 import `session`(单向)。

2. **`AgentOpts` 合并唯一实现**(伪代码,省 lock):

   ```go
   func (r *KeyResolver) ResolveForChat(platform, chatType, chatID, agentID string) (string, AgentOpts) {
       base := r.defaults[agentID] // 零值安全:map 查不到返回零值 AgentOpts
       if r.data == nil {
           return SessionKey(platform, chatType, chatID, agentID), base
       }
       b := r.data.ProjectBinding(platform, chatType, chatID)
       if !b.Bound {
           return SessionKey(platform, chatType, chatID, agentID), base
       }
       if agentID != "general" {
           // 非 general agent:仅复用 workspace,不继承 planner 配置
           base.Workspace = b.WorkspaceDir
           base.Exempt = false // defense-in-depth;见 B6
           return SessionKey(platform, chatType, chatID, agentID), base
       }
       // general agent + bound project ⇒ planner(chat-view 视角)
       base.Exempt = true
       base.Workspace = b.WorkspaceDir
       if b.PlannerModel != "" {
           base.Model = b.PlannerModel
       }
       if b.PlannerPrompt != "" {
           // 三参数切片:强制 fresh backing array,防止污染 r.defaults[agentID].ExtraArgs
           base.ExtraArgs = append(
               base.ExtraArgs[:len(base.ExtraArgs):len(base.ExtraArgs)],
               "--append-system-prompt", b.PlannerPrompt,
           )
       }
       return plannerKeyFor(b.Name), base
   }

   // ResolveForPlannerKey — planner-view 视角,不读 defaults。
   func (r *KeyResolver) ResolveForPlannerKey(name string) (string, AgentOpts, bool) {
       if r.data == nil {
           return "", AgentOpts{}, false
       }
       b, ok := r.data.ProjectByName(name)
       if !ok {
           return "", AgentOpts{}, false
       }
       opts := AgentOpts{
           Exempt:    true,
           Workspace: b.WorkspaceDir,
           Model:     b.PlannerModel,
       }
       if b.PlannerPrompt != "" {
           // 新 slice,无 aliasing 风险;不需要 [:len:len](defaults 未读)。
           opts.ExtraArgs = []string{"--append-system-prompt", b.PlannerPrompt}
       }
       return plannerKeyFor(b.Name), opts, true
   }

   // ResolveForKey — 4 分支分派。
   func (r *KeyResolver) ResolveForKey(key string) (AgentOpts, bool) {
       // 分支 (a): planner key → delegate 到 planner-view
       if isPlannerKey(key) {
           name := strings.TrimSuffix(strings.TrimPrefix(key, ProjectKeyPrefix), ":planner")
           _, opts, ok := r.ResolveForPlannerKey(name)
           return opts, ok
       }
       // 分支 (b): scratch / cron / 其他保留命名空间 → caller 应走专用路径
       if IsReservedNamespace(key) {
           return AgentOpts{}, false
       }
       // 分支 (c): IM 4 段 key → 从 defaults[agentID] 起步,不查 project
       // (resume 路径的 workspace 由 sessions.json 恢复,不需要这里覆盖)
       parts := strings.SplitN(key, ":", 4)
       if len(parts) != 4 {
           return AgentOpts{}, false // 分支 (d) 畸形
       }
       base := r.defaults[parts[3]]
       return base, true
   }
   ```

   `[:len:len]` 唯一出现在 `routing.go`,其他所有调用方都走 `resolver.ResolveForChat` / `ResolveForKey`,**不可能再漏写**。

3. **`plannerKeyFor` / `isPlannerKey` 的 session 包内复刻**:详见 §3.5。简述:session 包无法 import project(反向依赖陷阱),所以在 session 包内新增两个 **unexported** helper,复用 `ProjectKeyPrefix` 常量,和 project 包的 `PlannerKeyFor` / `IsPlannerKey` 保持字面量同步;一致性通过"双端各硬编码字面量断言"打平,不成 import cycle。

### 3.2 两种实现方案的对比

#### 方案 A:接口由调用方(dispatch/server)自行定义,session 只做纯工具

- dispatch 和 server 各自定义一个小接口,session 只导出 `SessionKey` / `PlannerKeyFor` / `MergeAgentOpts` 等工具函数
- 调用方自己编排"查 project → 拼 opts"的流程

**why-not**:7 处调用点里有 5 处要完整流程(ResolveForChat / ResolveForKey 语义),"只给工具"等于把合并逻辑还给调用方,那 aliasing 防护还是要在调用方写,**这个 RFC 的第二目标(把 aliasing 升级为接口内部不变量)达不到**。

#### 方案 B(推荐):session 同时定义接口 + 实现,project 注入回调

- `session/routing.go` 定义 `PlannerDataSource` 接口 + `KeyResolver` 实现
- `project` 包里写 `ManagerDataSource{m *Manager}` 方法集满足接口
- `main.go` 构造:`resolver := session.NewKeyResolver(agentDefaults, project.NewDataSource(projectMgr))`,然后注入 dispatch / server / upstream

**why-选它**:
- 实现只写一次,在底层
- 调用方拿到 `*KeyResolver` 直接调,签名统一
- `session` 不依赖 `project`,`project` 依照原来方向引用 `session`,依赖图无环
- 测试友好:`PlannerDataSource` 用 mock 结构体一把实现,不用 spawn 真 Manager

### 3.3 AgentOpts 合并的完整代码

(详见 3.1 伪代码)要点:

| 分支(ResolveForChat) | Key | Workspace | Model | Prompt 追加 | Exempt |
|---|---|---|---|---|---|
| 未绑定 project | `{plat}:{ct}:{id}:{agent}` | base | base | base | 保持 base |
| 绑定 + 非 general | `{plat}:{ct}:{id}:{agent}` | **proj.Path** | base | base | **显式 false**(§3.1 伪代码) |
| 绑定 + general | `project:{name}:planner` | **proj.Path** | planner > base | **`[:len:len]` 追加** | **显式 true** |

ResolveForPlannerKey 分支(planner-view,**不读 defaults**):

| 条件 | Key | Workspace | Model | Prompt 追加 | Exempt | ok |
|---|---|---|---|---|---|---|
| project 存在 | `project:{name}:planner` | **proj.Path** | **planner only** | **新 slice `[]string{...}`** | **true** | true |
| project 缺失 | `""` | `""` | `""` | `nil` | false | **false**(caller 判 404) |

ResolveForKey 分支(4 种):

| Key 形状 | 动作 | ok |
|---|---|---|
| `project:{name}:planner` | delegate 到 ResolveForPlannerKey | true / false(看 project 是否存在) |
| `cron:*` / `scratch:*` 等保留 | 返回零 opts | **false** |
| `{plat}:{ct}:{id}:{agent}` 4 段 | `defaults[agentID]` 原样返回(**不查 project**,不覆盖 workspace) | true |
| 非 4 段、非保留 | 返回零 opts | **false**(畸形) |

### 3.4 `PlannerDataSource` 的 project 实现

`internal/project/datasource.go`(新文件):

```go
package project

import "github.com/naozhi/naozhi/internal/session"

type dataSource struct{ m *Manager }

func NewDataSource(m *Manager) session.PlannerDataSource {
    if m == nil {
        return nil
    }
    return &dataSource{m: m}
}

func (d *dataSource) ProjectBinding(platform, chatType, chatID string) session.ProjectBinding {
    p := d.m.ProjectForChat(platform, chatType, chatID)
    if p == nil {
        return session.ProjectBinding{}
    }
    return session.ProjectBinding{
        Bound:         true,
        Name:          p.Name,
        WorkspaceDir:  p.Path,
        PlannerModel:  d.m.EffectivePlannerModel(p),
        PlannerPrompt: d.m.EffectivePlannerPrompt(p),
    }
}

func (d *dataSource) ProjectByName(name string) (session.ProjectBinding, bool) {
    p := d.m.Get(name)
    if p == nil {
        return session.ProjectBinding{}, false
    }
    return session.ProjectBinding{
        Bound:         true,
        Name:          p.Name,
        WorkspaceDir:  p.Path,
        PlannerModel:  d.m.EffectivePlannerModel(p),
        PlannerPrompt: d.m.EffectivePlannerPrompt(p),
    }, true
}
```

- `dataSource` 是一个 13 行的 adapter,纯转发,不含业务逻辑
- 所有"Effective" 精度规则继续留在 `Manager` 内(project-override > global-default > "") — Resolver 不重新实现
- `NewDataSource(nil) → nil` 让 `NewKeyResolver(defaults, nil)` 天然处理 "projects.root 未配置" 的场景,Resolver 内 `data == nil` 短路

注意 `NewDataSource(nil)` 必须**返回 untyped nil interface**(写法就是 `return nil`),而不是 `return &dataSource{m: nil}` (会产生 typed-nil,`data != nil` 判 true 但调方法 panic)。加专门的 `TestNewDataSource_NilManagerReturnsNilInterface` 断言 `NewDataSource(nil) == nil`,防御未来重构。

### 3.5 session 包内的 plannerKeyFor / isPlannerKey

session 包不 import project,但需要反推 planner key。因此在 `internal/session/key.go` 新增 2 个 **unexported** helper:

```go
// internal/session/key.go 追加

// plannerKeyFor is the session-package local replica of
// internal/project.PlannerKeyFor. Kept unexported so external callers
// continue to use the project package as the authoritative API.
func plannerKeyFor(name string) string {
    return ProjectKeyPrefix + name + ":planner"
}

// isPlannerKey is the session-package local replica of
// internal/project.IsPlannerKey. Replicated here so KeyResolver.ResolveForKey
// can dispatch without importing project. Both implementations lock to the
// same string literal via independent hardcoded test assertions — see
// session/routing_planner_format_test.go and project/project_test.go
// TestPlannerKeyFor.
func isPlannerKey(key string) bool {
    const suffix = ":planner"
    if !strings.HasPrefix(key, ProjectKeyPrefix) {
        return false
    }
    if !strings.HasSuffix(key, suffix) {
        return false
    }
    // 至少 "project:X:planner" 长度,且去前后缀后 name 非空
    mid := key[len(ProjectKeyPrefix) : len(key)-len(suffix)]
    return len(mid) > 0
}
```

**一致性测试**(独立断言,避免 import cycle):

- `internal/session/routing_planner_format_test.go`:`assert plannerKeyFor("foo") == "project:foo:planner"` + `assert isPlannerKey("project:foo:planner")` 硬编码字面量
- `internal/project/project_test.go:60-67` 已有的 `TestPlannerKeyFor`:硬编码同样字面量
- 两端各断自己,同向字面量锁死同一个格式。**不需要** `assert session.plannerKeyFor(x) == project.PlannerKeyFor(x)` 这种跨包断言(会引入 cycle 或 test-only 反向依赖)。

未来 key 格式升级(例如加 `v2:` 前缀)时,两处硬编码断言都会 fail,明确提示要同步修改。

---

## 4. 调用点迁移清单

逐个列出 7 处调用点的 before/after 差异(抽象层面)。

### 4.1 调用点 #1: `dispatch/dispatch.go:214-252`(IM 主路径)

**Before**:40 行手工拼装,双层条件嵌套,含 `[:len:len]` 长注释。

**After**:
```go
key, opts := d.resolver.ResolveForChat(msg.Platform, msg.ChatType, msg.ChatID, agentID)
```
一行。那段 R37-CONCUR1 注释搬到 `routing.go` 里 `ResolveForChat` 的实现处(把语义保留在唯一权威实现旁)。

### 4.2 调用点 #2: `dispatch/commands.go:151-158`(`/stop`)

**Before**:手工拼 `SessionKey(...)` + `proj := d.projectMgr.ProjectForChat(...)` 覆盖 key。

**After**:
```go
key := d.resolver.KeyForChat(msg.Platform, msg.ChatType, msg.ChatID, "general")
```
只要 key,不构造 opts。

### 4.3 调用点 #3: `dispatch/commands.go:180-196`(`/urgent`)

**Before**:7 行拼 key + opts,**漏 model / prompt 覆盖**(既有 bug)。

**After**:
```go
key, opts := d.resolver.ResolveForChat(msg.Platform, msg.ChatType, msg.ChatID, "general")
```
顺便修 planner prompt 缺失 bug — **这就是抽接口的真实收益之一**。

### 4.4 调用点 #4: `dispatch/commands.go:252-275`(`/new` reset 分支)

**Before**:`if proj := d.projectMgr.ProjectForChat(...); proj != nil { plannerKey := proj.PlannerSessionKey() }` 再走 agent reset 分支。

**After**:
```go
plannerKey := d.resolver.KeyForChat(msg.Platform, msg.ChatType, msg.ChatID, "general")
// plannerKey 已经是 project:{name}:planner 或 fallback general key
// 对 /new {agent},仍用 resolver.KeyForChat(...,agentID) 拿非 general key
```
注意:`/new` 的"绑定后 /new 重置 planner, /new {agent} 只重置 agent"分支逻辑保留在 commands.go 里——Resolver 不懂 `/new` 意图。

### 4.5 调用点 #5: `server/dashboard.go:458-485`(`buildSessionOpts`,key 反推)

**Before**:`SplitN(key, ":", 4)` → 判 agentID → `project.IsPlannerKey(key)` 判 planner → `SplitN(key, ":", 3)` 再拆一次拿 name → `projectMgr.Get(name)` → 合并 opts,共 27 行含 aliasing 防护。

**After**:
```go
opts, ok := resolver.ResolveForKey(key)
if !ok {
    // scratch/cron/malformed — 由 sessionOptsFor 的上游 fallback 处理
    opts = agents[agentID] // 保留原来的 fallback 路径
}
```
`ResolveForKey` 内部分派见 §3.1 伪代码。重要语义保留:**resume 路径不覆盖 workspace**(workspace 在 sessions.json 已存,dashboard 侧的 workspace 从别处来),和正向 `ResolveForChat`(新鲜 chat 信息覆盖 workspace)形成互补。

### 4.6 调用点 #6: `server/project_api.go:326-333`(planner restart)

**Before**(`project_api.go:326-333`):
```go
opts := session.AgentOpts{
    Model:     h.projectMgr.EffectivePlannerModel(p),
    Workspace: p.Path,
    Exempt:    true,
}
if prompt := h.projectMgr.EffectivePlannerPrompt(p); prompt != "" {
    opts.ExtraArgs = []string{"--append-system-prompt", prompt}
}
```
**注意**:这里 `opts` 是**字面量**起步,**不继承** `h.agents["general"]`。这是 planner restart 的既有 spec,**不能改**。

**After**:
```go
key, opts, ok := resolver.ResolveForPlannerKey(p.Name)
if !ok {
    http.Error(w, "project not found", http.StatusNotFound)
    return
}
// key == p.PlannerSessionKey()
```

**关键**:走 `ResolveForPlannerKey`(planner-view,不读 defaults),**不**走 `ResolveForKey`(对 planner key 会内部 delegate 到 ResolveForPlannerKey,行为等价,但调用方语义更清晰)。

**Migration note**:改完后必须做线下 smoke 对照 planner argv —— `ps -ef | grep -- '--model'` 对比迁移前后 **完全一致**,多出任何 flag 都是 defaults 继承漏了、RFC 的 `ResolveForPlannerKey` 实现有 bug。Phase 4 smoke 硬要求这一点。

### 4.7 调用点 #7: `upstream/connector.go:951-958`(reverse-RPC restart_planner)

**Before** (`connector.go:951-958`):同 #6 的字面量 opts 模式。

**After**:
```go
key, opts, ok := resolver.ResolveForPlannerKey(proj.Name)
if !ok {
    return nil, fmt.Errorf("project not found: %q", p.ProjectName)
}
// key == proj.PlannerSessionKey()
```
同 #6,走 ResolveForPlannerKey,**不读 defaults**,保持既有 planner restart spec。

### 4.8 调用点总结表

| # | Before 行数 | After 行数 | 减少 | 使用接口 | 修复 bug |
|---|---|---|---|---|---|
| 1 | ~38 | 1 | -37 | ResolveForChat | aliasing 防护内化 |
| 2 | ~6 | 1 | -5 | KeyForChat | — |
| 3 | ~16 | 1 | -15 | ResolveForChat | **补齐 /urgent 的 model/prompt**(走 S3 回归测试锁定) |
| 4 | ~5 | 1 | -4 | KeyForChat | — |
| 5 | ~27 | 3 | -24 | ResolveForKey + fallback | — |
| 6 | ~9 | 4 | -5 | ResolveForPlannerKey + 404 | 预防未来 aliasing |
| 7 | ~8 | 4 | -4 | ResolveForPlannerKey + error | 预防未来 aliasing |
| **合计** | **~109** | **15** | **-94** | — | 1 已知 + 2 潜在 |

注:#5/#6/#7 的 "After 行数" 比 v1 的 "1 行" 偏大,是因为 ok=false 分支需要显式的 fallback/404/error 处理。仍比 before 少 80%+。

---

## 5. 向后兼容 & 回滚

### 5.1 API 契约

- `project.Manager.ProjectForChat / EffectivePlannerModel / EffectivePlannerPrompt / PlannerSessionKey` 全部**保留不动**,以保证:
  - 外部集成测试 / 非重构路径继续可用
  - 万一 Resolver 有坑,调用方可以一行回退到旧写法
- `session.AgentOpts` 不加字段 / 不改语义
- session key 字符串格式不变 — 已有 `sessions.json` 数据不需要迁移

### 5.2 回滚路径

每个调用点的迁移都是**独立的、可回退的**。某一处 Resolver 化后线上出问题:
- 把该调用点 revert 回手工拼装(原代码已经 cover 该路径)
- Resolver 实现和其他调用点的迁移保持在 head
- 不需要 feature flag,不需要配置项

### 5.3 依赖注入的渐进式接入

Phase 1 仅新增 `routing.go` + `datasource.go` + 单元测试,**零调用方改动**。接口存在但无人消费。Phase 2-5 分别迁移一个调用方,每个 Phase 都是独立 PR、独立 build、独立 smoke。

---

## 6. 测试策略

### 6.1 表驱动覆盖矩阵

`internal/session/routing_test.go` 一张表覆盖 `ResolveForChat`:

| 维度 | 取值 |
|---|---|
| agentID | `"general"` / `"code-reviewer"` / `"researcher"` / 不在 defaults 里的 |
| project 绑定 | 未绑定 / 绑定 naozhi |
| planner model override | 空 / `"opus"` |
| planner prompt override | 空 / `"你是 naozhi 规划者"` |
| defaults[agentID].ExtraArgs | nil / 1 元素 / 2 元素且 cap > len(测 aliasing) |
| defaults[agentID].Backend | 空 / `"claude"` / `"kiro"` |

笛卡尔积约 **4 × 2 × 2 × 2 × 3 × 3 = 288** 组。每组断言:
- `key` 字面量
- `opts.Workspace` / `opts.Model` / `opts.Backend`(S6)
- `opts.Exempt`(非 general bound 应显式为 false,见 B6)
- `opts.ExtraArgs` 内容和**长度**
- **Aliasing canary**(B3 核心)——不能用 `reflect.DeepEqual(snapshot, defaults)`(只比 header 内可见范围)。正确姿势:

```go
// defaults 构造成 cap > len,留 "canary slot"
shared := make([]string, 2, 8)
shared[0], shared[1] = "--model", "opus"
defaults["general"] = AgentOpts{ExtraArgs: shared, ...}

_, opts := resolver.ResolveForChat(..., "general")  // 触发 append --append-system-prompt ...

// 关键断言:defaults 的 backing array 在 len 之后的 slot 必须没被 Resolver 污染
if peek := shared[:cap(shared)]; peek[2] != "" || peek[3] != "" {
    t.Fatalf("Resolver wrote past defaults len: peek=%v", peek)
}
```

参考 `internal/dispatch/planner_args_isolation_test.go:82` 的 `shared[:cap(shared)]` peek 模式(文件名里有 "TwoArgAppendDoesLeak" 作为 negative-control,展示了"不安全" append 的真实表现)。

### 6.2 race / 并发测试

```go
func TestResolver_AgentOptsNoAliasing_Concurrent(t *testing.T) {
    // defaults 构造成 cap > len 的 ExtraArgs
    // 10 个 goroutine 各跑 100 次 ResolveForChat 同 project-bound general agent
    // 预期:defaults 的 ExtraArgs 完整不变,10 个 goroutine 拿到的 opts.ExtraArgs 互不共享
}
```
`go test -race` 下必须零警告。

### 6.3 对 project.Manager 的集成

`internal/project/datasource_test.go` 一个薄测试:
- 用 `NewManager` + `Scan` 建一个真 manager
- `NewDataSource(m).ProjectBinding(...)` 回传的 `ProjectBinding` 等价于 `m.EffectivePlannerModel / Prompt / ProjectForChat` 三者合成
- 断言 `NewDataSource(nil) == nil`(防御性实现)

### 6.4 历史 key 兼容性(S4)

`internal/session/routing_historical_keys_test.go` 覆盖 `ResolveForKey` 对所有历史 key 形态不 panic:

- `testdata/historical_keys.txt` 内容(脱敏):
  - 正常 4 段:`feishu:direct:alice:general` / `feishu:group:xxx:code-reviewer` / `dashboard:session:local:general`
  - planner:`project:naozhi:planner` / `project:my-project:planner`
  - scratch:`scratch:abc123:general:general`
  - cron:`cron:daily-standup`
  - 畸形:`foo:bar` / `` / `a:b:c:d:e:f`
- 断言:
  - 正常 4 段 → `ok=true`,opts 非零(至少 agentID = parts[3])
  - planner + project 存在 → `ok=true`,opts.Exempt=true
  - planner + project 缺失 → `ok=false`
  - scratch / cron → `ok=false`
  - 畸形 → `ok=false`,**不 panic**

### 6.5 `/urgent` planner prompt 继承回归测试(S3)

Phase 3 落地 `/urgent` 迁移的**同一 commit** 必须包含 `internal/dispatch/commands_test.go:TestUrgent_AppendsPlannerPromptWhenProjectBound`:

- 构造 project-bound chat + EffectivePlannerPrompt="P"
- 触发 `/urgent` 消息
- 断言下游 spawnSession 收到的 opts.ExtraArgs 含 `["--append-system-prompt", "P"]`

**反向断言**:回滚 Resolver 迁移后该测试 fail(证明它锁住了修复,没有 false-positive)。

### 6.6 迁移后的冒烟

每迁移一个调用点,既有集成测试(dispatch_test / dashboard_test / connector_test)全绿。

**Phase 4 特有的 argv diff smoke**(B1 Migration note):planner restart 走本地 HTTP 前后对比 `ps -ef | grep claude | grep -- '--model'`,flag 集必须**字节级一致**。新增任何 flag 都是 Resolver 偷偷继承了 defaults,Resolver 实现有 bug。

---

## 7. 实施阶段

每阶段独立 commit、独立 `go build + go test -race + go vet`,独立部署 + smoke。

### Phase 1: 新增 Resolver 骨架(0.5 天)

**Done when**:
- `internal/session/routing.go` 含 `PlannerDataSource` / `ProjectBinding` / `KeyResolver` / `NewKeyResolver` / `ResolveForChat` / `ResolveForKey` / `KeyForChat`
- `internal/project/datasource.go` 含 adapter,15-20 行
- `internal/session/routing_test.go` 96 组表测试 + 并发 race 测试全绿
- 现有所有调用点**不改**,Resolver 在线但无人消费
- `go vet ./...` 零警告

### Phase 2: 迁移 dispatch.go 主路径(0.5 天)

**Done when**:
- 调用点 #1 迁移为单行 `resolver.ResolveForChat(...)`
- R37-CONCUR1 长注释搬到 `routing.go`
- `dispatch_test.go` 原有测试全绿
- 部署 + 飞书 IM 往返一条带图消息 + 一条 planner 消息 + 一条 agent 命令 smoke 通过

### Phase 3: 迁移 commands.go 三处(0.5 天)

**Done when**:
- 调用点 #2 / #3 / #4 全部迁移
- **#3 `/urgent` 补齐 planner model/prompt 覆盖——此阶段附带修复的 bug 要在 commit message 里写明**
- `/stop` / `/urgent` / `/new` 三个 IM 命令手动 smoke 全绿

### Phase 4: 迁移 server 侧(0.5 天)

**Done when**:
- 调用点 #5(`buildSessionOpts`)替换为 `resolver.ResolveForKey` + ok 分支 fallback
- 调用点 #6(planner restart handler)替换为 `resolver.ResolveForPlannerKey` + 404 分支
- dashboard resume + 点 restart 按钮 smoke 通过
- **argv diff smoke**(B1):迁移前后各跑一次 planner restart,`ps -ef | grep claude | awk '{print NF}'` 参数数一致,对比 `diff` 全 flag 列表字节级相同

### Phase 5: 迁移 upstream 侧(0.5 天)

**Done when**:
- 调用点 #7(reverse-RPC restart_planner)替换为 `resolver.ResolveForPlannerKey` + error 分支
- 多节点场景:primary 点远程 project restart 按钮,remote 端 `restart_planner` 接收后用 Resolver 拼 opts,planner 重启成功 smoke 通过
- **argv diff smoke**(同 Phase 4)

### 总计 **2.5 人天**

---

## 8. 风险

### 8.1 AgentOpts 值传递的额外分配

Resolver 返回 `AgentOpts`(值),而不是 `*AgentOpts`(指针)。每次调用都拷贝整个 struct(5 个字段,含 slice header)。

- 实测代价:`AgentOpts` 约 64 字节(`Model string` 16 + `ExtraArgs slice header` 24 + `Workspace string` 16 + `Backend string` 16 + `Exempt bool` 1 + padding)
- 分配次数:IM 消息每条 +1 次栈拷贝(不进堆,Go escape analysis 能优化掉);dashboard restart 同;**实际 heap alloc 不变**
- 风险:零。Go 对返回值 struct 的 copy 在绝大多数场景是零分配。

### 8.2 测试基线更新

现有测试里若有对 `d.agents[agentID].ExtraArgs` 的直接断言(非 aliasing 意义上的),在 Resolver 接入后路径变化可能需要轻量调整。预期影响面极小——大多测试用 mock agent 配置,不会断言底层切片身份。

### 8.3 迁移期间 dispatch 和 commands 逻辑暂时不一致

Phase 2 迁移了 dispatch.go 主路径但 commands.go 未动 → 同一个 `ProjectForChat` 判断 / opts 合并 两种写法并存 1 次迭代。**这是可接受的短暂不一致,因为**:
- 两种写法对同一输入必然产生同一输出(Resolver 实现是 dispatch 副本的精确提炼)
- 用单元测试 + 既有集成测试把关
- Phase 间隔按天计,窗口很短

如果 Phase 2-5 必须一次性合并,改成**一个大 commit**也可以,只是失去阶段性 smoke 的优势。推荐分阶段。

### 8.4 接口扩展压力

`PlannerDataSource` 只有两个方法,将来如果新增 "planner 启动前 hook"(例如 git pull 校验)之类需求,接口要扩。接口扩 = 实现扩 = 测试扩。**这是抽接口的既有代价,不特别严重**;一旦扩到 5+ 方法要考虑拆分,但那是未来问题。

### 8.5 PlannerKeyFor 字符串重复定义

session 包的 `plannerKeyFor("x")` 和 project 包的 `PlannerKeyFor("x")` 是两处字符串拼接,一旦 key 格式升级(如加入 `v2:` 前缀),两处都要改。缓解:加一条 `TestPlannerKeyFormat` 断言两者字节一致,分歧 = 测试红。

---

## 9. 非目标声明

以下**本 RFC 不做**,留给独立提案:

1. **不改 session key 字符串格式**。现有 `project:{name}:planner` / `{platform}:{chatType}:{id}:{agentID}` 保持不变。
2. **不引入 `SessionKey` 强类型**(newtype)。字符串继续走到底。强类型是独立话题,与 Resolver 正交。
3. **不动 `project.Manager` 公开 API**。`ProjectForChat` / `Effective*` / `PlannerSessionKey` 继续是权威方法,Resolver 是其薄封装。
4. **不合并 `scratch` / `cron` 的 key 派生**。两者都有独立语义(scratch 的 `BaseOpts` 继承、cron 的 `cron:{jobID}` 固定 shape),强行收敛反而引入不需要的耦合。
5. **不改 `session.AgentOpts` 结构**。字段顺序、字段名、字段语义全保留。
6. **不引入 feature flag**。每个调用点迁移可独立 revert,不需要运行期开关。
7. **不改 sessions.json schema**。重启恢复路径的 exempt 推导(`strings.HasPrefix(key, "project:")`)保留在 router.go,Resolver 只负责"新建 opts"不负责"从持久化恢复 opts"。

---

## 10. 参考

- `internal/dispatch/dispatch.go:214-252` 规范参考实现(chat-view)
- `internal/server/project_api.go:326-333` 规范参考实现(planner-view)
- `internal/dispatch/planner_args_isolation_test.go:82` aliasing canary 正确姿势
- R37-CONCUR1 修复记录(`docs/TODO.md` 里搜 R37)
- `docs/design/DESIGN.md` "Session key 命名空间"章节
- `docs/rfc/attachment-refcount.md` / `docs/rfc/event-log-persistence.md`(本 RFC 风格参考)
- `docs/rfc/consumer-interfaces.md` v2(并行 RFC:消费端小接口替换具体 Router 指针,正交推进)
