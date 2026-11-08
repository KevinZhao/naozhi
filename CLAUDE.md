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
- **Persistence / history**: `eventlog`, `history` (claudejsonl/kirojsonl/codexjsonl/naozhilog/merged), `attachment`, `runlog`, `claudefs` (Claude CLI on-disk layout), `discovery`, `subagent`, `planner/journal` + `planner/workspace` (Planner state root: event journal, workspace registry; `docs/rfc/planner.md` §3.7), `planner/activity` (Activity records derived from tool events, outputs index behind `/api/sessions/outputs`; §3.4), `planner/agent` (the global Planner session `planner:main`: launch spec, revival, daily shift change with handoff summary, message snapshot and receipt behind `/api/planner/*`), `planner/brief` (the deterministic weekday morning brief 早报: journaled as kind=brief, pushed once through `notify`, listed by `/api/planner/briefs`; also lists what the Planner held for it, and the scout's calendar, replies waiting and Asana tasks due; what custody did, the cron jobs whose result changed, and the open notices, which it closes once sent), `planner/promise` (the promises `planner/promises.json`: due reminders through `notify` past the daily cap; a pure reminder is done once it reminded, others are closed by the Planner's status block or the user, and can be reopened), `planner/task` (the Planner page's "任务": the dispatched `task:` sessions, the observed sessions the user opened (你开的, §15.12 ⑥, archived once done or idle 24 hours) and the promises in one list — 进行中 · 待提醒 · 你开的 · 已完成, 等你 / 失败 from health, done ones archived after 24 hours; `dashboard/ext/tasks` serves `/api/tasks`; §15.4), `planner/reap` (收起, §5.2 / D95, only with `planner.enabled`: a `task:` session marked done within the last 24 hours is put away once its turn ended normally, no AskUserQuestion is open, nothing runs in the background and the user is not at it (custody's `Nearby`): `Router.Archive` closes it and moves it to the history keeping its event log, then each linked git worktree its transcript's `cwd` went into is removed (`git worktree remove`, no force) once clean, pushed and its PR merged or closed, else kept and the user told; journaled as kind=reap, shown on the task, undone by reopening it (`--resume` under the same key, worktrees checked out again)), `planner/action` (the Planner's ```` ```naozhi-action ```` blocks parsed from `planner:main` turn results: dispatch → a `task:` session started at once (D88), stoppable from 任务; promise → `planner/promise`; ask → a question whose answer goes back to the Planner; status → a task's state through `planner/task`; notify → a notice pushed once a day, or held for the next brief; relay → the user's decision passed on to a session in the gateway's `<naozhi-relay>` envelope with the user's own words, only in a turn the user started, as does an ask naming a `task` (D98, §15.11); the blocks not run come back to the Planner in one receipt), `planner/sidecar` (per-session sidecar files `planner/sessions/<sha256(key)>.json`: purpose, lineage and the task state the Planner or the user set, kept 30 days after the session retires; feeds `index.json` and the sidebar's "Planner 派出" group), `planner/digest` (§15.12 ②, D100, only with `planner.enabled`: one markdown digest per `task:` session and per observed session the user opened, `planner/digest/<persist.KeyHash(key)>.md` beside a `<hash>.json` keeping its goal and latest report; written without a model as turns end and health moves, at most once a minute per session; `index.json` rows carry its path and the report's first sentence, the task card opens it through `/api/sessions/digest`), `planner/note` (the session view's side notes: health and "等你" notes read back from the journal plus the dispatch and reports to the Planner, behind `/api/sessions/notes` and the `session_note` frame; §15.3 ③), `planner/custody` (托管, §5.4 / D93 / D94, only with `planner.enabled`: a per-session switch in the sidecar, on for `task:` sessions (off once their task is done) and, while it runs, sessions with a workflow running; `Deliver` is the one way a message goes out on the user's behalf, the text alone, its transcript entry marked `source: custody` with `custody_reason` / `custody_n`, stopped per lineage after 3 in a row without progress or 8 in all, the user's message to a stopped one starting it afresh; the switch only gates replies for the user (held back while the user views the session or sent it a message in the last 5 minutes): a failure of a retryable class gets "继续" after 1/2/4/8 minutes on every local session but cron / scratch (at once for a network error after a wake), an expired credential's once the Midway cookie is renewed; a normal turn end goes to the Planner as a receipt (`planner/action`); paused by the inbox's one pause (暂停推送与托管); beside it the 观察 switch (§15.12 ⑥ / D104, sidecar `watch`, on by default for this node's sessions the user opened, not `task:` / cron / scratch): it feeds the digests and 你开的, and a turn that ended with the user away is a result (`result_at`); `dashboard/ext/custody` serves `/api/custody`), `planner/scout` (the guarding patrol 巡检, §15.6: weekday 08:00–20:00 every two hours plus 08:15, a one-shot `scout:<run>` session with only the mail / Slack / Asana MCP read tools under dontAsk; strict JSON output, cursors and (source, ID, kind) dedup under `planner/scout/`; only new findings wake the Planner as a `<naozhi-scout>` block; 15 minutes before a meeting with others in the cached calendar, a brief session's points wake it as `<naozhi-scout kind="meeting">` (once per meeting, `briefed.json`), and that turn (and the receipt of what it was refused) runs only promise / ask blocks, by the turn sources the server keeps in `planner/turns.json`; `POST /api/scout/run` runs one now).
- **Domain helpers**: `selfupdate`, `transcribe`, `imauth`, `costledger`, `budget`, `webhook`, `runtelemetry`, `naozhisettings`, `ccassets`, `ccmodels`, `uiprefs`, `replyfmt`, `usermsg`, `gitinfo`, `monitor` (`beat`: critical-loop heartbeats behind `/health` status; `wake`: sleep/wake detection; `health`: session ledger and health state machine, writes the Planner journal and `planner/index.json`, sources wired in `wireup/health_sources.go`), `notify` (the "等你" inbox `planner/inbox.json` opened from health transitions, heartbeats and disk, plus "后台任务跑完了" and the upgrade a start made; failures close themselves once recovered; push to the dashboard and macOS notifications with a daily cap, quiet hours and the push pause; `dashboard/ext/inbox` serves `/api/inbox`).
- **Leaves**: `osutil`, `textutil`, `netutil`, `ratelimit`, `limits`, `sessionkey`, `tuningspec`, `backendid`, `cliinfo`, `apierr`, `ctxutil`, `midway` (the Midway session cookie's expiry, read for `notify`'s warning and `planner/custody`'s retry), `spawndiag`, `envpolicy`, `datadir`, `imageorient`, `testhelper`.

### CLI Process Lifecycle

Each CLI process is long-lived (stdin/stdout stay open across turns). The Wrapper selects a Protocol via the `backend.Profile` registry (`internal/cli/backend/profile.go`, canonical list of registered backends) keyed by `cli.backend` config:
- `claude` (default): `ClaudeProtocol` -- stream-json, session ID from init event
- `kiro`: `ACPProtocol` -- JSON-RPC 2.0 Agent Client Protocol, session ID from `session/new` response
- `codex`: `CodexProtocol` -- codex app-server JSON-RPC 2.0 over stdio (see `docs/rfc/codex-backend.md`)

Multi-backend deployments use `cli.backends: [{id, path, model, args, effort}, ...]` so dashboard / IM / cron / reverse-node can pick backend per session (`docs/rfc/multi-backend.md`). `effort` is the kiro thinking-effort tier, forwarded as `acp --effort`. ACP gotchas:
- ACP `session/cancel` is a **notification** (no id), not a request
- ACP RPC ID can be **string UUID** (kiro `permission_request`), not always int — use `json.RawMessage`
- ACP permission `optionId` is `allow_once / allow_always / reject_once` (underscore, not hyphen) — read from request `options[].optionId`, do not hardcode
- Kiro persists session state at `~/.kiro/sessions/cli/<sid>.{json,jsonl}` with stale-PID lock auto-recovery

Protocol.Init() runs after spawn but before readLoop, handling any handshake (no-op for Claude, initialize + session/new for ACP). Session ID is captured during Init or from the first Send.

Process states: `Spawning -> Ready <-> Running -> Dead`. Dead processes with a SessionID can be resumed via `--resume` (Claude) or `session/load` (ACP).

**Watchdog**: During Running state, two timers enforce limits:
- `no_output_timeout` (default 15min): Reset on any event; if triggered, kill process. A running tool keeps it fed through the Claude CLI's `tool_progress` heartbeat (every 30s), so it only catches silent model generation or a hung CLI
- `total_timeout` (default 2h): Single shot; if triggered, kill process. A `task:` session gets at least 6h (`cliinfo.DefaultTaskTotalTimeout`), taken per key at spawn, shim reconnect and the stuck sweep (2× it)
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
- `planner:main` -- the global Planner (exempt; dashboard sends refuse the namespace, messages go through `/api/planner/send`)
- `task:<random ID>` -- a session the Planner dispatched (counts against max_procs; only a Planner dispatch creates one, sends then reach it like any session)
- `scout:<run ID>` -- a scout run's one-shot session (exempt; never persisted or listed, reset when the run ends)
- `dashboard:pj:<workspace-hash>:<agent>` -- project-stable dashboard keys (chatType `pj`, deliberately not `project`)

Each session owns one long-lived CLI process with its own context, session ID and per-agent model/args.

Command routing: `/review xxx` -> `code-reviewer` agent, `/research xxx` -> `researcher` agent, plain messages -> `general` agent (or planner if chat is project-bound). `/new` resets general; `/new review` resets code-reviewer. `/cd <path>` changes working directory for all sessions in a chat. `/project <name>` binds a chat to a project.

**One turn per session**: `turn.Orchestrator` runs one owner loop per session key; a message for a busy key is queued (and merged into the next turn) rather than rejected. Duplicate deliveries are dropped by `platform.Dedup`.

### Multi-Node

The primary polls `workspaces` nodes over HTTP (`node.CacheManager`, never blocking the dashboard on an unreachable node); nodes behind NAT dial in via `upstream` to `/ws-node` (`node.ReverseServer`, protocol in `internal/node/protocol.go`). Each node runs its own Planner over its own sessions (D96); the Planner page switches to a node's, then calling `/api/remote/{node}/…`, forwarded by the `api` RPC to the node's own Planner routes (`node.APIRoutes`, `docs/rfc/remote-planner.md`), and a node running a Planner stays out of the primary's health ledger. Design: `docs/design/multi-node-design.md`, `docs/rfc/reverse-protocol.md`.

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
- Each entry stores `key`, `session_id`, `workspace`, `total_cost`
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
4. **router.Shutdown** last -- waits up to 2s for running turns a shim does not hold (a shim-held turn finishes on the reattached CLI), saves the session store, detaches shim-held processes and closes the rest concurrently

Each phase emits a `phase=` timing log line. A new subsystem MUST be inserted at the correct slot.

## Config

`config.yaml` supports `${ENV_VAR}` expansion; `config.example.yaml` is the commented reference. Key sections:

- **server.addr**: Listen address (default `:8080`); `server.dashboard_token` guards the dashboard
- **cli**: `backend` (`claude`|`kiro`|`codex`), `path`, `model`, `args`, `effort`, `mcp_config` (→ `--mcp-config`; needed for MCP servers when `naozhi_settings.enabled` is on, see `docs/rfc/cli-mcp-config.md`), `permission_mode` (Claude only: `bypass` (default) or `auto`), `backends` for multi-backend. Effort precedence: `cli.effort < cli.backends[].effort < agents[].effort`
- **session**: `max_procs`, `ttl`, `cwd`, `store_path`, `watchdog.no_output_timeout`, `watchdog.total_timeout`
- **agents**: Map of agent_id -> {model, args, backend, access_profile, effort, system_prompt}. `system_prompt` is passed as `--append-system-prompt` (Claude only); the flag itself is denylisted in `args`
- **agent_commands**: Map of command -> agent_id (e.g., `review: code-reviewer`)
- **platforms**: `feishu`, `slack`, `discord`, `weixin`
- **planner**: `enabled` (default false) runs the global Planner session `planner:main` (`docs/rfc/planner.md` §15.1); `POST /api/planner/state` stops it at runtime
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

`docs/design/` (`DESIGN.md` is the architecture source of truth), `docs/rfc/` (working RFCs; status table in `docs/rfc/README.md`), `docs/ops/` (release-gate / doctor / codesign playbooks). TODOs go to GitHub Issues.
