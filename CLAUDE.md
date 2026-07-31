# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
go build ./...                                       # check compilation
CGO_ENABLED=0 go build -o bin/naozhi ./cmd/naozhi/   # build binary
go vet ./...                                         # lint
go test ./...                                        # run all tests
go test ./internal/cli/...                           # run tests for one package
go test -run TestCandidatePaths ./internal/cli/...   # run a single test

bin/naozhi --config config.yaml                      # run locally
```

`config.yaml` is gitignored (environment-specific). Use `config.example.yaml`
as the template: `cp config.example.yaml config.yaml` then fill in real values.

Cross-compile for deployment target (ARM64 Linux):
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/naozhi ./cmd/naozhi/
```

Deploy: `cmd/naozhi/service.go::generateSystemdUnit` is authoritative for `sudo naozhi install`; `deploy/naozhi.service` is a manual-deploy reference kept in sync (regression gate: `TestGenerateSystemdUnit_MatchesDeployTemplate`).

## Architecture

Naozhi is an IM gateway that wraps AI CLI agents (Claude CLI, Kiro, or Codex) as long-lived subprocesses. Communication uses a pluggable Protocol interface: `ClaudeProtocol` (stream-json NDJSON over stdin/stdout), `ACPProtocol` (JSON-RPC 2.0 Agent Client Protocol), or `CodexProtocol` (codex app-server JSON-RPC 2.0 over stdio). The entire agent loop (tools, context, reasoning) is delegated to the CLI -- Naozhi is just the routing layer.

**Request flow**: IM platform -> message handler -> async goroutine -> session router -> CLI stdin -> read stdout until turn complete -> platform reply API.

**Key constraint**: Feishu requires 200 response within 3s. The webhook handler returns 200 immediately and processes asynchronously via `go handler(context.Background(), msg)`.

### Module Dependency

`internal/` 每个顶层包一行、按层分组。本清单与磁盘目录的同步由
`cmd/naozhi/claude_md_modules_test.go::TestClaudeMDModuleList_MatchesInternalPackages`
把关——新增/删除顶层包必须同步更新这里，否则 CI fail。

```
cmd/naozhi/main.go
  组合根
  -> wireup       组合根装配：backend 注册、config 校验、cron+sysession 调度器装配
  -> config       YAML 加载、${ENV_VAR} 展开、校验

  核心链路（IM 消息 → CLI 进程）
  -> cli          Protocol 接口（stream-json/ACP）+ spawn/manage CLI 进程 + watchdog；子包 clievent/backend
  -> session      Session router、并发控制、TTL、持久化恢复；子包 agentlink/api/runhistory
  -> dispatch     消息处理 + slash 命令 + per-session 队列
  -> platform     Platform 接口 + feishu/slack/discord/weixin 子包
  -> server       HTTP server、路由注册、WebSocket hub、REST API
  -> dashboard    dashboard handler 子包群（auth/cron/cronview/discovery/project/session/ext/*；httputil 叶子）
  -> cron         定时任务调度（robfig/cron）+ 运行历史
  -> sysession    内建后台 daemon 框架（system sessions）
  -> project      项目发现、chat 绑定、planner 路由
  -> projectapi   project 的零依赖契约类型

  多节点
  -> node         WebSocket 协议类型 + HTTP/reverse node 客户端
  -> upstream     Reverse-connect 客户端（NAT 穿透；拨号 primary naozhi）

  持久化 / 历史
  -> eventlog     Event log 子系统（persist/schema/api 子包；append-only + crash recovery）
  -> history      后端无关历史加载接口；claudejsonl/kirojsonl/codexjsonl/naozhilog/merged 子包
  -> attachment   附件持久化 + refcount tracker 子包
  -> discovery    扫描 Claude CLI 磁盘工件（外部进程发现 / takeover）

  辅助域
  -> agentcore    AgentCore 云沙箱 control-plane 客户端
  -> agentroute   "/command agentId" 解析的单一真相源
  -> assets       Dashboard "installed assets" 零依赖叶子
  -> ccassets     Claude Code 资产浏览 provider（dashboard 用）
  -> transcribe   语音转写（Amazon Transcribe Streaming）
  -> selfupdate   GitHub Releases 自升级 + 校验
  -> shim         零停机重启：outlive naozhi 的 sidecar 进程
  -> usermsg      用户消息分类
  -> gitinfo      读取目录的 git branch / worktree 状态
  -> i18n         Locale 解析与消息渲染
  -> metrics      进程级计数器（expvar）
  -> runtelemetry 跨子系统 run 生命周期事件类型
  -> naozhisettings  naozhi 托管的 Claude settings 文件
  -> uiprefs      Dashboard 展示偏好持久化
  -> registry     插件 / 扩展注册表的 canonical home
  -> envpolicy    共享 env 过滤原语
  -> datadir      数据根目录磁盘布局策略

  叶子工具
  -> osutil       Home/路径展开、进程 helpers、sd_notify、PID 复用防护
  -> textutil     字符串工具：截断、脱敏、UUID 派生（依赖 osutil.IsLogInjectionRune，非零依赖叶子）
  -> netutil      Client-IP 提取（trusted-proxy 处理）
  -> ratelimit    Per-key token-bucket 限流（login/WS/upload 用）
  -> limits       跨包大小/数量上限常量
  -> timeouts     超时 / deadline 常量的 canonical home
  -> sessionconst Session 调优常量
  -> sessionkey   Session key 前缀规范（router 命名空间）
  -> backendid    Backend-ID 长度/格式校验
  -> apierr       Claude API 错误检测与本地化
  -> ctxutil      context.Context helpers
  -> leakguard    "leaked tool" 检测的单一真相源
  -> leakcheck    测试用泄漏断言 helper
  -> testhelper   共享测试工具
```

### CLI Process Lifecycle

Each CLI process is long-lived (stdin/stdout stay open across turns). The Wrapper selects a Protocol via the `backend.Profile` registry (`internal/cli/backend/profile.go`, canonical list of registered backends) keyed by `cli.backend` config:
- `claude` (default): `ClaudeProtocol` -- stream-json, session ID from init event
- `kiro`: `ACPProtocol` -- JSON-RPC 2.0 Agent Client Protocol, session ID from `session/new` response
- `codex`: `CodexProtocol` -- codex app-server JSON-RPC 2.0 over stdio (see `docs/rfc/codex-backend.md`)

Multi-backend deployments use `cli.backends: [{id, path, model, args}, ...]` so dashboard / IM / cron / reverse-node can pick backend per session. See `docs/rfc/multi-backend.md` for the full design (backend.Profile registry, kirojsonl history source, ACP `session/cancel` notification, reverse-node capability routing). Implementation gotchas confirmed in `docs/rfc/multi-backend-validation.md`:
- ACP `session/cancel` is a **notification** (no id), not a request
- ACP RPC ID can be **string UUID** (kiro `permission_request`), not always int — use `json.RawMessage`
- ACP permission `optionId` is `allow_once / allow_always / reject_once` (underscore, not hyphen) — read from request `options[].optionId`, do not hardcode
- Kiro persists session state at `~/.kiro/sessions/cli/<sid>.{json,jsonl}` with stale-PID lock auto-recovery

Protocol.Init() runs after spawn but before readLoop, handling any handshake (no-op for Claude, initialize + session/new for ACP). Session ID is captured during Init or from the first Send.

Process states: `Spawning -> Ready <-> Running -> Dead`. Dead processes with a SessionID can be resumed via `--resume` (Claude) or `session/load` (ACP).

**Watchdog**: During Running state, two timers enforce limits:
- `no_output_timeout` (default 2min): Reset on any event; if triggered, kill process
- `total_timeout` (default 5min): Single shot; if triggered, kill process

### Protocol Interface

```go
type Protocol interface {
    Name() string
    Clone() Protocol
    BuildArgs(opts SpawnOptions) []string
    Init(rw *JSONRW, resumeID string, cwd string) (sessionID string, err error)
    WriteMessage(w io.Writer, text string, images []Attachment) error
    WriteUserMessageLocked(w io.Writer, uuid, text string, images []Attachment, priority string) error
    SupportsPriority() bool
    SupportsReplay() bool
    WriteInterrupt(w io.Writer, requestID string) error
    ReadEvent(line string) (events []Event, done bool, err error)
    HandleEvent(w io.Writer, ev Event) (handled bool)
}
```

> Notes:
> - The interface is a strict partition of two facets (compile-time pinned):
>   `ProtocolCore` (Name/Clone/BuildArgs/Init/WriteMessage/ReadEvent/HandleEvent
>   -- what every backend implements meaningfully) and
>   `ProtocolPassthroughExt` (WriteUserMessageLocked/WriteInterrupt/
>   SupportsPriority/SupportsReplay -- stream-json-only surface; ACP degrades).
> - `Init` takes the workspace `cwd` so ACP can pass it in `session/new`;
>   `ClaudeProtocol` ignores the argument because stream-json inherits the
>   shim's `os.Chdir`.
> - `WriteInterrupt` emits a mid-turn interrupt; ACP returns
>   `ErrInterruptUnsupported` so callers fall back to SIGINT.
> - `ReadEvent` returns a **slice** (one wire frame can surface multiple
>   semantic events, e.g. ACP turn-end = assistant text + result). The `done`
>   flag is advisory and discarded by all production callers -- turn-end is
>   detected from a `result`/turn-end Event, never from `done`.
> - Hot-path allocation is handled by the optional `eventReaderInto` facet
>   (`ReadEventInto(line, buf)`), surfaced via type assertion, not by changing
>   `ReadEvent` itself.
> - This signature is shared by DESIGN.md, this file, and
>   `internal/cli/protocol.go`; any change must update all three in lockstep.

### Platform Adapter Pattern

Platforms implement `Platform` interface and register their own webhook routes via `RegisterRoutes(mux, handler)`. The platform calls `handler(ctx, msg)` when a message arrives -- the server never parses platform-specific formats.

Platforms needing background goroutines implement `RunnablePlatform` with `Start()/Stop()`. Platforms that cannot send interim messages (e.g. WeChat iLink's single-use reply tokens) implement `SupportsInterimMessages() bool` returning false.

| Platform | Transport | Interface |
|----------|-----------|-----------|
| Feishu   | WebSocket (default) or HTTP webhook | `RunnablePlatform` |
| Slack    | Socket Mode (WebSocket) | `RunnablePlatform` |
| Discord  | WebSocket gateway | `RunnablePlatform` |
| WeChat   | iLink Bot HTTP long-poll | `RunnablePlatform` |

### Session Management & Agent Routing

Session key format: `{platform}:{chatType}:{chatID}:{agentId}` (e.g., `feishu:direct:alice:code-reviewer`).
Other key namespaces (canonical home: `internal/sessionkey`, wire-stable constants):
- `project:{name}:planner` -- project planner sessions (exempt from TTL and max_procs)
- `cron:<jobID>` / `sys:<daemonID>` / `scratch:<sessionID>` -- cron jobs, sysession daemons, scratch sessions
- `dashboard:pj:<workspace-hash>:<agent>` -- project-stable dashboard keys (chatType `pj`, deliberately not `project`, to stay unambiguous vs the planner namespace)

Each session is independent: owns one long-lived CLI process, maintains separate context and session ID, uses per-session model/args from agent config.

Command routing: `/review xxx` -> `code-reviewer` agent, `/research xxx` -> `researcher` agent, plain messages -> `general` agent (or planner if chat is project-bound). `/new` resets general; `/new review` resets code-reviewer. `/cd <path>` changes working directory for all sessions in a chat. `/project <name>` binds a chat to a project.

**Session guard**: Only one message is processed per session at a time (`sessionGuard` with `sync.Map`). Duplicate messages are rejected; busy sessions reply "please wait" with 3s rate-limiting.

### Multi-Node Architecture

Naozhi supports aggregating sessions from multiple machines into a single dashboard:

- **Primary node** (`nodes` config): Polls remote nodes via HTTP REST every 10s, caches results in `node.CacheManager` (`internal/node/cache.go`). Never blocks dashboard API on unreachable nodes.
- **Reverse-connect** (`upstream` config): Nodes behind NAT dial into the primary via WebSocket (`/ws-node`). The `internal/upstream` Connector handles reconnection with jittered exponential backoff (1s -> 30s, plus a circuit breaker). The `node.ReverseServer` validates tokens with constant-time comparison.
- **Protocol** (`node.ReverseMsg`, `internal/node/protocol.go`): JSON over WebSocket -- `register/registered`, `request/response` (fetch_sessions, fetch_projects, send), `subscribe/event` (real-time streaming), `ping/pong`.

### Project Management

When `projects.root` is configured, the `project.Manager` scans **every non-hidden subdirectory** (`os.ReadDir` + skip names starting with `.`). There is NO marker-file requirement — a bare directory is a valid project, pinned by `TestScan_PicksUpDirsWithoutCLAUDEMd`. Each project stores config in `.naozhi/project.yaml` (planner model, prompt, chat bindings); a project with no `.naozhi/project.yaml` simply uses defaults.

Chat binding (`/project <name>`) routes plain messages to a planner session (`project:{name}:planner`) with the project directory as workspace. Agent commands still create per-chat sessions but use the project path. Planner sessions are exempt from TTL eviction and max_procs capacity.

The project list is rescanned every 60s. Orphaned planner sessions for removed projects are cleaned up automatically.

### Dashboard & WebSocket

The dashboard is an embedded PWA served at `/dashboard`: `internal/server/static/` holds `dashboard.html` plus JS modules (`dashboard.js`, `agent_view.js`, `cron_view.js`, `asset_browser.js`, `files_view.js`, `nz_util.js`), `sw.js`, and `manifest.json`. Real-time updates use a WebSocket hub (`/ws`) with:

- **Client messages** (dispatch switch: `internal/server/wsclient.go`): `auth`, `subscribe` (with optional `after` timestamp), `unsubscribe`, `send`, `interrupt`, `ping`, `agent_subscribe`, `agent_unsubscribe`
- **Server messages**: `auth_ok`, `auth_fail`, `subscribed`, `unsubscribed`, `history`, `event`, `send_ack`, `pong`, `error`, plus agent/team streaming types -- see `internal/server/wshub_types.go`
- Remote node events are relayed transparently -- subscribe with `node` field to stream from a remote session.

REST API: ~80 method-prefixed routes registered in `internal/server/routes.go` (the authoritative list -- grep `HandleFunc` there rather than trusting any doc enumeration). Families: `/api/sessions/*` (list/send/events/runs/interrupt/resume/bind/label/upload/attachment/git...), `/api/cron/*` (CRUD + pause/resume/trigger/preview + runs history/replay), `/api/projects/*` (config/files/favorite/planner), `/api/discovered/*` (preview/takeover/close), `/api/scratch/*`, `/api/settings`, `/api/auth/*`, `/api/access-profiles`, `/api/cc/assets`, `/api/cli/backends`, `/api/system/*`, `/api/transcribe`, `/api/memory/{slug}`. WebSocket: `/ws` (dashboard), `/ws-node` (reverse-connect nodes). Health: `/health`, `/livez`, `/readyz`.

### Session Discovery & Takeover

The `discovery` package scans `~/.claude/projects/<slug>/<sessionId>.jsonl` (Claude CLI's on-disk artifacts, which naozhi reads but does NOT own) to find external (non-naozhi-managed) CLI processes. It cross-references PIDs, upgrades stale session IDs from JSONL mtimes, and extracts summaries from each project dir's `sessions-index.json`. The dashboard can "takeover" a discovered process: kill the original PID (verified via `/proc/PID/stat` start time to prevent PID reuse attacks), then `--resume` under naozhi management.

### Session Persistence

Sessions are persisted to `~/.naozhi/sessions.json` at shutdown:
- Each entry stores `key`, `session_id`, `workspace`, `total_cost`
- A sibling `sessions.meta.json` sidecar records `{version, written_at, generator}`; the main file stays as a plain JSON array so older naozhi builds read it unchanged. `loadStore` treats a missing sidecar as legacy v1 and only `slog.Warn`s if the sidecar reports a version higher than the one this build understands
- On restart, dead sessions are loaded and history is async-loaded (naozhi event log first, Claude JSONL fallback)
- Next message to a dead session resumes via `--resume`
- Captures session_id under sendMu to avoid Send() data races

### Event Log Persistence (`docs/rfc/event-log-persistence.md`)

A second persistence tier lives at `~/.naozhi/events/<keyhash>.log` / `.idx`. Unlike `sessions.json` which only records session metadata, the event log captures every `cli.EventEntry` — including fields that Claude's own JSONL cannot recover such as `Images` (thumbnail data URIs), `ImagePaths` (workspace-relative attachment paths), `AskQuestion` card payloads, and agent-team linkage IDs.

Layout:

```
~/.naozhi/
  sessions.json                    # session catalog
  events/
    <keyhash>.log                  # append-only length-prefixed records
    <keyhash>.idx                  # sparse seq → byteOffset index
```

Key invariants:
- `<keyhash>` is `sha256(session_key)[:16]` — file names never leak the raw session key; the in-file header records the plaintext key so operators can `less`/`jq` to audit
- Write order is strict: `log.Write → log.Sync → idx.Write → idx.Sync`. A crash between any two steps is recovered by `Recover()` on next startup by truncating the log to the idx-backed safe edge
- `cli.EventLog.SetPersistSink` MUST be called AFTER any `InjectHistory` (replay) completes. A runtime `replayPhase` guard in `EventLog` tags pre-sink entries so a broken caller gets surfaced as `naozhi_eventlog_persist_replay_leak_total > 0` (or panic in DevMode)
- Read path is `merged.Source` = `naozhilog.Source` (primary) + `claudejsonl.Source` (fallback). UUID dedup keeps the local richer entry when both tiers see the same turn
- Rotate threshold 100 MiB; rotate keeps the newest `DefaultKeepRecords` (1000) records, splices via offset-index so it's O(1) in practice
- Orphan `<keyhash>.log` files whose stem doesn't match any known session AND whose mtime is > 30 days get swept on NewRouter startup
- FS detection (Linux `statfs`) runs once and surfaces via `/health.eventlog.{fs_type, fs_supported}`; NFS/overlayfs report `supported=false` so operators see a warning

`/health.eventlog` fields: `writer_alive` (= `last_drain_ms_ago < 5s AND channel_depth < 0.8*cap`), `channel_depth`, `channel_cap`, `last_drain_ms_ago`, `written_total`, `dropped_total`, `fsync_total`, `malformed_total`, `replay_leak_total`, `fs_type`, `fs_supported`.

### Attachment Refcount (`docs/rfc/attachment-refcount.md`)

A companion tier on top of event log persistence. Each image attachment's `.meta` sidecar (`<workspace>/.naozhi/attachments/<date>/<uuid>.meta`) now records `ReferencingKeyHashes []string` (sorted SHA-256 session hashes that have persisted an entry referencing the attachment) and `LastReferencedAt int64` (latest unix ms bump).

The `internal/attachment/tracker` package runs a single-goroutine worker that observes non-replay `EventEntry.ImagePaths` via the event-log sink bridge and coalesces bumps within a 1s window before rewriting `.meta`. On `Router.Remove` the tracker walks the workspace and removes the keyhash from every `.meta`. `OnPersistedEntry` is non-blocking (drops + counter on full channel); `OnSessionRemoved` is synchronous (serialized in the worker) with 5s timeout.

`attachment.GCWithRefs(workspace, uploadTTL, refTTL, now)` replaces the legacy day-directory reaper with per-file double-TTL eligibility:

```
keep iff ( uploaded_at + uploadTTL > now )
    OR  ( len(ReferencingKeyHashes) > 0 AND last_referenced_at + refTTL > now )
```

Defaults: `uploadTTL=7d` (operator-tunable), `refTTL=30d` (via `DefaultRefTTL`). Files predating the refcount RFC (Meta without the new fields) fall back to the legacy single-TTL path so an upgrade doesn't delete lot-sized history on day 0.

`/health.attachment_tracker` exposes `writer_alive` (same formula as eventlog's), `channel_depth`, `channel_cap`, `last_drain_ms`, `pending`, `written_total`, `cleared_total`, `dropped_total`, `meta_error_total`. `/debug/vars` adds `naozhi_attachment_ref_{bump,clear,meta_error,drop}_total` counters.

GC is wired as the sysession `attachment-gc` daemon (`internal/sysession/attachment_gc.go`, design: `docs/rfc/attachment-gc-daemon.md`), which sweeps the union of the router default workspace, every per-chat workspace override, and every bound project path. It ships default `enabled: false` + `dry_run` — reclamation starts only after an operator enables it in config; until then tracker data accumulates without driving deletion.

### Graceful Shutdown

On SIGTERM/SIGINT: `sd_notify STOPPING=1`, cancel the root context (stops connector, node cache loop, project scan loop), then run the ordered teardown in `cmd/naozhi/runshutdown.go`. The order is a hard correctness contract, pinned by `runshutdown_order_test.go`:

1. **sysMgr.Stop** first -- daemon Tick paths call into the router; leaving them running while downstream state tears down would race
2. **scheduler.Stop** (cron) -- in-flight cron jobs still call GetOrCreate/Send on the router
3. **HTTP drain barrier** -- no handler may observe a half-cleaned session map
4. **router.Shutdown** last -- waits for running sessions (30s via shutdownCond), saves the session store, closes all processes concurrently (via stdin close)

Each phase emits a `phase=` timing log line so a hung subsystem is attributable from journalctl alone. A new subsystem MUST be inserted at the correct slot -- the order test breaks otherwise.

## Config

`config.yaml` supports `${ENV_VAR}` expansion. Key sections:

- **server.addr**: Listen address (default `:8080`)
- **cli**: `backend` (`claude`|`kiro`|`codex`), `path`, `model`, `args`. Multi-backend deployments use `cli.backends: [{id, path, model, args}, ...]` so the dashboard picker can choose per-session — see `config.example.yaml` for the commented-out canonical example
- **session**: `max_procs`, `ttl`, `cwd` (working directory), `store_path`, `watchdog.no_output_timeout`, `watchdog.total_timeout`
- **agents**: Map of agent_id -> {model, args}. Each agent spawns with custom system prompt via `--append-system-prompt`
- **agent_commands**: Map of command -> agent_id for routing (e.g., `review: code-reviewer`)
- **platforms**: `feishu` (app credentials, connection_mode), `slack` (bot_token, app_token), `discord` (bot_token), `weixin` (token, base_url)
- **cron**: `store_path`, `max_jobs`, `execution_timeout`
- **projects**: `root` (scan directory), `planner_defaults.model`, `planner_defaults.prompt`
- **workspace**: `id`, `name` (local node identity, defaults to hostname)
- **nodes**: Map of node_id -> {url, token, display_name} (poll remote nodes via HTTP)
- **reverse_nodes**: Map of node_id -> {token, display_name} (accept incoming reverse connections)
- **upstream**: `url` (ws://), `node_id`, `token`, `display_name` (connect to primary as reverse node)
- **transcribe**: `enabled`, `region`, `language` (voice message STT). There is no `provider` key — the AWS Transcribe backend is the only implementation and is selected implicitly
- **log**: `level` (debug/info/warn/error)

Config field `session.workspace` is a deprecated alias for `session.cwd`. Both `nodes` and `workspaces` are accepted (`workspaces` is the preferred name; **`workspaces` takes precedence if both are present** and `nodes` is overwritten with a `slog.Warn` — see `Config.Normalize`, R71-ARCH-L1).

## Concurrency Patterns

- **Router.mu** protects the sessions map. Released during `Spawn()` (may block on ACP handshake) with TOCTOU guard on re-acquire. `shutdownCond` is conditioned on `mu` for Shutdown wait.
- **ManagedSession.sendMu** serializes Send() calls and protects session_id capture.
- **sessionGuard** (`sync.Map`) prevents goroutine accumulation -- one message per session at a time.
- **Hub.mu** protects WebSocket client set and subscriptions. `nodesMu` (shared with Server) protects the nodes map.
- Node cache is a separate `nodeCacheMu` to avoid blocking dashboard API.
- Process Close() is always called outside router lock to prevent deadlock.

## Deployment

Production: CloudFront -> ALB (SG: CloudFront-only) -> EC2 t4g.small :8180 -> systemd. Bedrock auth via IAM role (no AKSK). The EC2 needs access to the `bedrock-runtime` VPC endpoint (check SG on the endpoint).

## Documentation

```
docs/
├── design/                       # 架构与已实现功能设计
│   ├── DESIGN.md                 # 主设计文档 —— 架构层面事实以此为准
│   ├── architecture.html         # 架构可视化
│   └── ...                       # 各子系统设计（multi-node / shim / server-split / i18n / ...）
├── adr/                          # Architecture Decision Records
├── rfc/                          # RFC 工作文档（proposal / 验证报告 / phase 报告）
│   └── README.md                 # 索引 + 状态表（Draft / 已实装 / 已废弃）——先看这里
├── ops/                          # 部署与运维 playbook（deployment / release-gate / pprof / ...）
├── review/                       # 代码评审批次原始记录（batch*-raw.md / cosmetic-backlog.md）
└── guides/                       # 操作手册（weixin-test / shim-testing）
```

待办事项不再放 `docs/TODO.md`，已迁移 GitHub Issues（见 `docs/rfc/todo-to-issues-migration.md`）。
RFC 不是最终规范——其中很多已实装或已废弃，状态以 `docs/rfc/README.md` 的表格为准。
