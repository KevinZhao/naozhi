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

Deploy: see `deploy/naozhi.service` for systemd unit. Manual deploy via SSM + S3.

## Architecture

Naozhi is an IM gateway that wraps AI CLI agents (Claude CLI or Kiro) as long-lived subprocesses. Communication uses a pluggable Protocol interface: `ClaudeProtocol` (stream-json NDJSON over stdin/stdout) or `ACPProtocol` (JSON-RPC 2.0 Agent Client Protocol). The entire agent loop (tools, context, reasoning) is delegated to the CLI -- Naozhi is just the routing layer.

**Request flow**: IM platform -> message handler -> async goroutine -> session router -> CLI stdin -> read stdout until turn complete -> platform reply API.

**Key constraint**: Feishu requires 200 response within 3s. The webhook handler returns 200 immediately and processes asynchronously via `go handler(context.Background(), msg)`.

### Module Dependency

```
cmd/naozhi/main.go
  -> config       YAML loading, env var expansion, validation
  -> cli          Protocol interface + spawn/manage CLI processes with watchdog
  -> session      Session router, concurrency control, TTL, persistence
  -> dispatch     Message handler + slash commands + per-session queue
  -> platform     Platform interface + feishu/slack/discord/weixin implementations
  -> server       HTTP server, dashboard, WebSocket hub, REST API
  -> cron         Scheduled task execution (robfig/cron)
  -> project      Project discovery, chat binding, planner routing
  -> node         WebSocket protocol types + HTTP/reverse node clients
  -> upstream     Reverse-connect client (NAT traversal; dials primary naozhi)
  -> discovery    Scan ~/.claude/sessions for external Claude CLI processes
  -> shim         Zero-downtime restart: sidecar process that outlives naozhi
  -> transcribe   Voice message transcription (Amazon Transcribe Streaming)
  -> netutil      Client-IP extraction with trusted-proxy handling
  -> osutil       Home/path expansion, process helpers, sd_notify
  -> ratelimit    Per-key LRU rate limiter (used by login/WS/upload)
```

### CLI Process Lifecycle

Each CLI process is long-lived (stdin/stdout stay open across turns). The Wrapper selects a Protocol based on `cli.backend` config:
- `claude` (default): `ClaudeProtocol` -- stream-json, session ID from init event
- `kiro`: `ACPProtocol` -- JSON-RPC 2.0, session ID from `session/new` response

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
    WriteMessage(w io.Writer, text string, images []ImageData) error
    WriteInterrupt(w io.Writer, requestID string) error
    ReadEvent(line string) (ev Event, done bool, err error)
    HandleEvent(w io.Writer, ev Event) (handled bool)
}
```

> Notes:
> - `Init` takes the workspace `cwd` so ACP can pass it in `session/new`;
>   `ClaudeProtocol` ignores the argument because stream-json inherits the
>   shim's `os.Chdir`.
> - `WriteInterrupt` emits a mid-turn interrupt; ACP returns
>   `ErrInterruptUnsupported` so callers fall back to SIGINT.
> - `ReadEvent` takes a single NDJSON line as `string`. A prior proposal
>   (R67-PERF-1) considered migrating to `[]byte` to skip a per-event heap
>   copy on the shim stdout hot path, but the current signature is shared
>   by DESIGN.md, this file, and `internal/cli/protocol.go`; any change
>   must update all three in lockstep.

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
Project planner sessions use: `project:{name}:planner` (exempt from TTL and max_procs).

Each session is independent: owns one long-lived CLI process, maintains separate context and session ID, uses per-session model/args from agent config.

Command routing: `/review xxx` -> `code-reviewer` agent, `/research xxx` -> `researcher` agent, plain messages -> `general` agent (or planner if chat is project-bound). `/new` resets general; `/new review` resets code-reviewer. `/cd <path>` changes working directory for all sessions in a chat. `/project <name>` binds a chat to a project.

**Session guard**: Only one message is processed per session at a time (`sessionGuard` with `sync.Map`). Duplicate messages are rejected; busy sessions reply "please wait" with 3s rate-limiting.

### Multi-Node Architecture

Naozhi supports aggregating sessions from multiple machines into a single dashboard:

- **Primary node** (`nodes` config): Polls remote nodes via HTTP REST every 10s, caches results (`nodeSessions`, `nodeProjects`, `nodeDiscovered`). Never blocks dashboard API on unreachable nodes.
- **Reverse-connect** (`upstream` config): Nodes behind NAT dial into the primary via WebSocket (`/ws-node`). The `connector` package handles reconnection with exponential backoff (1s -> 30s). The `ReverseNodeServer` validates tokens with constant-time comparison.
- **Protocol** (`reverse.ReverseMsg`): JSON over WebSocket -- `register/registered`, `request/response` (fetch_sessions, fetch_projects, send), `subscribe/event` (real-time streaming), `ping/pong`.

### Project Management

When `projects.root` is configured, the `project.Manager` scans subdirectories containing `CLAUDE.md`. Each project stores config in `.naozhi/project.yaml` (planner model, prompt, chat bindings).

Chat binding (`/project <name>`) routes plain messages to a planner session (`project:{name}:planner`) with the project directory as workspace. Agent commands still create per-chat sessions but use the project path. Planner sessions are exempt from TTL eviction and max_procs capacity.

The project list is rescanned every 60s. Orphaned planner sessions for removed projects are cleaned up automatically.

### Dashboard & WebSocket

The dashboard is an embedded single-page HTML (`server/static/dashboard.html`) served at `/dashboard`. Real-time updates use a WebSocket hub (`/ws`) with:

- **Client messages**: `auth`, `subscribe` (with optional `after` timestamp), `unsubscribe`, `send`, `interrupt`, `ping`
- **Server messages**: `auth_ok`, `auth_fail`, `subscribed`, `unsubscribed`, `history`, `event`, `send_ack`, `pong`, `error`
- Remote node events are relayed transparently -- subscribe with `node` field to stream from a remote session.

REST API endpoints: `/api/sessions` (GET/DELETE), `/api/sessions/events`, `/api/sessions/send`, `/api/discovered`, `/api/discovered/preview`, `/api/discovered/takeover`, `/api/projects`, `/api/projects/config` (GET/PUT), `/api/projects/planner/restart`, `/api/transcribe`, `/api/cron` (GET/POST/PATCH/DELETE), `/api/cron/pause`, `/api/cron/resume`, `/api/cron/trigger` (manual run-now), `/api/cron/preview` (schedule validation). WebSocket: `/ws` (dashboard), `/ws-node` (reverse-connect nodes).

### Session Discovery & Takeover

The `discovery` package scans `~/.claude/sessions/*.json` to find external (non-naozhi-managed) Claude CLI processes. It cross-references PIDs, upgrades stale session IDs from JSONL mtimes, and extracts summaries from `sessions-index.json`. The dashboard can "takeover" a discovered process: kill the original PID (verified via `/proc/PID/stat` start time to prevent PID reuse attacks), then `--resume` under naozhi management.

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

GC caller wiring in `cmd/naozhi` is pending operator work — `GCWithRefs` is ready to call from a cron job, but the historic `cmd/naozhi/main.go` never registered an `attachment-gc` job. Until it does, tracker data accumulates but never drives reclamation (attachments only grow). This is a documented follow-up, not a blocker for the refcount MVP.

### Graceful Shutdown

On SIGTERM/SIGINT:
1. Cancel context (stops connector, cleanup loop, node cache loop, project scan loop)
2. Stop cron scheduler
3. Wait for running sessions to complete (timeout 30s via shutdownCond)
4. Save session store
5. Close all processes concurrently (via stdin close)
6. Shutdown WebSocket hub and platform connections

## Config

`config.yaml` supports `${ENV_VAR}` expansion. Key sections:

- **server.addr**: Listen address (default `:8080`)
- **cli**: `backend` (`claude`|`kiro`), `path`, `model`, `args`
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
- **transcribe**: `enabled`, `provider` (`aws`), `region`, `language` (voice message STT)
- **log**: `level` (debug/info/warn/error)

Config field `session.workspace` is a deprecated alias for `session.cwd`. Both `nodes` and `workspaces` are accepted (workspaces is preferred name; nodes takes precedence if both present).

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
├── TODO.md                       # 待办事项（只含 open 项，本地 gitignored）
├── design/                       # 架构与已实现功能设计
│   ├── DESIGN.md                 # 主设计文档（架构、选型、已实现功能设计）
│   ├── architecture.html         # 架构可视化
│   ├── multi-node-design.md      # 多节点聚合设计（已实现）
│   ├── shim-design.md            # Shim 进程设计（已实现）
│   ├── server-split-design.md    # Server 包拆分设计（Phase 1-2 已完成）
│   └── voice-transcription.md    # 语音转写设计（已实现）
├── ops/                          # 部署与运维
│   ├── deployment-strategy.md    # 部署策略设计（部分实现）
│   └── naozhi-deploy-skill.md    # 部署 skill playbook
├── rfc/                          # 未实现的设计提案
│   ├── message-queue.md          # 消息队列策略
│   └── learning-system.md        # 自学习系统
└── guides/                       # 操作手册
    ├── weixin-test.md            # 微信渠道测试
    └── shim-testing.md           # Shim 调试指南
```
