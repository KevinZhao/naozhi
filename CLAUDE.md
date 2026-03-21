# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
go build ./...                                    # check compilation
CGO_ENABLED=0 go build -o bin/naozhi ./cmd/naozhi/  # build binary
go vet ./...                                      # lint

bin/naozhi --config config.yaml                   # run locally
```

Cross-compile for deployment target (ARM64 Linux):
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/naozhi ./cmd/naozhi/
```

Deploy to remote EC2 via SSM:
```bash
./deploy/deploy.sh deploy   # build + upload to S3 + install on EC2
./deploy/deploy.sh status   # check service status
./deploy/deploy.sh logs     # view recent logs
```

Set `INSTANCE_ID` in `deploy/deploy.sh` before first deploy.

## Architecture

Naozhi is an IM gateway that wraps AI CLI agents (Claude CLI or Kiro) as long-lived subprocesses. Communication uses a pluggable Protocol interface: `ClaudeProtocol` (stream-json NDJSON over stdin/stdout) or `ACPProtocol` (JSON-RPC 2.0 Agent Client Protocol). The entire agent loop (tools, context, reasoning) is delegated to the CLI — Naozhi is just the routing layer.

**Request flow**: IM platform → message handler → async goroutine → session router → CLI stdin → read stdout until turn complete → platform reply API.

**Key constraint**: Feishu requires 200 response within 3s. The webhook handler returns 200 immediately and processes asynchronously via `go handler(context.Background(), msg)`.

### Module Dependency

```
cmd/naozhi/main.go
  → config      (YAML loading, env var expansion, agent config validation)
  → cli         (Protocol interface + spawn/manage CLI processes with watchdog)
  → session     (map session keys to processes, concurrency control, persistence)
  → platform    (Platform interface + feishu/slack/discord implementations)
  → server      (HTTP routing, message handler, agent routing, reply logic)
```

### CLI Process Lifecycle

Each CLI process is long-lived (stdin/stdout stay open across turns). The Wrapper selects a Protocol based on `cli.backend` config:
- `claude` (default): `ClaudeProtocol` — stream-json, session ID from init event
- `kiro`: `ACPProtocol` — JSON-RPC 2.0, session ID from `session/new` response

Protocol.Init() runs after spawn but before readLoop, handling any handshake (no-op for Claude, initialize + session/new for ACP). Session ID is captured during Init or from the first Send.

Process states: `Spawning → Ready ↔ Running → Dead`. Dead processes with a SessionID can be resumed via `--resume` (Claude) or `session/load` (ACP).

**Watchdog**: During Running state, two timers enforce limits:
- `no_output_timeout` (default 2min): Reset on any event; if triggered, kill process
- `total_timeout` (default 5min): Single shot; if triggered, kill process

### Protocol Interface

```go
type Protocol interface {
    Name() string
    BuildArgs(opts SpawnOptions) []string
    Init(rw *JSONRW, resumeID string) (sessionID string, err error)
    WriteMessage(w io.Writer, text string) error
    ReadEvent(line []byte) (ev Event, done bool, err error)
    HandleEvent(w io.Writer, ev Event) (handled bool)
}
```

- `ClaudeProtocol`: stream-json NDJSON, hook filtering, `--setting-sources ""` isolation
- `ACPProtocol`: JSON-RPC 2.0, auto-grants `session/request_permission`, accumulates `agent_message_chunk` text

### Platform Adapter Pattern

Platforms implement `Platform` interface and register their own webhook routes via `RegisterRoutes(mux, handler)`. The platform calls `handler(ctx, msg)` when a message arrives — the server never parses platform-specific formats.

Platforms needing background goroutines implement `RunnablePlatform` with `Start()/Stop()`:

| Platform | Transport | Interface |
|----------|-----------|-----------|
| Feishu   | WebSocket (default) or HTTP webhook | `RunnablePlatform` |
| Slack    | Socket Mode (WebSocket) | `RunnablePlatform` |
| Discord  | WebSocket gateway | `RunnablePlatform` |

### Session Management & Agent Routing

Session key format: `{platform}:{chatType}:{userId}:{agentId}` (e.g., `feishu:direct:alice:code-reviewer`).

Each session is independent:
- Owns one long-lived claude process
- Maintains separate context and session ID
- Uses per-session model/args from agent config

Different commands route to different agents:
- `/review xxx` → `code-reviewer` agent (sonnet, custom system prompt)
- `/research xxx` → `researcher` agent (opus, custom system prompt)
- Plain messages → `general` agent (default)
- `/new` resets general; `/new review` resets code-reviewer

On startup, validate all `agent_commands` entries reference existing agents in config.

### Session Persistence

Sessions are persisted to `~/.naozhi/sessions.json` at startup and shutdown:
- Each entry stores `key` and `session_id`
- On restart, dead sessions are loaded
- Next message to a dead session resumes via `--resume`
- Captures session_id under sendMu to avoid Send() data races

### Graceful Shutdown

On SIGTERM:
1. Stop accepting webhooks
2. Wait for running sessions to complete (timeout 30s)
3. Save session store
4. Close all processes (via stdin close)

## Config

`config.yaml` supports `${ENV_VAR}` expansion. Includes:
- **cli**: Backend (`claude`|`kiro`), path, model, extra args
- **session**: Concurrency (max_procs), idle timeout (ttl), watchdog timers (no_output_timeout, total_timeout), persistence path
- **agents**: Map of agent_id → (model, extra args). Each agent spawns with custom system prompt via `--append-system-prompt`
- **agent_commands**: Map of command → agent_id for routing (e.g., `/review` → `code-reviewer`)
- **platforms**: Feishu (app credentials, connection_mode, encrypt_key), Slack (bot_token, app_token for Socket Mode), Discord (bot_token)

Feishu credentials come from environment or config. v2 signature verification uses SHA-256 hash of `timestamp + nonce + encrypt_key + body`.

## Deployment

Production: CloudFront → ALB (SG: CloudFront-only) → EC2 t4g.small :8180 → systemd. Bedrock auth via IAM role (no AKSK). The EC2 needs access to the `bedrock-runtime` VPC endpoint (check SG on the endpoint).
