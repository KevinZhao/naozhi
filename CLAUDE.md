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

Naozhi is an IM gateway that wraps the `claude` CLI as a long-lived subprocess, communicating via the stream-json NDJSON protocol over stdin/stdout. The entire agent loop (tools, context, reasoning) is delegated to the CLI — Naozhi is just the routing layer.

**Request flow**: Feishu webhook → HTTP handler → async goroutine → session router → claude CLI stdin → read stdout until `type=result` → Feishu reply API.

**Key constraint**: Feishu requires 200 response within 3s. The webhook handler returns 200 immediately and processes asynchronously via `go handler(context.Background(), msg)`.

### Module Dependency

```
cmd/naozhi/main.go
  → config      (YAML loading, env var expansion)
  → cli         (spawn and manage claude CLI processes)
  → session     (map session keys to processes, concurrency control)
  → platform    (Platform interface + feishu implementation)
  → server      (HTTP routing, message handler, reply logic)
```

### CLI Process Lifecycle

Each claude CLI process is long-lived (stdin/stdout stay open across turns). The `system/init` event only arrives **after** the first message is written to stdin, not at process start. Session ID is captured from the init event inside `Process.Send()`.

Process states: `Spawning → Ready ↔ Running → Dead`. Dead processes with a SessionID can be resumed via `--resume`.

### stream-json Protocol

- Input (stdin): `{"type":"user","message":{"role":"user","content":"..."}}\n`
- Output (stdout): NDJSON lines with `type` field: `system/init`, `assistant`, `result`
- `type=result` signals turn completion
- Hook events (`hook_started`, `hook_response`) are filtered out in `readLoop`
- Must use `--setting-sources ""` to avoid Stop hook death loops from plugins
- Must use `--verbose` or stream-json output fails

### Platform Adapter Pattern

Platforms implement `Platform` interface and register their own webhook routes via `RegisterRoutes(mux, handler)`. The platform calls `handler(ctx, msg)` when a message arrives — the server never parses platform-specific formats.

Platforms needing background goroutines (Telegram polling, Discord WebSocket) implement `RunnablePlatform` with `Start()/Stop()`.

## Config

`config.yaml` supports `${ENV_VAR}` expansion. Feishu credentials come from `EnvironmentFile` in systemd (`~/.naozhi/env`).

## Deployment

Production: CloudFront → ALB (SG: CloudFront-only) → EC2 t4g.small :8180 → systemd. Bedrock auth via IAM role (no AKSK). The EC2 needs access to the `bedrock-runtime` VPC endpoint (check SG on the endpoint).
