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

Cross-compile for ARM64 Linux: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/naozhi ./cmd/naozhi/`.

## Architecture

Naozhi is an IM gateway that wraps AI CLI agents (Claude CLI, Kiro, or Codex) as long-lived subprocesses. Communication uses a pluggable Protocol interface: `ClaudeProtocol` (stream-json NDJSON over stdin/stdout), `ACPProtocol` (JSON-RPC 2.0 Agent Client Protocol), or `CodexProtocol` (codex app-server JSON-RPC 2.0 over stdio). The entire agent loop (tools, context, reasoning) is delegated to the CLI -- Naozhi is just the routing layer.

**Request flow**: IM platform -> message handler -> async goroutine -> session router -> CLI stdin -> read stdout until turn complete -> platform reply API.

**Key constraint**: IM webhooks (Slack HTTP mode, WeChat) must answer fast, so handlers return 200 immediately and process asynchronously via `go handler(context.Background(), msg)`.

### Module Layers

- **Composition root**: `cmd/naozhi` + `wireup` (backend registration, config validation, scheduler wiring); `config` loads YAML and expands `${ENV_VAR}`.
- **Core path (IM -> CLI)**: `platform` (feishu/slack/discord/weixin) -> `dispatch` (IM handling, slash commands; `agentroute` parses `/command agentId`) -> `turn` (per-session owner loop and queue, shared by IM and dashboard sends) -> `session` (router, TTL, persistence; `routerrelay` breaks the router↔hub cycle) -> `cli` (Protocol, process spawn, watchdog) -> `shim` (sidecar that outlives naozhi restarts).
- **HTTP / dashboard**: `server` (routes, WebSocket hub, REST), `dashboard/*` handler packages, `wsproto` (browser WS frames), `contractjs` (generates `static/contract.js`), `assets`.
- **Scheduling / projects**: `cron` (+ `runstore` run history), `sysession` (background daemons), `project` / `projectapi` (discovery, chat binding, planner).
- **Multi-node**: `node` (protocol, HTTP/reverse clients), `upstream` (reverse-connect dialer).
- **Persistence / history**: `eventlog`, `history` (claudejsonl/kirojsonl/codexjsonl/naozhilog/merged), `attachment`, `runlog`, `claudefs` (Claude CLI on-disk layout), `discovery`, `subagent`.
- **Domain helpers**: `selfupdate`, `transcribe`, `imauth`, `costledger`, `budget`, `webhook`, `runtelemetry`, `naozhisettings`, `ccassets`, `ccmodels`, `uiprefs`, `replyfmt`, `usermsg`, `gitinfo`, `metrics`.
- **Leaves**: `osutil`, `textutil`, `netutil`, `ratelimit`, `limits`, `sessionkey`, `tuningspec`, `backendid`, `cliinfo`, `apierr`, `ctxutil`, `leakguard`, `spawndiag`, `envpolicy`, `datadir`, `imageorient`, `testhelper`.

### CLI Process Lifecycle

Each CLI process is long-lived (stdin/stdout stay open across turns). The Wrapper selects a Protocol via the `backend.Profile` registry (`internal/cli/backend/profile.go`, canonical list of registered backends) keyed by `cli.backend` config:
- `claude` (default): `ClaudeProtocol` -- stream-json, session ID from init event
- `kiro`: `ACPProtocol` -- JSON-RPC 2.0 Agent Client Protocol, session ID from `session/new` response
- `codex`: `CodexProtocol` -- codex app-server JSON-RPC 2.0 over stdio (see `docs/rfc/codex-backend.md`)

Multi-backend deployments use `cli.backends: [{id, path, model, args, effort}, ...]` so dashboard / IM / cron / reverse-node can pick backend per session (`docs/rfc/multi-backend.md`). `effort` is the kiro thinking-effort tier, forwarded as `acp --effort`. ACP gotchas (`docs/rfc/multi-backend-validation.md`):
- ACP `session/cancel` is a **notification** (no id), not a request
- ACP RPC ID can be **string UUID** (kiro `permission_request`), not always int — use `json.RawMessage`
- ACP permission `optionId` is `allow_once / allow_always / reject_once` (underscore, not hyphen) — read from request `options[].optionId`, do not hardcode
- Kiro persists session state at `~/.kiro/sessions/cli/<sid>.{json,jsonl}` with stale-PID lock auto-recovery

Protocol.Init() runs after spawn but before readLoop, handling any handshake (no-op for Claude, initialize + session/new for ACP). Session ID is captured during Init or from the first Send.

Process states: `Spawning -> Ready <-> Running -> Dead`. Dead processes with a SessionID can be resumed via `--resume` (Claude) or `session/load` (ACP).

**Watchdog**: During Running state, two timers enforce limits:
- `no_output_timeout` (default 15min): Reset on any event; if triggered, kill process. A running tool keeps it fed through the Claude CLI's `tool_progress` heartbeat (every 30s), so it only catches silent model generation or a hung CLI
- `total_timeout` (default 2h): Single shot; if triggered, kill process
- Both defaults live in `cliinfo` (`DefaultNoOutputTimeout` / `DefaultTotalTimeout`); `cli` and `config` alias them

The shim argv is compared on restart; a field missing from that comparison reads as argv drift and kills live shims (`spawn_argv_ast_test.go` guards it).

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

Turn-end is detected from a `result`/turn-end Event, never from `done`. ACP returns `ErrInterruptUnsupported` from `WriteInterrupt`, so callers fall back to SIGINT.

### Platform Adapter Pattern

Platforms implement `Platform` interface and register their own webhook routes via `RegisterRoutes(mux, handler)`. The platform calls `handler(ctx, msg)` when a message arrives -- the server never parses platform-specific formats.

Platforms needing background goroutines implement `RunnablePlatform` with `Start()/Stop()`. Platforms that cannot send interim messages (e.g. WeChat iLink's single-use reply tokens) implement `SupportsInterimMessages() bool` returning false. Platforms that observe their own connection implement `ConnStateReporter` (`internal/platform/connstate.go`); `/health` serves it in `platforms` / `platform_conn`.

| Platform | Transport | Interface |
|----------|-----------|-----------|
| Feishu   | WebSocket long connection | `RunnablePlatform` |
| Slack    | Socket Mode (WebSocket) | `RunnablePlatform` |
| Discord  | WebSocket gateway | `RunnablePlatform` |
| WeChat   | iLink Bot HTTP long-poll | `RunnablePlatform` |

### Session Management & Agent Routing

Session key format: `{platform}:{chatType}:{chatID}:{agentId}` (e.g., `feishu:direct:alice:code-reviewer`). In a group chat `session.group_scope` (default `thread`) can narrow `chatID` to `{chatID}#t{threadID}` (Slack thread / Feishu topic) or `{chatID}#u{userID}` (`user`). The chat itself still owns the `/cd` workspace, project binding and cron jobs; only the session (and `/new`, `/stop`, `/urgent`) is per thread or member.
Other key namespaces (canonical home: `internal/sessionkey`, wire-stable constants):
- `project:{name}:planner` -- project planner sessions (exempt from TTL and max_procs)
- `cron:<jobID>` / `sys:<daemonID>` / `scratch:<sessionID>` -- cron jobs, sysession daemons, scratch sessions
- `dashboard:pj:<workspace-hash>:<agent>` -- project-stable dashboard keys (chatType `pj`, deliberately not `project`)

Each session owns one long-lived CLI process with its own context, session ID and per-agent model/args.

Command routing: `/review xxx` -> `code-reviewer` agent, `/research xxx` -> `researcher` agent, plain messages -> `general` agent (or planner if chat is project-bound). `/new` resets general; `/new review` resets code-reviewer. `/cd <path>` changes working directory for all sessions in a chat. `/project <name>` binds a chat to a project.

**One turn per session**: `turn.Orchestrator` runs one owner loop per session key; a message for a busy key is queued (and merged into the next turn) rather than rejected. Duplicate deliveries are dropped by `platform.Dedup`.

### Multi-Node

The primary polls `workspaces` nodes over HTTP (`node.CacheManager`, never blocking the dashboard on an unreachable node); nodes behind NAT dial in via `upstream` to `/ws-node` (`node.ReverseServer`, protocol in `internal/node/protocol.go`). Design: `docs/design/multi-node-design.md`, `docs/rfc/reverse-protocol.md`.

### Project Management

When `projects.root` is configured, `project.Manager` scans every non-hidden subdirectory (no marker file needed; `projects.exclude` globs skip names) every 60s. Per-project config lives in `.naozhi/project.yaml`, created only by an explicit bind/favorite/config edit; Scan never writes into a project. `/project <name>` routes plain messages to the `project:{name}:planner` session with the project directory as workspace.

### Dashboard & WebSocket

Embedded PWA at `/dashboard`: `internal/server/static/` (`dashboard.html`, ES modules, `vendor/` KaTeX + mermaid served from `/static/vendor/`). Everything under `static/` is embedded and served from `/static/` by one auth-gated route (`static_module_inventory_test.go` checks every module answers 401/200); no inline `onclick` handlers (CSP).
`/ws` frame types live in `internal/wsproto` and `contract.js`; the dashboard registers handlers via `wsm.on(NZ_CONTRACT.WS.<type>, ...)` (`ws_manager.js`). Remote node events are relayed with a `node` field.
REST routes: `internal/server/routes.go` is the authoritative list (grep `HandleFunc`). Health: `/health`, `/livez`, `/readyz`.

### Session Discovery & Takeover

The `discovery` package scans `~/.claude/projects/<slug>/<sessionId>.jsonl` (Claude CLI's artifacts, read but not owned by naozhi) to find external CLI processes. Takeover kills the original PID (start time verified to prevent PID reuse), then `--resume`s it under naozhi.

### Session Persistence

Sessions are persisted to `~/.naozhi/sessions.json` at shutdown:
- Each entry stores `key`, `session_id`, `workspace`, `total_cost`; a sibling `sessions.meta.json` sidecar records `{version, written_at, generator}`
- On restart, dead sessions are loaded and history is async-loaded (naozhi event log first, Claude JSONL fallback)
- Next message to a dead session resumes via `--resume`
- Captures session_id under sendMu to avoid Send() data races

**Event log** (`docs/rfc/event-log-persistence.md`): every `cli.EventEntry` is appended to `~/.naozhi/events/<keyhash>.log` + `.idx` (`keyhash = sha256(key)[:16]`), keeping fields Claude's JSONL cannot recover (images, attachment paths, AskQuestion cards). Write order `log.Write → log.Sync → idx.Write → idx.Sync`; `Recover()` truncates to the idx-backed edge. `SetPersistSink` must be called after any `InjectHistory` replay. Reads go through `merged.Source` (naozhilog primary, claudejsonl fallback, UUID dedup).

**Attachments** (`docs/rfc/attachment-refcount.md`, `docs/rfc/attachment-gc-daemon.md`): each `.meta` sidecar records which session hashes reference it; `attachment.GCWithRefs` keeps a file while either the upload TTL or the reference TTL holds. GC runs as the sysession `attachment-gc` daemon, default off + dry-run.

### Graceful Shutdown

On SIGTERM/SIGINT: `sd_notify STOPPING=1`, cancel the root context (stops connector, node cache loop, project scan loop), then run the ordered teardown in `cmd/naozhi/runshutdown.go`. The order is a hard correctness contract, pinned by `runshutdown_order_test.go`:

1. **sysMgr.Stop** first -- daemon Tick paths call into the router; leaving them running while downstream state tears down would race
2. **scheduler.Stop** (cron) -- in-flight cron jobs still call GetOrCreate/Send on the router
3. **HTTP drain barrier** -- no handler may observe a half-cleaned session map
4. **router.Shutdown** last -- waits for running sessions (30s via shutdownCond), saves the session store, closes all processes concurrently (via stdin close)

Each phase emits a `phase=` timing log line. A new subsystem MUST be inserted at the correct slot.

## Config

`config.yaml` supports `${ENV_VAR}` expansion; `config.example.yaml` is the commented reference. Key sections:

- **server.addr**: Listen address (default `:8080`); `server.dashboard_token` guards the dashboard
- **cli**: `backend` (`claude`|`kiro`|`codex`), `path`, `model`, `args`, `effort`, `mcp_config` (→ `--mcp-config`; needed for MCP servers when `naozhi_settings.enabled` is on, see `docs/rfc/cli-mcp-config.md`), `permission_mode` (Claude only: `bypass` (default) or `auto`), `backends` for multi-backend. Effort precedence: `cli.effort < cli.backends[].effort < agents[].effort`
- **session**: `max_procs`, `ttl`, `cwd`, `store_path`, `watchdog.no_output_timeout`, `watchdog.total_timeout`
- **agents**: Map of agent_id -> {model, args, backend, access_profile, effort, system_prompt}. `system_prompt` is passed as `--append-system-prompt` (Claude only); the flag itself is denylisted in `args`
- **agent_commands**: Map of command -> agent_id (e.g., `review: code-reviewer`)
- **platforms**: `feishu`, `slack`, `discord`, `weixin`
- **cron**, **projects**, **sysession**, **transcribe**, **update**, **im_access**, **log**
- **workspace** / **workspaces** / **reverse_nodes** / **upstream**: local node identity, polled nodes, accepted reverse nodes, dial-out to a primary

Config describes schema v2 only; there is no migration chain. v1 keys (`nodes`, `session.workspace`, `session.auto_chain`) are not read and are reported as `config-deprecated` diags (`internal/config/unknown_keys.go`); so is the removed `cron.sandbox` block. A `schema_version` newer than the binary is rejected.

## Concurrency Patterns

- **The session table** (`internal/session/sessiontable`) owns its lock; the only way in is a transaction (`r.ss.View` / `r.ss.Update`). Slow work inside a transaction runs in `tx.Unlocked`; a spawn reserves in the caller's transaction (`reserveSpawn`) and finishes outside it (`completeSpawn`).
- **ManagedSession.sendMu** serializes Send() calls and protects session_id capture.
- **turn's queue** gives each session key one owner loop, so goroutines do not pile up per session.
- **Hub.mu** protects WebSocket client set and subscriptions; node cache uses a separate `nodeCacheMu` to avoid blocking dashboard API.
- Process Close() is always called outside router lock to prevent deadlock.

## Logging

On a turn's path (dispatch, turn, session, cli) log with `slog.*Context(ctx, …)`: `ctxutil.Handler` adds the ctx's `trace_id` / `run_id` / `session_key`. See `docs/ops/log-correlation.md`.

## Code Comments

Comments say *why* (the invariant, the non-obvious choice, what a caller must not do); history belongs in git and issues.

## Deployment

macOS: a launchd job (label read from `XPC_SERVICE_NAME`); `naozhi upgrade` keeps the self-signed codesign identity (`docs/ops/macos-codesign.md`). Linux: `sudo naozhi install` writes the systemd unit (`cmd/naozhi/service_systemd.go::generateSystemdUnit`; `deploy/naozhi.service` is a manual reference kept in sync by `TestGenerateSystemdUnit_MatchesDeployTemplate`).

## Documentation

`docs/design/` (`DESIGN.md` is the architecture source of truth), `docs/rfc/` (working RFCs; status table in `docs/rfc/README.md`), `docs/ops/` (deployment / release / pprof playbooks), `docs/guides/`. TODOs go to GitHub Issues.
